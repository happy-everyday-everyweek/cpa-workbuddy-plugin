package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 本文件锁定「失败回传状态码」契约。
//
// 宿主只按状态码决定换号与冷却（conductor_selection.go 的
// isCredentialRetryRoundStatus 白名单：403/408/429/500/502/503/504）。
// 插件错误不带 http_status 时宿主取到 0，坏号会被反复挑中
// （2026-10-19 生产实证：积分耗尽的号连续两次被选中并失败）。

// TestExecutionFaultClassifiesRequestFault 验证 4xx 仍归为请求级故障。
//
// 请求本身非法时换号没有意义，只会赔上更多账号的配额；
// 这类失败必须让宿主直接返回用户。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，请求级分类回归。
func TestExecutionFaultClassifiesRequestFault(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "内容超长",
			err:        verifyChatContent(strings.Repeat("x", clawMaxContentBytes+1)),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   faultCodeRequest,
		},
		{
			name:       "请求解析失败",
			err:        withStatus(errors.New("请求缺少 messages"), http.StatusBadRequest),
			wantStatus: http.StatusBadRequest,
			wantCode:   faultCodeRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code := executionFault(tc.err)
			if status != tc.wantStatus {
				t.Errorf("状态码 = %d，期望 %d", status, tc.wantStatus)
			}
			if code != tc.wantCode {
				t.Errorf("错误码 = %q，期望 %q", code, tc.wantCode)
			}
		})
	}
}

// TestExecutionFaultClassifiesQuotaAsRetryableCredential 验证积分耗尽可换号。
//
// 403 是宿主换号白名单里唯一语义贴合「凭证被拒」的码：
// 402 只冷却不换号，429 的冷却下限只 10 秒（积分不可能自愈）。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，积分耗尽分类回归。
func TestExecutionFaultClassifiesQuotaAsRetryableCredential(t *testing.T) {
	env := &clawEnvelope{Code: 1000521, Msg: "该用户积分已耗尽"}
	err := clawBusinessError("提交消息失败", env)
	if err == nil {
		t.Fatal("业务失败必须返回错误")
	}
	if !strings.Contains(err.Error(), "积分已耗尽") {
		t.Errorf("文案应保留上游原文，实际 %q", err.Error())
	}

	status, code := executionFault(err)
	if status != http.StatusForbidden {
		t.Errorf("状态码 = %d，期望 403", status)
	}
	if code != faultCodeQuota {
		t.Errorf("错误码 = %q，期望 %q", code, faultCodeQuota)
	}
}

// TestExecutionFaultDefaultsToUpstreamError 验证未分类失败兜底 502。
//
// 「上游未返回任何图片」这类失败既不携带状态码也不在业务码表里，
// 但它确实是这一轮上游没成功：必须让宿主冷却该号并换号重试，
// 而不是当成 0 状态码放过。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，兜底分类回归。
func TestExecutionFaultDefaultsToUpstreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "未返回图片", err: errors.New("上游未返回任何图片")},
		{name: "回复正文为空", err: errors.New("回复正文为空")},
		{name: "未知业务码", err: clawBusinessError("提交消息失败", &clawEnvelope{Code: 999999, Msg: "未知故障"})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code := executionFault(tc.err)
			if status != http.StatusBadGateway {
				t.Errorf("状态码 = %d，期望 502", status)
			}
			if code != faultCodeUpstream {
				t.Errorf("错误码 = %q，期望 %q", code, faultCodeUpstream)
			}
		})
	}
}

// TestExecutionFaultHonoursInnerStatus 验证底层状态码不被外层覆盖。
//
// 超限错误在进入执行器前就带 413，若被外层统一改写 502，
// 宿主又会把请求级故障当成账号故障去冷却整池
// （2026-10-19 生产实证：一次超长请求让两个账号同时不可用）。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，状态码优先回归。
func TestExecutionFaultHonoursInnerStatus(t *testing.T) {
	inner := verifyChatContent(strings.Repeat("x", clawMaxContentBytes+1))
	wrapped := fmt.Errorf("入口校验失败：%w", inner)

	status, code := executionFault(withUpstreamStatus(wrapped))
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("状态码 = %d，期望保留底层 413", status)
	}
	if code != faultCodeRequest {
		t.Errorf("错误码 = %q，期望 %q", code, faultCodeRequest)
	}
}

// TestWithStatusKeepsErrorChain 验证包装后仍可沿错误链取到原错误。
//
// 失败日志与断言都依赖 errors.Is；一旦包装切断错误链，
// 排障时会只剩「上游失败」这种无从下手的文案。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，错误链回归。
func TestWithStatusKeepsErrorChain(t *testing.T) {
	sentinel := errors.New("底层故障")
	wrapped := withUpstreamStatus(fmt.Errorf("外层说明：%w", sentinel))

	if !errors.Is(wrapped, sentinel) {
		t.Error("包装后必须保留错误链")
	}
	var coder statusCoder
	if !errors.As(wrapped, &coder) {
		t.Fatal("包装后必须能取到状态码")
	}
	if coder.StatusCode() != http.StatusBadGateway {
		t.Errorf("状态码 = %d，期望 502", coder.StatusCode())
	}
	if !strings.Contains(wrapped.Error(), "外层说明") {
		t.Errorf("文案应保留外层说明，实际 %q", wrapped.Error())
	}
}

