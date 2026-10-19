package main

// 本文件实现账号级阶梯冷却与同请求换号。
//
// 上游账号失败（401/403/429/5xx / 传输错误 / trust-rule-denied）后进入冷却窗口，
// 窗口内调度一律跳过该账号，让请求改落到健康账号，而不是继续往已经失效的账号
// 上堆失败。
//
// 冷却按连续失败次数阶梯递增：第 n 次为 n × 基数（默认 30s），封顶 10 分钟；
// 任意一次成功即清零并解除冷却。状态只存内存：不写 auth 文件、不写 DB，
// 进程重启即清空——冷却只是运行期路由优化，不是账号状态。
//
// 业务 400 不计入：那说明请求本身有问题，换任何账号都会同样失败，计入只会
// 把健康账号逐个冷却掉。

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// failoverCooldownMaxSeconds 是阶梯上限的秒数。
	failoverCooldownMaxSeconds = 600

	// failoverDefaultBaseSeconds 是配置缺失时的阶梯基数（60s 起、每次 +60s）。
	failoverDefaultBaseSeconds = 60
)

var (
	// failoverMu 保护 failoverStates。
	failoverMu sync.Mutex

	// failoverStates 按账号标识记录连续失败次数与冷却截止时间。
	failoverStates = make(map[string]*authFailoverState)
)

// authFailoverState 是一个账号的冷却状态。
type authFailoverState struct {
	// Count 是连续失败次数，成功一次即清零。
	Count int
	// CooldownUntil 是冷却截止时间；零值表示未冷却。
	CooldownUntil time.Time
}

// failoverCooldownFor 返回第 count 次连续失败对应的冷却时长。
//
// 基数取自运行期配置的 cooldown_seconds，让用户可以在面板上调参；
// 上限固定 10 分钟，防止异常账号长时间退出轮转。
// [参数] count: 当前连续失败次数；baseSeconds: 阶梯基数（秒）。
// [返回] 冷却时长；count <= 0 返回 0。
// 最近修改时间：2026-10-18；改动原因：新增，阶梯计算。
func failoverCooldownFor(count, baseSeconds int) time.Duration {
	if count <= 0 {
		return 0
	}
	if baseSeconds <= 0 {
		baseSeconds = failoverDefaultBaseSeconds
	}
	// 先按次数封顶再相乘，避免 count 极大时的乘法溢出。
	if count > failoverCooldownMaxSeconds/baseSeconds {
		return failoverCooldownMaxSeconds * time.Second
	}
	seconds := baseSeconds * count
	if seconds > failoverCooldownMaxSeconds {
		seconds = failoverCooldownMaxSeconds
	}
	return time.Duration(seconds) * time.Second
}

// isAccountFailure 判断一次失败是否应计入账号冷却。
//
// 计入：传输层失败（status 0）、5xx、429、402（额度耗尽）、
// 401/403/404/405（登录态失效、trust rule 拒绝、空间或端点不可用）。
// 不计入：400 等业务错误——请求本身有问题，换号无意义且会误伤健康账号。
// [参数] status: HTTP 状态码。
// [返回] 应计入时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，失败分类。
func isAccountFailure(status int) bool {
	if status == 0 || status >= 500 {
		return true
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusPaymentRequired, http.StatusTooManyRequests:
		return true
	}
	return false
}

// shouldRotate 判断一次失败是否值得在同一次请求内换号。
//
// 429、额度耗尽值得立刻换号（换号即可恢复）；5xx 与传输错误只记冷却、
// 不在同请求内烧预算——它们往往是上游整体抖动，换号也大概率失败。
// 401/403/404/405 由 retry_on_4xx 预算控制：预算为 0 时只冷却不换号，
// 让用户能一键关掉 40x 换号而保留 429 换号。
// [参数] status: HTTP 状态码；retryOn4xxBudget: 本轮剩余的 40x 换号预算。
// [返回] 应换号时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，同请求换号判定。
func shouldRotate(status int, retryOn4xxBudget int) bool {
	switch status {
	case http.StatusPaymentRequired, http.StatusTooManyRequests:
		return true
	case http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusMethodNotAllowed:
		return retryOn4xxBudget > 0
	}
	return false
}

// noteAccountFailure 递增连续失败计数并施加阶梯冷却。
// [参数] authID: 账号标识；status: HTTP 状态码；baseSeconds: 阶梯基数（秒）。
// [返回] true 表示已计入冷却。
// 最近修改时间：2026-10-18；改动原因：新增，失败记账。
func noteAccountFailure(authID string, status, baseSeconds int) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || !isAccountFailure(status) {
		return false
	}

	failoverMu.Lock()
	defer failoverMu.Unlock()

	state := failoverStates[authID]
	if state == nil {
		state = &authFailoverState{}
		failoverStates[authID] = state
	}
	state.Count++
	state.CooldownUntil = time.Now().Add(failoverCooldownFor(state.Count, baseSeconds))
	return true
}

// isAccountCoolingDown 判断账号是否仍在冷却窗口内。
// [参数] authID: 账号标识。
// [返回] 冷却中返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，路由跳过判定。
func isAccountCoolingDown(authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}

	failoverMu.Lock()
	defer failoverMu.Unlock()

	state := failoverStates[authID]
	return state != nil && time.Now().Before(state.CooldownUntil)
}

// resetAccountFailover 在请求成功后清零计数并解除冷却。
// [参数] authID: 账号标识。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，成功清零。
func resetAccountFailover(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}

	failoverMu.Lock()
	defer failoverMu.Unlock()
	delete(failoverStates, authID)
}

// accountFailureCount 返回账号当前连续失败次数（供面板展示）。
// [参数] authID: 账号标识。
// [返回] 连续失败次数。
// 最近修改时间：2026-10-18；改动原因：新增，面板状态展示。
func accountFailureCount(authID string) int {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}

	failoverMu.Lock()
	defer failoverMu.Unlock()
	if state := failoverStates[authID]; state != nil {
		return state.Count
	}
	return 0
}

// classifyInferenceError 从推理错误里取出 HTTP 状态码。
//
// 三类错误的分类口径：
//   - notionAPIError：上游给了非 200，用真实状态码；
//   - trust-rule-denied：HTTP 200 但被权限拒绝，等价于 403；
//   - 其它（建连失败、空正文、流中断）：按传输错误处理，返回 0。
//
// [参数] err: 推理错误。
// [返回] 用于冷却判定的状态码。
// 最近修改时间：2026-10-18；改动原因：新增，错误到状态码的归一。
func classifyInferenceError(err error) int {
	if err == nil {
		return 0
	}
	var apiErr *notionAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	if errors.Is(err, errTrustRuleDenied) || strings.Contains(err.Error(), trustRuleDeniedMarker) {
		return http.StatusForbidden
	}
	return 0
}

// isRetryOn4xxStatus 判断状态码是否落在 retry_on_4xx 预算覆盖范围内。
// [参数] status: HTTP 状态码。
// [返回] 是否覆盖。
// 最近修改时间：2026-10-18；改动原因：新增，4xx 预算口径。
func isRetryOn4xxStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusMethodNotAllowed:
		return true
	}
	return false
}

// resetFailoverStates 清空全部冷却状态（仅测试使用）。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，避免测试间状态串扰。
func resetFailoverStates() {
	failoverMu.Lock()
	defer failoverMu.Unlock()
	failoverStates = make(map[string]*authFailoverState)
}
