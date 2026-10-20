package main

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

type failoverEntry struct {
	failCount int
	cooldown  time.Time
	lastError string
	// lastModel 记录最近一次触发冷却的上游模型，供冷却前探活回放。
	lastModel string
	// modelStates 保存账号×模型级冷却，键为上游模型名。
	modelStates map[string]*modelFailoverState
}

// modelFailoverState 记录单个账号×模型维度的连续失败与冷却窗口。
type modelFailoverState struct {
	failCount int
	cooldown  time.Time
}

var (
	failoverMu      sync.RWMutex
	failoverRecords = make(map[string]*failoverEntry)
)

// failoverCooldownBase 是阶梯基数：每次连续失败使冷却窗口增加一个基数，
// 首次失败即冷却一个基数（60s），第 n 次失败冷却 n × 60s。
const failoverCooldownBase = 60 * time.Second

// failoverCooldownMax 是阶梯上限：连续失败再多也不超过 10 分钟。
const failoverCooldownMax = 10 * time.Minute

// failoverCooldownLadderCap 是达到上限所需的连续失败次数（10min / 60s = 10）。
// 先比较次数再相乘，避免 count 极大时的乘法溢出。
const failoverCooldownLadderCap = int(failoverCooldownMax / failoverCooldownBase)

// failoverCooldownFor 返回第 count 次连续失败对应的阶梯冷却时长。
// 时长按 failoverCooldownBase 线性递增并在 failoverCooldownMax 封顶，
// count <= 0 返回零值表示不冷却。
// [参数] count: 当前连续失败次数，由 noteAccountFailure 递增、成功后清零。
// [返回] 本次失败应施加的冷却时长。
// 最近修改时间：2026-10-19；改动原因：阶梯基数由 30s 调整为 60s（仍为 base × count、上限 10 分钟）。
func failoverCooldownFor(count int) time.Duration {
	if count <= 0 {
		return 0
	}
	if count >= failoverCooldownLadderCap {
		return failoverCooldownMax
	}
	return failoverCooldownBase * time.Duration(count)
}

// isAccountCoolingDown 检查账号是否处于冷却期。
func isAccountCoolingDown(authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	failoverMu.RLock()
	defer failoverMu.RUnlock()

	entry, ok := failoverRecords[authID]
	if !ok {
		return false
	}
	return time.Now().Before(entry.cooldown)
}

// noteAccountFailure 记录账号故障并按连续失败次数施加阶梯冷却。
//
// 记账范围由 classifyUpstreamFailure 统一判定：400/422 这类请求形状问题不再
// 计入冷却（换账号同样失败，冷却只会无谓缩小可用池）；传输错误、5xx、账号级
// 4xx、硬额度与限流仍照常记账。
//
// [参数] authID: 账号唯一标识；statusCode: 上游 HTTP 状态码，0 表示传输层失败；
// errMsg: 错误文本。
// [返回] 无。
// 最近修改时间：2026-10-19 02:00:00；改动原因：记账收敛到唯一落点，支持模型维度。
func noteAccountFailure(authID string, statusCode int, errMsg string) {
	noteAccountFailureForModel(authID, "", statusCode, errMsg)
}

// noteAccountFailureForModel 按分类维度记账：账号级失败写账号层，
// 账号×模型级失败写模型层并同时记录 lastModel。
//
// 模型名为空时退化为账号级记账，与历史行为一致，供尚未改造的调用点继续工作。
//
// [参数] authID: 账号唯一标识；model: 已归一化的上游模型名，可为空；
// statusCode: 上游 HTTP 状态码，0 表示传输层失败；errMsg: 错误文本。
// [返回] 无。
// 最近修改时间：2026-10-19 02:00:00；改动原因：新增账号×模型双层冷却。
func noteAccountFailureForModel(authID, model string, statusCode int, errMsg string) {
	recordFailureScoped(authID, model, classifyUpstreamFailure(statusCode, errMsg), errMsg)
}

// recordFailureScoped 按给定维度写入冷却状态，是记账的唯一落点。
// 调用方必须已完成分类；scope 为 none 时不写入。
// [参数] authID: 账号唯一标识；model: 已归一化的上游模型名，可为空；
// scope: 记账维度；errMsg: 错误文本，用于面板展示最近错误。
// [返回] 无。
// 最近修改时间：2026-10-19 02:00:00；改动原因：抽出唯一写入落点，避免多处复制阶梯逻辑。
func recordFailureScoped(authID, model string, scope failureScope, errMsg string) {
	authID = strings.TrimSpace(authID)
	if authID == "" || scope == failureScopeNone {
		return
	}
	model = strings.TrimSpace(model)
	failoverMu.Lock()
	defer failoverMu.Unlock()

	entry, ok := failoverRecords[authID]
	if !ok {
		entry = &failoverEntry{}
		failoverRecords[authID] = entry
	}
	entry.lastError = errMsg

	// 账号×模型级失败：只冷却该模型，不摘掉整个账号。
	// 模型名为空时无维度可写，退化为账号级，避免失败被静默丢弃。
	if scope == failureScopeAuthModel && model != "" {
		if entry.modelStates == nil {
			entry.modelStates = make(map[string]*modelFailoverState)
		}
		entry.lastModel = model
		ms := entry.modelStates[model]
		if ms == nil {
			ms = &modelFailoverState{}
			entry.modelStates[model] = ms
		}
		ms.failCount++
		ms.cooldown = time.Now().Add(failoverCooldownFor(ms.failCount))
		return
	}

	entry.failCount++
	entry.cooldown = time.Now().Add(failoverCooldownFor(entry.failCount))
	if model != "" {
		entry.lastModel = model
	}
}

