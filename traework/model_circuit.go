// model_circuit.go 实现模型级风控熔断：同一 provider × 模型在短窗口内跨多个
// 账号命中同一个风控错误码时，判定为上游对「模型」的拦截，而非账号故障。
//
// 背景（2026-10-18 生产取证）：workbuddy-ai-provider 的 deepseek-v4.1-flash
// 在 3 小时内返回 1173 次 {"code":11128,"msg":"request illegal"}，20 个账号
// 命中次数几乎均匀（55~73 次/账号），而其他模型正常。该形态不可能由账号故障
// 产生：账号级故障会让失败的账号逐个退出轮转，而不是全池等量失败。同一上游
// 家族的其他插件（含本插件）共用这套判定口径，防止风控信号被按账号逐个稀释。
//
// 若继续按账号粒度处理，调度器会逐个换号，每个账号各赔一次 0.5~3.7 秒的上游
// 往返，形成「整池轮转后才失败」的时间损耗（生产实测单链路 4.5~10.3 秒）。
// 本模块把这类失败折叠成一次模型级判定：达到阈值后直接短路，不再消耗账号。
//
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增模型级风控熔断。
package main

import (
	"strings"
	"sync"
	"time"
)

const (
	// modelCircuitDistinctAuths 是触发熔断所需的「不同账号」数量。
	// 用不同账号数而非失败次数：单账号反复失败是账号故障，只有跨账号
	// 等量失败才说明拦截点在模型/租户层。
	modelCircuitDistinctAuths = 3

	// modelCircuitWindow 是统计窗口。生产实测 11128 在同一分钟内跨 20 个
	// 账号出现，3 分钟窗口足以聚合出判定信号，又不会把已恢复的历史失败
	// 一直算进来。
	modelCircuitWindow = 3 * time.Minute

	// modelCircuitHold 是熔断后的抑制时长。取 60 秒：足够让调度侧跳过该
	// 模型，又不至于在上游恢复后仍长时间拦截正常流量。
	modelCircuitHold = 60 * time.Second
)

// modelCircuitState 记录某个 provider × 模型的风控聚合状态。
type modelCircuitState struct {
	auths       map[string]time.Time // 账号 → 最近一次命中时间
	blockedTill time.Time            // 熔断截止时间，零值表示未熔断
}

var (
	modelCircuitMu     sync.Mutex
	modelCircuitStates = make(map[string]*modelCircuitState)
)

// modelCircuitKey 生成 provider × 模型的聚合键。
// [参数] provider：插件 provider 名；model：已归一化的上游模型名。
// [返回] 聚合键；任一为空时返回空串表示不参与熔断。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func modelCircuitKey(provider, model string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	if provider == "" || model == "" {
		return ""
	}
	return provider + "\x00" + model
}

// isModelCircuitError 判定一次失败是否属于「模型级风控」信号。
// 只有明确指向请求被拦截、且与具体账号无关的错误码才计数：账号级封禁
// （11140「Your account」）与限流（6004）不在此列，它们分别由账号冷却和
// 频率限制逻辑处理。
// [参数] status：上游 HTTP 状态码；body：上游响应正文。
// [返回] 命中模型级风控语义时返回 true。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func isModelCircuitError(status int, body string) bool {
	if status != 400 && status != 403 {
		return false
	}
	// 11128 = 请求被拦截（模型级）；11140 是账号级封禁，必须排除。
	if strings.Contains(body, "11140") {
		return false
	}
	return strings.Contains(body, "11128")
}

// noteModelCircuitHit 记录一次模型级风控命中，并在跨账号达到阈值时熔断。
// [参数] provider：插件 provider 名；model：上游模型名；authID：命中账号。
// [返回] 本次命中后该模型是否处于熔断状态。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func noteModelCircuitHit(provider, model, authID string) bool {
	key := modelCircuitKey(provider, model)
	if key == "" {
		return false
	}
	now := time.Now()
	modelCircuitMu.Lock()
	defer modelCircuitMu.Unlock()

	st := modelCircuitStates[key]
	if st == nil {
		st = &modelCircuitState{auths: make(map[string]time.Time)}
		modelCircuitStates[key] = st
	}

	// 1. 清理窗口外的账号记录，避免历史失败长期占位
	for id, at := range st.auths {
		if now.Sub(at) > modelCircuitWindow {
			delete(st.auths, id)
		}
	}

	// 2. 记录本次命中
	if authID = strings.TrimSpace(authID); authID != "" {
		st.auths[authID] = now
	}

	// 3. 跨账号数达到阈值即熔断；已熔断期间继续命中则续期，
	//    避免持续拦截的模型每 60 秒放行一次再失败。
	if len(st.auths) >= modelCircuitDistinctAuths {
		st.blockedTill = now.Add(modelCircuitHold)
	}
	return now.Before(st.blockedTill)
}

// isModelCircuitOpen 判定某 provider × 模型当前是否处于熔断抑制中。
// [参数] provider：插件 provider 名；model：上游模型名。
// [返回] 熔断中返回 true，调用方应快速失败而不消耗账号。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func isModelCircuitOpen(provider, model string) bool {
	key := modelCircuitKey(provider, model)
	if key == "" {
		return false
	}
	modelCircuitMu.Lock()
	defer modelCircuitMu.Unlock()
	st := modelCircuitStates[key]
	if st == nil || st.blockedTill.IsZero() {
		return false
	}
	return time.Now().Before(st.blockedTill)
}

// resetModelCircuit 清除某个 provider × 模型的熔断与计数。
// 上游恢复正常（一次成功请求）时调用，避免残留状态影响后续判定。
// [参数] provider：插件 provider 名；model：上游模型名。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func resetModelCircuit(provider, model string) {
	key := modelCircuitKey(provider, model)
	if key == "" {
		return
	}
	modelCircuitMu.Lock()
	delete(modelCircuitStates, key)
	modelCircuitMu.Unlock()
}

// clearModelCircuits 清空全部熔断状态。测试辅助，生产路径不调用。
func clearModelCircuits() {
	modelCircuitMu.Lock()
	modelCircuitStates = make(map[string]*modelCircuitState)
	modelCircuitMu.Unlock()
}

// modelCircuitSnapshot 返回某 provider × 模型的当前状态快照。
// [参数] provider：插件 provider 名；model：上游模型名。
// [返回] 命中账号数、熔断截止时间、是否存在记录。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func modelCircuitSnapshot(provider, model string) (authCount int, blockedTill time.Time, ok bool) {
	key := modelCircuitKey(provider, model)
	if key == "" {
		return 0, time.Time{}, false
	}
	modelCircuitMu.Lock()
	defer modelCircuitMu.Unlock()
	st := modelCircuitStates[key]
	if st == nil {
		return 0, time.Time{}, false
	}
	return len(st.auths), st.blockedTill, true
}
