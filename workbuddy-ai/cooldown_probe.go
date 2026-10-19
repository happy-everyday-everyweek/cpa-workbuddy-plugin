// cooldown_probe.go 在账号冷却到期前主动探活，避免「冷却结束 → 业务请求被路由到
// 它 → 再次失败」这条浪费用户等待时间的链路。
//
// 设计要点（与实施总览第 5 节的裁定一致）：
//  1. 触发点是 cooldownUntil - 5s，不是到期后立即探测——后者等于把首次探测成本
//     推给业务请求；
//  2. 只要进入冷却就探测，不设 count >= 3 门槛；
//  3. 必须用当初失败的模型（state.lastModel）回放，任意模型成功不算恢复；
//  4. 只探测账号级冷却，账号×模型级冷却不探测——模型级失败无法通过重试自愈；
//  5. 每个冷却周期最多探测一次，失败后跟随新的 cooldownUntil 排期，不会自激。
//
// 与 active_ping.go 的边界（本文件最重要的约束）：
//
//	doActivePing 在失败时会写 test_failed 标签，而该标签是硬排除、永不自动过期。
//	冷却前探活绝不能走那条路径——探活失败只应让冷却继续沿阶梯递增。因此本模块
//	独立实现，只调用 sendActivePingWorkbuddy（纯发请求）并自行处理结果，
//	任何分支都不调用 persistTestFailedToggle(..., true)。
//
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增冷却前主动探活。
package main

import (
	"log"
	"strings"
	"sync"
	"time"
)

const (
	// cooldownProbeLeadTime 是探测相对冷却到期的提前量。用户裁定为 5 秒：
	// 最短冷却 60s 也留有 55s 的准备窗口。
	cooldownProbeLeadTime = 5 * time.Second

	// cooldownProbeInterval 是探活调度粒度。必须显著小于最短冷却（60s），
	// 否则首档冷却会整轮错过探测窗口。
	cooldownProbeInterval = 5 * time.Second
)

var (
	// probeNowFn 与 probePingFn 是测试注入点，生产路径使用真实实现。
	probeNowFn  = time.Now
	probePingFn = sendActivePingWorkbuddy

	// probedUntil 记录每个账号本冷却周期已探测到的时间点，键为账号标识。
	// 用 cooldownUntil 作为周期标识：冷却被阶梯延长后是新的周期，允许再探一次。
	probedUntilMu sync.Mutex
	probedUntil   = make(map[string]time.Time)
)

// cooldownProbeCandidate 描述一个待探测的账号。
type cooldownProbeCandidate struct {
	authID        string
	cooldownUntil time.Time
	model         string
}

// shouldProbeCandidate 判定某条冷却状态当前是否该探测。
// 纯函数，便于单测覆盖边界而不触碰全局状态。
// [参数] authID：账号标识；cooldownUntil：账号级冷却截止时间；
// model：最近失败模型；probedAt：本周期已探测的时间点（零值表示未探测）。
// [返回] true 表示此刻应发起探测。
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增。
func shouldProbeCandidate(authID string, cooldownUntil time.Time, model string, probedAt, now time.Time) bool {
	// 1. 没有冷却或没有模型就无法探测：探活必须回放当初失败的模型。
	if strings.TrimSpace(authID) == "" || cooldownUntil.IsZero() || strings.TrimSpace(model) == "" {
		return false
	}
	// 2. 冷却已过期：交给正常路由，不再补探测（探测慢不该延长冷却）。
	if !now.Before(cooldownUntil) {
		return false
	}
	// 3. 尚未进入提前量窗口。
	if now.Before(cooldownUntil.Add(-cooldownProbeLeadTime)) {
		return false
	}
	// 4. 本周期已探测过则跳过。probedAt 记录的是「已探测的那个冷却截止时间」，
	//    与当前 cooldownUntil 相等即同一周期；冷却被阶梯延长后两者不等，允许再探。
	if probedAt.Equal(cooldownUntil) {
		return false
	}
	return true
}

// cooldownProbeTargets 从当前冷却状态中挑出该探测的账号。
// [参数] now：当前时间。
// [返回] 待探测候选列表；无候选时返回 nil。
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增。
func cooldownProbeTargets(now time.Time) []cooldownProbeCandidate {
	failoverMu.Lock()
	type snapshot struct {
		authID string
		until  time.Time
		model  string
	}
	var snaps []snapshot
	for id, st := range failoverStates {
		if st == nil || !now.Before(st.cooldownUntil) {
			continue
		}
		snaps = append(snaps, snapshot{authID: id, until: st.cooldownUntil, model: st.lastModel})
	}
	failoverMu.Unlock()

	probedUntilMu.Lock()
	defer probedUntilMu.Unlock()
	var out []cooldownProbeCandidate
	for _, s := range snaps {
		if !shouldProbeCandidate(s.authID, s.until, s.model, probedUntil[s.authID], now) {
			continue
		}
		out = append(out, cooldownProbeCandidate{
			authID:        s.authID,
			cooldownUntil: s.until,
			model:         s.model,
		})
	}
	return out
}

// currentCooldownUntil 读取账号当前的冷却截止时间，读不到时回退到给定值。
// 探活失败会改写 cooldownUntil，标记周期必须以最新值为准。
// [参数] authID：账号标识；fallback：读取失败时的回退值。
// [返回] 当前冷却截止时间。
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增，修复延长后被误判为已探测。
func currentCooldownUntil(authID string, fallback time.Time) time.Time {
	failoverMu.Lock()
	defer failoverMu.Unlock()
	if st := failoverStates[authID]; st != nil && !st.cooldownUntil.IsZero() {
		return st.cooldownUntil
	}
	return fallback
}