// resetAccountFailover 成功调用后重置故障计数与冷却状态。
func resetAccountFailover(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	failoverMu.Lock()
	defer failoverMu.Unlock()

	delete(failoverRecords, authID)
}

// isAuthModelCoolingDown 判定账号在指定模型上是否处于冷却中。
//
// 路由与换号必须使用本函数而非 isAccountCoolingDown：账号级冷却会让该账号
// 在所有模型上让位，而账号×模型级冷却只应让位给同一个模型。模型名为空时
// 退化为账号级判定，保证调用方尚未传模型时行为与历史一致。
//
// [参数] authID: 账号唯一标识；model: 已归一化的上游模型名，可为空。
// [返回] true 表示该账号不应承载该模型的请求。
// 最近修改时间：2026-10-19 02:00:00；改动原因：新增账号×模型双层判定。
func isAuthModelCoolingDown(authID, model string) bool {
	if isAccountCoolingDown(authID) {
		return true
	}
	model = strings.TrimSpace(model)
	authID = strings.TrimSpace(authID)
	if model == "" || authID == "" {
		return false
	}
	failoverMu.RLock()
	defer failoverMu.RUnlock()
	entry, ok := failoverRecords[authID]
	if !ok || entry.modelStates == nil {
		return false
	}
	ms := entry.modelStates[model]
	return ms != nil && time.Now().Before(ms.cooldown)
}

// resetAuthModelFailover 只清除指定账号×模型的冷却，保留账号级状态。
// 供冷却前探活确认「该模型已恢复」时调用，避免把其它模型的冷却一起抹掉。
// [参数] authID: 账号唯一标识；model: 已归一化的上游模型名。
// [返回] 无。
// 最近修改时间：2026-10-19 02:00:00；改动原因：新增，供探活精确清除模型维度。
func resetAuthModelFailover(authID, model string) {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return
	}
	failoverMu.Lock()
	defer failoverMu.Unlock()
	if entry, ok := failoverRecords[authID]; ok && entry.modelStates != nil {
		delete(entry.modelStates, model)
	}
}

// clearAuthModelCooldowns 清除某个模型上的冷却记录。
//
// 传空 authID 时清空该模型在所有账号上的记录，用于「一次成功证明模型已恢复」
// 的场景；传了 authID 时只清该账号。账号级冷却不受影响——它代表账号整体不可用，
// 与单个模型的恢复无关。
//
// [参数] authID: 账号标识，空表示全部账号；model: 已归一化的上游模型名。
// [返回] 无。
// 最近修改时间：2026-10-20；改动原因：新增，统一模型维度清理的唯一落点。
func clearAuthModelCooldowns(authID, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	authID = strings.TrimSpace(authID)
	failoverMu.Lock()
	defer failoverMu.Unlock()
	for id, entry := range failoverRecords {
		if entry == nil || entry.modelStates == nil {
			continue
		}
		if authID != "" && strings.TrimSpace(id) != authID {
			continue
		}
		delete(entry.modelStates, model)
	}
}

// lastFailoverModel 返回账号最近一次触发冷却的模型名，供冷却前探活回放。
// [参数] authID: 账号唯一标识。
// [返回] 归一化后的上游模型名；无记录时为空串。
// 最近修改时间：2026-10-19 02:00:00；改动原因：新增，供冷却前探活读取失败模型。
func lastFailoverModel(authID string) string {
	failoverMu.RLock()
	defer failoverMu.RUnlock()
	if entry, ok := failoverRecords[strings.TrimSpace(authID)]; ok {
		return entry.lastModel
	}
	return ""
}

// authModelFailoverSnapshot 返回账号×模型维度的冷却状态，供单测与诊断使用。
// [参数] authID: 账号唯一标识；model: 已归一化的上游模型名。
// [返回] 该模型的连续失败次数、冷却截止时间、是否存在记录。
// 最近修改时间：2026-10-19 02:00:00；改动原因：新增账号×模型双层诊断入口。
func authModelFailoverSnapshot(authID, model string) (count int, cooldownUntil time.Time, ok bool) {
	failoverMu.RLock()
	defer failoverMu.RUnlock()
	entry, exists := failoverRecords[strings.TrimSpace(authID)]
	if !exists || entry.modelStates == nil {
		return 0, time.Time{}, false
	}
	ms := entry.modelStates[strings.TrimSpace(model)]
	if ms == nil {
		return 0, time.Time{}, false
	}
	return ms.failCount, ms.cooldown, true
}

// shouldRotateOnUpstreamErr 判断上游错误是否应触发同请求换号重试。
//
// GitHub Copilot 的鉴权失败（401/403）、配额耗尽（429）与上游 5xx 都属账号级
// 或配额级故障，换号可能恢复；400 类业务参数错误换号无意义，不重试。
// [参数] statusCode: 上游 HTTP 状态码；errMsg: 错误文本。
// [返回] true 表示应换号重试。
// 最近修改时间：2026-10-15；改动原因：新增，Copilot 上游错误换号判定。
func shouldRotateOnUpstreamErr(statusCode int, errMsg string) bool {
	switch statusCode {
	case http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	lower := strings.ToLower(errMsg)
	for _, kw := range []string{
		"unauthorized",
		"forbidden",
		"rate limit",
		"quota",
		"exceeded",
		"insufficient",
		"bad credentials",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
