package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件锁定 message/chat 的内容体积上限契约。
//
// 上游把 content 按 65536 字节落库，越界返回 code=1000003「数据库错误」
// 而不是参数错误；宿主对这个码只会当账号级故障，把账号打入冷却后
// 整个池返回 auth_unavailable（2026-10-19 生产实证：一次超长请求让
// 两个账号同时不可用，之后所有请求 3ms 内 503）。

// TestVerifyChatContentBoundary 验证 64KB 边界的精确判据。
//
// 二分实测：65535 字节成功、65536 字节失败，因此上限取 65535 且按
// 字节而不是字符计数——中文一字符三字节，按字符判会漏判。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，内容上限边界。
func TestVerifyChatContentBoundary(t *testing.T) {
	if err := verifyChatContent(strings.Repeat("x", clawMaxContentBytes)); err != nil {
		t.Fatalf("上限内不应报错：%v", err)
	}
	if err := verifyChatContent(strings.Repeat("x", clawMaxContentBytes+1)); err == nil {
		t.Fatal("超出上限必须报错")
	}

	// 中文按字节计：21845 个汉字 = 65535 字节（通过），
	// 21846 个 = 65538 字节（拒绝）。若按 rune 计数会全部放行。
	cjkOk := strings.Repeat("测", clawMaxContentBytes/3)
	if err := verifyChatContent(cjkOk); err != nil {
		t.Fatalf("恰好 65535 字节的中文不应报错：%v", err)
	}
	if err := verifyChatContent(cjkOk + "测"); err == nil {
		t.Fatal("超过 65535 字节的中文必须报错")
	}
}

// TestChatContentTooLongErrorCarries413 验证超限错误自带 413 状态码。
//
// 宿主只按错误状态码区分「请求级故障」与「账号级故障」：
// 没有状态码时一律当 5xx，会冷却账号并换号重试。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，状态码语义。
func TestChatContentTooLongErrorCarries413(t *testing.T) {
	err := verifyChatContent(strings.Repeat("x", clawMaxContentBytes+10))
	if err == nil {
		t.Fatal("前置条件：应当超限")
	}
	coder, ok := err.(statusCoder)
	if !ok {
		t.Fatalf("超限错误必须实现 StatusCode，实际类型 %T", err)
	}
	if code := coder.StatusCode(); code != http.StatusRequestEntityTooLarge {
		t.Errorf("状态码 = %d，期望 413", code)
	}
	if !strings.Contains(err.Error(), "65535") || !strings.Contains(err.Error(), "65545") {
		t.Errorf("文案应同时给出实际字节与上限，实际 %q", err.Error())
	}
}

// TestRequestFaultEnvelopeCarriesStatus 验证信封序列化出 http_status。
//
// 宿主的 rpc_client 只在 envelope.Error.HTTPStatus > 0 时构造带状态码的
// 错误；字段缺失即退化成 0，请求级故障会被误当成账号故障。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：改为新信封出口的重名契约。
func TestRequestFaultEnvelopeCarriesStatus(t *testing.T) {
	raw := faultEnvelope(http.StatusRequestEntityTooLarge, faultCodeRequest, "请求内容过长")
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
	if parsed.Error.HTTPStatus != http.StatusRequestEntityTooLarge {
		t.Errorf("http_status = %d，期望 413", parsed.Error.HTTPStatus)
	}
	if parsed.Error.Code != "invalid_request" {
		t.Errorf("code = %q", parsed.Error.Code)
	}
	// 请求级故障不应提示宿主重试：同一份输入重试多少次都一样。
	if parsed.Error.Retryable {
		t.Error("请求级故障不应标记 retryable")
	}
}