// TestExecutionFaultEnvelopeAlwaysCarriesStatus 锁定回写路径的整段契约。
//
// 只测分类函数抓不到「调用点仍用无状态码构造器」这种回退：
// 必须直接断言最终信封字节里的 http_status 非 0。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，回写路径回归。
func TestExecutionFaultEnvelopeAlwaysCarriesStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "配额耗尽", err: clawBusinessError("提交消息失败", &clawEnvelope{Code: 1000521, Msg: "该用户积分已耗尽"}), wantStatus: http.StatusForbidden},
		{name: "未返回图片", err: errors.New("上游未返回任何图片"), wantStatus: http.StatusBadGateway},
		{name: "内容超长", err: verifyChatContent(strings.Repeat("x", clawMaxContentBytes+1)), wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := executionFaultEnvelope(tc.err)
			var parsed struct {
				Error struct {
					HTTPStatus int `json:"http_status"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("信封解析失败：%v；原文 %s", err, raw)
			}
			if parsed.Error.HTTPStatus == 0 {
				t.Fatalf("执行失败信封必须带状态码，原文 %s", raw)
			}
			if parsed.Error.HTTPStatus != tc.wantStatus {
				t.Errorf("http_status = %d，期望 %d", parsed.Error.HTTPStatus, tc.wantStatus)
			}
		})
	}
}

// TestSendChatQuotaFailureCarries403 锁定调用点，而不只是分类函数。
//
// 只测 clawBusinessError 抓不到「sendChat 又改回 fmt.Errorf」这种回退：
// 必须从真实调用链拿到错误，再断言它能被归类成可换号的 403
// （2026-10-19 生产实证：积分耗尽的号反复被选中并失败）。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，调用点回归。
func TestSendChatQuotaFailureCarries403(t *testing.T) {
	doer := &fakeDoer{handler: func(*http.Request, []byte) (*http.Response, error) {
		return jsonResponse(200, `{"code":1000521,"msg":"该用户积分已耗尽"}`), nil
	}}
	client := newTestClawClient(doer)

	_, errChat := client.sendChat("s1", "画一只猫")
	if errChat == nil {
		t.Fatal("积分耗尽必须返回错误")
	}
	if !strings.Contains(errChat.Error(), "积分已耗尽") {
		t.Errorf("文案应保留上游原文，实际 %q", errChat.Error())
	}

	status, code := executionFault(errChat)
	if status != http.StatusForbidden {
		t.Errorf("状态码 = %d，期望 403（宿主换号白名单）", status)
	}
	if code != faultCodeQuota {
		t.Errorf("错误码 = %q，期望 %q", code, faultCodeQuota)
	}
}

// TestFaultEnvelopeCarriesStatus 验证信封序列化出 http_status。
//
// 宿主 decodeEnvelopeResult 只读 envelope.Error.HTTPStatus，
// 字段缺失即退化成 0；这是本轮「坏号不被换下」的直接成因。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，信封字段契约。
func TestFaultEnvelopeCarriesStatus(t *testing.T) {
	raw := faultEnvelope(http.StatusForbidden, faultCodeQuota, "该用户积分已耗尽")

	var parsed struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
			Retryable  bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if parsed.OK {
		t.Error("失败信封不得标记 ok=true")
	}
	if parsed.Error.HTTPStatus != http.StatusForbidden {
		t.Errorf("http_status = %d，期望 403", parsed.Error.HTTPStatus)
	}
	if parsed.Error.Code != faultCodeQuota {
		t.Errorf("code = %q", parsed.Error.Code)
	}
}

// TestClawBusinessErrorKeepsUnknownCodeUnclassified 验证未登记的业务码不猜。
//
// 猜错会把请求类错误标成账号故障，导致整池被无谓冷却；
// 未登记的码保持可兜底但不带业务语义。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，避免过度分类。
func TestClawBusinessErrorKeepsUnknownCodeUnclassified(t *testing.T) {
	err := clawBusinessError("提交消息失败", &clawEnvelope{Code: 1000003, Msg: "数据库错误"})
	var coder statusCoder
	if errors.As(err, &coder) {
		t.Errorf("未登记的业务码不应自报状态码，实际 %d", coder.StatusCode())
	}
	if !strings.Contains(err.Error(), "数据库错误") || !strings.Contains(err.Error(), "1000003") {
		t.Errorf("文案应保留上游原文与码值，实际 %q", err.Error())
	}
}
