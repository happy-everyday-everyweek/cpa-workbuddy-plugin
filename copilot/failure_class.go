// failure_class.go 把上游失败分类固化为纯函数，作为冷却维度与探活策略的单一判据。
//
// 与 workbuddy 系的关键差异（周期 1 勘察结论）：
// 本插件的 noteAccountFailure 是无条件记账——只要调用方判定要记账，就递增计数
// 并按阶梯冷却，不再检查状态码与正文。换号判定则由 shouldRotateOnUpstreamErr
// 独立负责（401/403/404/405/429/5xx + 正文关键词）。
//
// 也就是说，本插件当前的实际边界是「调用方决定要不要记账」，而设计总览第 3 节
// 要求的是「状态码与正文决定记账维度」。两者对 400 的处理不同：设计表要求 400
// 不冷却，而调用方在部分路径上仍会把它交给 noteAccountFailure。
//
// 周期 1 的约束是行为零变化，因此本轮只建立分类规格函数与单测，供周期 2 接入
// 记账路径时使用；当前 noteAccountFailure 保持原样，偏差在测试中显式断言。
//
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增失败分类纯函数，固化实施总览第 3 节边界表。
package main

import (
	"net/http"
	"strings"
)

// failureScope 表示一次上游失败应记入的冷却维度。
// 零值 failureScopeNone 表示不进入任何冷却（请求形状问题或客户端主动断开）。
type failureScope int

const (
	// failureScopeNone 不冷却：换账号也会同样失败，或根本不是账号故障。
	failureScopeNone failureScope = iota
	// failureScopeAccount 账号级冷却：凭据、端点、额度或传输层问题，与模型无关。
	failureScopeAccount
	// failureScopeAuthModel 账号×模型级冷却：限流或业务错误帧通常按账号+模型生效。
	failureScopeAuthModel
)

// String 返回分类的可读名，供日志与测试断言使用。
func (s failureScope) String() string {
	switch s {
	case failureScopeAccount:
		return "account"
	case failureScopeAuthModel:
		return "account_model"
	default:
		return "none"
	}
}

// hardCreditMarkers 匹配正文中的额度耗尽信号，与 workbuddy 系保持同一口径。
var hardCreditMarkers = []string{
	"insufficient credit",
	"insufficient credits",
	"no credit",
	"no credits",
	"credit exhausted",
	"credits exhausted",
	"out of credit",
	"out of credits",
	"quota exceeded",
	"quota exhaust",
	"exceeded the quota",
	"14018",
	"payment required",
	"点数不足",
	"额度不足",
	"余额不足",
	"配额用尽",
	"点数耗尽",
	"没有积分",
	"credit not enough",
	"not enough credit",
}

// isHardCreditError 判定失败是否表示账号额度耗尽。
// [参数] status：上游 HTTP 状态码；body：上游响应正文或错误文本。
// [返回] true 表示 402 或正文含额度耗尽标记。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增，与 workbuddy 系统一口径。
func isHardCreditError(status int, body string) bool {
	if status == http.StatusPaymentRequired {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range hardCreditMarkers {
		if strings.Contains(lower, m) || strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// isSoftRateLimit 判定失败是否属于软限流（不含硬额度语义）。
// [参数] status：上游 HTTP 状态码；body：上游响应正文或错误文本。
// [返回] true 表示 429 或正文含限流文案。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func isSoftRateLimit(status int, body string) bool {
	if isHardCreditError(status, body) {
		return false
	}
	if status == http.StatusTooManyRequests {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "throttl")
}

// classifyUpstreamFailure 把一次上游失败归类到冷却维度。
//
// 判定顺序与 workbuddy 系一致：先看硬额度与账号级 4xx，再看传输/5xx，
// 最后看限流与零字节断流；400/422 在正文无标记时不记账。
//
// 注意：本函数目前只表达「目标规格」，尚未被 noteAccountFailure 调用。
// 接入点在周期 2，与冷却状态扩维一起落地。
//
// [参数] status：上游 HTTP 状态码，0 表示传输层失败；body：上游响应正文或错误文本。
// [返回] 该失败应记入的冷却维度。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增，按已确认边界表固化分类。
func classifyUpstreamFailure(status int, body string) failureScope {
	// 1. 硬额度与凭据/端点问题属账号级：换模型也不可用。
	if isHardCreditError(status, body) || isAccountLevel4xxExcludingRateLimit(status) {
		return failureScopeAccount
	}
	// 2. 传输错误与 5xx 属账号级。
	if status == 0 || status >= 500 {
		return failureScopeAccount
	}
	// 3. 请求被拦截（11128）按账号×模型冷却：冷却探活会用同一模型回放，
	//    验证该账号在这个模型上是否真的恢复。先于限流文案判定，因为 11128
	//    的 displayMsg 可能带「请重新发送」这类无关文案。
	if isModelBlockedError(status, body) {
		return failureScopeAuthModel
	}
	// 4. 限流文案与零字节断流按账号×模型处理。
	if isSoftRateLimit(status, body) || isEmptyStreamBody(body) {
		return failureScopeAuthModel
	}
	// 5. 其余不记账：400/422 等请求形状问题换账号同样失败。
	return failureScopeNone
}

// isAccountLevel4xx 判定是否为账号级 4xx（不含 429）。
// [参数] status：上游 HTTP 状态码。
// [返回] true 表示 401/403/404/405。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func isAccountLevel4xx(status int) bool {
	switch status {
	case http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusMethodNotAllowed:
		return true
	}
	return false
}

// isAccountLevel4xxExcludingRateLimit 保留与 workbuddy 系同名语义。
// 本插件的 isAccountLevel4xx 已排除 429，因此直接委托。
// [参数] status：上游 HTTP 状态码。
// [返回] true 表示 401/403/404/405。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func isAccountLevel4xxExcludingRateLimit(status int) bool {
	return isAccountLevel4xx(status)
}

// isEmptyStreamBody 判定失败文本是否描述上游零字节断流。
// [参数] body：上游响应正文或错误文本。
// [返回] true 表示零字节断流。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func isEmptyStreamBody(body string) bool {
	return strings.Contains(body, "closed before first payload") ||
		strings.Contains(body, "empty stream") ||
		strings.Contains(body, "invalid SSE response: missing output and done event")
}

// isRequestShapeFailure 判定失败是否由请求本身引起，供调用方判断「不值得换号」。
// [参数] status：上游 HTTP 状态码。
// [返回] true 表示 400/422 这类换账号也会同样失败的请求形状问题。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增。
func isRequestShapeFailure(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
}

// isModelBlockedError 判定一次失败是否属于「上游拦截了这次请求」。
//
// 只认 11128（request illegal / 请求被拦截）。11140 是账号级封禁、6004 是
// 频率限制，两者分别由账号冷却与限流逻辑处理，不能混进这里。
//
// 该判据同时用于两个落点：失败分类（写入账号×模型冷却）与同请求换号决策
// （一次命中就够，不必把预算耗在其余账号上）。
//
// [参数] status：上游 HTTP 状态码；body：上游响应正文。
// [返回] 命中 11128 语义时返回 true。
// 最近修改时间：2026-10-20；改动原因：从 model_circuit.go 迁入，成为唯一判定点。
func isModelBlockedError(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusForbidden {
		return false
	}
	// 11140 = 账号级封禁，必须排除，否则会把封号误判成模型拦截。
	if strings.Contains(body, "11140") {
		return false
	}
	return strings.Contains(body, "11128")
}