// TestCommonErrorEnvelopeOmitsStatus 验证无状态码的下层构造器行为。
//
// errorEnvelope 仍是方法分发等非执行路径的通用出口，它不带状态码；
// 执行路径必须走 faultEnvelope（见 faults_test.go），
// 否则宿主取到 0 会既不冷却也不换号。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：明确通用出口的适用边界。
func TestCommonErrorEnvelopeOmitsStatus(t *testing.T) {
	raw := errorEnvelope("plugin_error", "提交消息失败：该用户积分已耗尽")
	var parsed struct {
		Error struct {
			HTTPStatus int `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if parsed.Error.HTTPStatus != 0 {
		t.Errorf("普通错误不得携带状态码，实际 %d", parsed.Error.HTTPStatus)
	}
}

// TestHandleExecStreamRejectsOversizeBeforeUpstream 验证超限在入口即被拦。
//
// 关键断言是「没有发出握手请求」：若先连 WS、再提交超长内容，上游已经
// 白跑一轮，而且失败发生在后台协程里，状态码无法再回传给宿主。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，入口拦截回归。
func TestHandleExecStreamRejectsOversizeBeforeUpstream(t *testing.T) {
	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {})
	// 一旦有上游请求就记下来，用于断言「根本没发」。
	upstreamCalls := 0
	env.doer.handler = func(*http.Request, []byte) (*http.Response, error) {
		upstreamCalls++
		return jsonResponse(200, `{}`), nil
	}

	payload, errMarshal := json.Marshal(map[string]any{
		"model":  "coze/doubao-seed-2-0-lite-260215",
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": strings.Repeat("x", clawMaxContentBytes+1)},
		},
	})
	if errMarshal != nil {
		t.Fatalf("构造请求失败：%v", errMarshal)
	}
	raw, errMarshal := json.Marshal(executorStreamRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Payload:     payload,
			StorageJSON: storageJSONForTest(t),
		},
		StreamID: "sid-1",
	})
	if errMarshal != nil {
		t.Fatalf("构造执行请求失败：%v", errMarshal)
	}

	_, errHandle := handleExecStream(raw)
	if errHandle == nil {
		t.Fatal("超限请求必须在入口失败")
	}
	if _, ok := errHandle.(statusCoder); !ok {
		t.Errorf("入口错误必须实现 StatusCode，实际类型 %T", errHandle)
	}
	if upstreamCalls != 0 {
		t.Errorf("超限请求不得触达上游，实际调用 %d 次", upstreamCalls)
	}
}

// storageJSONForTest 构造执行请求所需的账号存储字节。
//
// StorageJSON 是 []byte 字段，JSON 编码后是 base64 字符串；这里直接返回
// 原始字节交给结构体序列化，避免手工拼线上形态。
// [参数] t: 测试上下文。
// [返回] 原始账号 JSON。
// 最近修改时间：2026-10-19；改动原因：新增，测试夹具。
func storageJSONForTest(t *testing.T) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(StoredAuth{CookieHeader: "sessionid=x"})
	if errMarshal != nil {
		t.Fatalf("构造账号存储失败：%v", errMarshal)
	}
	return raw
}

// TestHandleExecExecuteRejectsOversizeBeforeUpstream 验证非流式入口同样预检。
//
// 非流式与流式共用同一段 prompt 折叠逻辑，但两条入口分别解析请求；
// 只在流式入口加护栏会让非流式继续把超长内容送到上游。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，非流式入口拦截。
func TestHandleExecExecuteRejectsOversizeBeforeUpstream(t *testing.T) {
	payload, errMarshal := json.Marshal(map[string]any{
		"model": "coze/doubao-seed-2-0-lite-260215",
		"messages": []map[string]any{
			{"role": "user", "content": strings.Repeat("x", clawMaxContentBytes+1)},
		},
	})
	if errMarshal != nil {
		t.Fatalf("构造请求失败：%v", errMarshal)
	}
	raw, errMarshal := json.Marshal(struct {
		Payload     []byte `json:"Payload"`
		StorageJSON []byte `json:"StorageJSON"`
	}{Payload: payload, StorageJSON: storageJSONForTest(t)})
	if errMarshal != nil {
		t.Fatalf("构造执行请求失败：%v", errMarshal)
	}

	_, errHandle := handleExecExecute(raw)
	if errHandle == nil {
		t.Fatal("超限请求必须在入口失败")
	}
	coder, ok := errHandle.(statusCoder)
	if !ok {
		t.Fatalf("入口错误必须实现 StatusCode，实际类型 %T", errHandle)
	}
	if coder.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Errorf("状态码 = %d，期望 413", coder.StatusCode())
	}
}
