package main

import (
	"errors"
	"fmt"
	"net/http"
)

// 本文件定义执行失败回传宿主时的状态码分类。
//
// 宿主只按状态码决定「这是账号故障还是请求故障」：conductor_selection.go 的
// isCredentialRetryRoundStatus 只认 403/408/429/500/502/503/504。
// 插件错误信封不带 http_status 时宿主取到 0，既不会冷却该账号，也不会
// 进入外层换号重试轮——坏号会被后续每个请求反复挑中，每次都先白等一轮
// 上游失败（2026-10-19 生产实证：积分耗尽的号反复出现在失败日志里）。
//
// 分类原则：
//   - 4xx（除 402）：请求本身非法，换号重试也不会成功；
//   - 402：账号资源耗尽，换号有效、同号重试无效；
//   - 5xx：账号或上游的瞬时故障，冷却该号并换号重试。

const (
	// upstreamStatusQuota 表示账号资源耗尽（扣子积分不足）。
	//
	// 用 403 而不是 402/429：宿主的换号白名单
	// （isCredentialRetryRoundStatus）只有 403/408/429/500/502/503/504，
	// 402 虽然能让账号冷却 30 分钟，却不会触发换号，用户仍会看到失败。
	// 429 会走配额退避阶梯，冷却下限只有 10 秒，积分耗尽不可能在 10 秒
	// 内自愈；403 在 MarkResult 里固定冷却 30 分钟，语义也正是
	// 「上游拒绝了这个凭证」。
	upstreamStatusQuota = http.StatusForbidden

	// upstreamStatusDefault 是未标注失败的兜底状态码。
	//
	// 选 502 而不是 500：网关语义更贴合「上游这一轮没成功」，
	// 同样命中宿主的冷却与换号白名单，且不会像 401 那样
	// 触发无意义的凭证刷新。
	upstreamStatusDefault = http.StatusBadGateway
)

const (
	// faultCodeRequest 表示请求级故障。
	faultCodeRequest = "invalid_request"
	// faultCodeQuota 表示账号资源耗尽。
	faultCodeQuota = "quota_exhausted"
	// faultCodeUpstream 表示账号或上游瞬时故障。
	faultCodeUpstream = "upstream_error"
)

// statusCoder 是可自报 HTTP 状态码的错误。
//
// 4xx 表示请求级故障（不换号），402 与 5xx 表示账号级故障（换号并冷却）；
// 未实现该接口的错误由 executionFault 兜底成 502。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，状态码识别契约。
type statusCoder interface {
	StatusCode() int
}

// upstreamFaultError 给上游失败补一个宿主可识别的状态码。
//
// 只包装不改文案：下游看到的仍是上游原始说明，
// 状态码只参与宿主分类，不进入用户可见文本。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，账号级故障载体。
type upstreamFaultError struct {
	status int
	err    error
}

// Error 返回原始错误说明。
// [参数] 无。
// [返回] 错误文案。
// 最近修改时间：2026-10-19；改动原因：新增，错误接口实现。
func (e *upstreamFaultError) Error() string {
	return e.err.Error()
}

// Unwrap 保留错误链，供 errors.Is / errors.As 继续匹配。
// [参数] 无。
// [返回] 被包装的错误。
// 最近修改时间：2026-10-19；改动原因：新增，错误链透传。
func (e *upstreamFaultError) Unwrap() error {
	return e.err
}

// StatusCode 返回宿主分类用的 HTTP 状态码。
// [参数] 无。
// [返回] 状态码。
// 最近修改时间：2026-10-19；改动原因：新增，宿主分类入口。
func (e *upstreamFaultError) StatusCode() int {
	return e.status
}

// withStatus 给错误补状态码，已有状态码时保持原样。
//
// 底层错误最了解自己的语义：413 超限不能在外层被升级成 502，
// 否则宿主又会把请求级故障当成账号故障去冷却。
// [参数] err: 待标注错误；status: 期望状态码。
// [返回] 带状态码的错误；入参为 nil 时返回 nil。
// 最近修改时间：2026-10-19；改动原因：新增，状态码标注。
func withStatus(err error, status int) error {
	if err == nil {
		return nil
	}
	var coder statusCoder
	if errors.As(err, &coder) && coder.StatusCode() > 0 {
		return err
	}
	return &upstreamFaultError{status: status, err: err}
}

// withUpstreamStatus 用兜底状态码标注未分类的上游失败。
// [参数] err: 待标注错误。
// [返回] 带 502 的错误。
// 最近修改时间：2026-10-19；改动原因：新增，上游失败兜底。
func withUpstreamStatus(err error) error {
	return withStatus(err, upstreamStatusDefault)
}

// clawAccountErrorCodes 是扣子明确表示「这个号不能再用」的业务码。
//
// 只登记已实证的码值：猜错会把请求错误也标成账号故障，
// 导致整池被无谓冷却。未登记的码保持无状态码，由兜底 502 处理。
var clawAccountErrorCodes = map[int]struct{}{
	// 1000521：积分耗尽。同号重试必然再失败，换号后仍有额度的号可以成功。
	1000521: {},
}

// clawBusinessError 把 claw 业务失败按 code 分类。
//
// claw 端点统一返回 HTTP 200 + code 表达业务结果，HTTP 状态码本身没有
// 信息量，必须看 code 才能区分「账号没额度」与「请求写错了」。
// [参数] prefix: 操作说明前缀；env: 上游响应外壳。
// [返回] 按 code 分类后的错误。
// 最近修改时间：2026-10-19；改动原因：新增，积分耗尽归入账号级故障。
func clawBusinessError(prefix string, env *clawEnvelope) error {
	if env == nil {
		return fmt.Errorf("%s：上游无响应", prefix)
	}
	err := fmt.Errorf("%s：%s", prefix, env.describe())
	if _, known := clawAccountErrorCodes[env.Code]; known {
		return withStatus(err, upstreamStatusQuota)
	}
	return err
}

// executionFault 把执行错误归类成回写给宿主的状态码与错误码。
//
// 按状态码分流：402/4xx 之外的账号级失败（403 配额耗尽、5xx 瞬时故障）
// 让宿主冷却该号并换号；请求级 4xx 直接返回用户，不赔其他账号的配额。
// [参数] err: 执行错误。
// [返回] HTTP 状态码与错误码。
// 最近修改时间：2026-10-19；改动原因：新增，失败分类唯一出口。
func executionFault(err error) (int, string) {
	var coder statusCoder
	if errors.As(err, &coder) {
		status := coder.StatusCode()
		switch {
		case status == upstreamStatusQuota:
			return status, faultCodeQuota
		case status >= 400 && status < 500:
			return status, faultCodeRequest
		case status >= 500 && status < 600:
			return status, faultCodeUpstream
		}
	}
	return upstreamStatusDefault, faultCodeUpstream
}

// executionFaultEnvelope 把执行错误直接构造成失败信封。
//
// 单独成函数是为了让「分类 → 信封」这一整段可测：writeExecutionError
// 依赖 cgo 缓冲区，无法在无 C 工具链的环境里断言实际写出了什么。
// [参数] err: 执行错误。
// [返回] 信封 JSON。
// 最近修改时间：2026-10-19；改动原因：新增，回写路径可测。
func executionFaultEnvelope(err error) []byte {
	status, code := executionFault(err)
	return faultEnvelope(status, code, err.Error())
}