// markCooldownProbed 记录某账号已探测过某个冷却周期。
// [参数] authID：账号标识；cooldownUntil：本次冷却的截止时间，作为周期标识——
// 冷却被阶梯延长后会得到新的截止时间，届时允许再探一次。
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增。
func markCooldownProbed(authID string, cooldownUntil time.Time) {
	probedUntilMu.Lock()
	probedUntil[authID] = cooldownUntil
	probedUntilMu.Unlock()
}

// clearCooldownProbeState 清空探测记录。测试辅助与账号删除时调用。
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增。
func clearCooldownProbeState() {
	probedUntilMu.Lock()
	probedUntil = make(map[string]time.Time)
	probedUntilMu.Unlock()
}

// probeCoolingAccount 对单个账号发起一次冷却前探活并处理结果。
//
// 结果处理（与实施总览 5.3 一致）：
//   - 成功：清除该账号的冷却与失败计数，让它能立刻重新承载流量；
//   - 失败：按阶梯继续递增冷却（下一次探测跟随新的到期时间）；
//   - 任何分支都不写 test_failed。
//
// [参数] candidate：待探测账号；sa：账号凭据。
// [返回] true 表示探测成功。
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增。
func probeCoolingAccount(candidate cooldownProbeCandidate, sa *storedAuth) bool {
	if sa == nil {
		return false
	}
	err := probePingFn(sa, candidate.model)
	if err != nil {
		// 失败：沿阶梯继续递增账号级冷却。用 status 0 是因为探活拿不到有意义
		// 的状态码，而对传输层失败的处理正是「继续冷却」。
		recordFailureScoped(candidate.authID, "", failureScopeAccount)
		// 探活失败不动 test_failed：标签是硬排除、永不自动过期，
		// 写进去比冷严重得多。此处只让冷却继续递增。
		log.Printf("[workbuddy-ai] cooldown probe failed: auth=%s model=%s err=%s",
			candidate.authID, candidate.model, truncateRedacted(err.Error(), 160))
		return false
	}
	resetAccountFailover(candidate.authID)
	log.Printf("[workbuddy-ai] cooldown probe ok: auth=%s model=%s, cooldown cleared",
		candidate.authID, candidate.model)
	return true
}

// runCooldownProbeTick 执行一轮冷却前探活。
// [参数] now：当前时间；resolve：账号标识到凭据的解析函数（测试注入）。
// [返回] 本轮实际探测的账号数。
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增。
func runCooldownProbeTick(now time.Time, resolve func(string) *storedAuth) int {
	targets := cooldownProbeTargets(now)
	probed := 0
	for _, tc := range targets {
		sa := resolve(tc.authID)
		if sa == nil {
			// 解析不到凭据就跳过本周期，不标记已探测——下一 tick 还会再试，
			// 这样临时性宿主 RPC 抖动不会让该账号整轮错过探测。
			continue
		}
		// 先探测再标记：探活失败会沿阶梯延长 cooldownUntil，若在探测前用旧值标记，
		// 新周期会被误判为「本周期已探测」而被永久跳过。
		probeCoolingAccount(tc, sa)
		markCooldownProbed(tc.authID, currentCooldownUntil(tc.authID, tc.cooldownUntil))
		probed++
	}
	return probed
}

// resolveProbeAuth 把冷却状态里的账号键解析为可用的账号凭据。
// 冷却键可能是 auth.ID、UID 或 auth_index，统一走 resolveAuthIndexAndID。
// [参数] authID：冷却状态键。
// [返回] 账号凭据；解析失败返回 nil。
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增。
func resolveProbeAuth(authID string) *storedAuth {
	authIndex, _ := resolveAuthIndexAndID(authID)
	if strings.TrimSpace(authIndex) == "" {
		return nil
	}
	sa, err := hostAuthGet(authIndex)
	if err != nil {
		return nil
	}
	return sa
}

// cooldownProbeLoop 冷却前探活的常驻循环。
//
// 与 watchdogLoop 分离：watchdog 的 10 分钟粒度对最短 60s 的冷却太粗，
// 首档冷却会整轮错过提前 5 秒的探测窗口。
//
// 单轮可能阻塞到探活超时（20s），远大于 5s 的 tick 间隔，因此用
// 「上一轮未结束就跳过本轮」而非排队：堆积轮次只会让同一批账号被重复探测，
// 而探测本身已经按冷却周期去重。
//
// 最近修改时间：2026-10-19 00:30:00；改动原因：新增，补轮次防堆积。
func cooldownProbeLoop() {
	// 与 watchdog 一致：init() 早于宿主装配，立即 tick 会静默失败。
	waitHostReadyForWatchdog(watchdogStartupWait, hostReadyForWatchdog)
	ticker := time.NewTicker(cooldownProbeInterval)
	defer ticker.Stop()
	var running sync.Mutex
	for range ticker.C {
		// 尝试加锁失败说明上一轮还在探测，直接跳过本轮。
		if !running.TryLock() {
			continue
		}
		go func() {
			defer running.Unlock()
			runCooldownProbeTick(probeNowFn(), resolveProbeAuth)
		}()
	}
}
