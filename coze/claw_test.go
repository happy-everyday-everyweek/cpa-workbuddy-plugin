package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 本文件验证 claw HTTP 协议层。
//
// 用假 doer 而不是 httptest：这些用例关心的是「请求体字段对不对、
// 响应怎么解析、失败怎么判」，不关心真实网络；假 doer 能直接断言
// 出站请求体，比抓包更精确，也不依赖端口。

// fakeDoer 是 httpDoer 的测试实现。
type fakeDoer struct {
	// handler 按请求返回响应；测试用它构造各种上游形态。
	handler func(req *http.Request, body []byte) (*http.Response, error)
	// lastBody 记录最近一次请求体，供断言。
	lastBody []byte
	// calls 记录调用次数。
	calls int
}

// Do 实现 httpDoer 接口。
// [参数] req: 出站请求。
// [返回] 假响应与错误。
// 最近修改时间：2026-10-18；改动原因：新增，测试替身。
func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.calls++
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		f.lastBody = raw
		req.Body = io.NopCloser(bytes.NewReader(raw))
	}
	return f.handler(req, f.lastBody)
}

// jsonResponse 构造一个 JSON 响应。
// [参数] status: HTTP 状态码；body: 响应体。
// [返回] 响应对象。
// 最近修改时间：2026-10-18；改动原因：新增，测试辅助。
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// newTestClawClient 构造一个注入假 doer 的 claw 客户端。
// [参数] doer: 假 HTTP 实现。
// [返回] 客户端。
// 最近修改时间：2026-10-18；改动原因：新增，测试辅助。
func newTestClawClient(doer httpDoer) *clawClient {
	return &clawClient{client: doer, cookieHeader: "sessionid=test", baseURL: cozeSiteHost}
}

// TestFetchClawID 验证 claw_id 提取与请求体。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，对话链路第一步。
func TestFetchClawID(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, clawInfoPath) {
			t.Errorf("请求路径 = %q，期望以 %q 结尾", req.URL.Path, clawInfoPath)
		}
		// body 必须是 `{}`：空 body 会被上游判为参数错误。
		if strings.TrimSpace(string(body)) != "{}" {
			t.Errorf("请求体 = %q，期望 {}", string(body))
		}
		if req.Header.Get("Cookie") != "sessionid=test" {
			t.Errorf("缺少会话 Cookie")
		}
		return jsonResponse(200, `{"code":0,"msg":"","data":{"user_claw_info":{"claw_id":"7697639566446149898","name":"扣子"}}}`), nil
	}}

	client := newTestClawClient(doer)
	clawID, err := client.fetchClawID()
	if err != nil {
		t.Fatalf("取 claw_id 失败: %v", err)
	}
	if clawID != "7697639566446149898" {
		t.Errorf("claw_id = %q，期望 7697639566446149898", clawID)
	}
}

// TestFetchClawIDRejectsBusinessError 验证 code!=0 被判为失败。
//
// 所有 claw 端点都是 HTTP 200 + code 表达业务结果；
// 只看状态码会把「参数错误」当成成功。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，业务错误判定。
func TestFetchClawIDRejectsBusinessError(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		return jsonResponse(200, `{"code":1000001,"msg":"参数错误","data":{}}`), nil
	}}

	client := newTestClawClient(doer)
	if _, err := client.fetchClawID(); err == nil {
		t.Fatal("code=1000001 应当报错，实际成功")
	}
}

// TestFetchMainSession 验证主会话提取。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，对话链路第二步。
func TestFetchMainSession(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("请求体不是 JSON: %v", err)
		}
		if payload["claw_id"] != "c1" {
			t.Errorf("claw_id = %v，期望 c1", payload["claw_id"])
		}
		return jsonResponse(200, `{"code":0,"data":{"session":{"id":"7697640660014383411","claw_id":"c1","status":2}}}`), nil
	}}

	client := newTestClawClient(doer)
	sessionID, err := client.fetchMainSession("c1")
	if err != nil {
		t.Fatalf("取主会话失败: %v", err)
	}
	if sessionID != "7697640660014383411" {
		t.Errorf("session_id = %q", sessionID)
	}
}

// TestSendChat 验证消息提交与 message_id 提取。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，消息提交契约。
func TestSendChat(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("请求体不是 JSON: %v", err)
		}
		if payload["session_id"] != "s1" || payload["content"] != "你好" {
			t.Errorf("请求体字段不符: %v", payload)
		}
		return jsonResponse(200, `{"code":0,"data":{"message_id":"mid-1"}}`), nil
	}}

	client := newTestClawClient(doer)
	messageID, err := client.sendChat("s1", "你好")
	if err != nil {
		t.Fatalf("提交消息失败: %v", err)
	}
	if messageID != "mid-1" {
		t.Errorf("message_id = %q，期望 mid-1", messageID)
	}
}

// TestListMessagesParsesReply 验证回复消息的内层 JSON 解析。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，落库格式契约。
func TestListMessagesParsesReply(t *testing.T) {
	inner := `{"id":"m1","session_id":"s1","message_type":"reply",` +
		`"message_data":{"reply":{"content":"收到","reply_to_message_id":"mid-1","stream":false}},` +
		`"turn_id":"t1"}`
	escaped, _ := json.Marshal(inner)
	body := `{"code":0,"data":{"messages":[` +
		`{"id":"m1","source":2,"message_type":"reply","content":` + string(escaped) + `}]}}`

	doer := &fakeDoer{handler: func(req *http.Request, _ []byte) (*http.Response, error) {
		return jsonResponse(200, body), nil
	}}

	client := newTestClawClient(doer)
	messages, err := client.listMessages("s1")
	if err != nil {
		t.Fatalf("拉取消息失败: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("消息数 = %d，期望 1", len(messages))
	}

	reply, found := findReplyFor(messages, "mid-1")
	if !found {
		t.Fatal("未找到配对回复")
	}
	if reply.MessageData.Reply.Content != "收到" {
		t.Errorf("回复正文 = %q，期望 收到", reply.MessageData.Reply.Content)
	}
}

// TestFindReplyForIgnoresWrongPair 验证不会错配到其它轮次。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，配对隔离守卫。
func TestFindReplyForIgnoresWrongPair(t *testing.T) {
	inner := `{"message_data":{"reply":{"content":"别的轮次","reply_to_message_id":"mid-OTHER","stream":false}}}`
	messages := []clawMessage{{ID: "m2", MessageType: "reply", Content: inner}}

	if _, found := findReplyFor(messages, "mid-1"); found {
		t.Fatal("不应配对到其它轮次的回复")
	}
}

// TestFindReplyForSkipsMalformedContent 验证坏 JSON 被跳过而不是崩溃。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，脏数据守卫。
func TestFindReplyForSkipsMalformedContent(t *testing.T) {
	messages := []clawMessage{
		{ID: "bad", MessageType: "reply", Content: "not-json"},
		{ID: "good", MessageType: "reply", Content: `{"message_data":{"reply":{"reply_to_message_id":"mid-1","content":"ok"}}}`},
	}

	reply, found := findReplyFor(messages, "mid-1")
	if !found {
		t.Fatal("应跳过坏 JSON 并找到有效回复")
	}
	if reply.MessageData.Reply.Content != "ok" {
		t.Errorf("回复正文 = %q", reply.MessageData.Reply.Content)
	}
}

// TestFetchFrontierKey 验证 WS 凭证提取。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，WS 前置契约。
func TestFetchFrontierKey(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Errorf("方法 = %q，期望 GET", req.Method)
		}
		return jsonResponse(200, `{"code":0,"data":{"access_key":"key32","device_id":"dev19"}}`), nil
	}}

	client := newTestClawClient(doer)
	key, err := client.fetchFrontierKey()
	if err != nil {
		t.Fatalf("取凭证失败: %v", err)
	}
	if key.AccessKey != "key32" || key.DeviceID != "dev19" {
		t.Errorf("凭证 = %+v", key)
	}
}

// TestFetchFrontierKeyRejectsEmptyFields 验证空凭证被拒绝。
//
// 空 access_key 连上去只会握手后立刻被断开，
// 在入口处失败比在 WS 层超时更容易定位。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，空凭证守卫。
func TestFetchFrontierKeyRejectsEmptyFields(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"data":{"access_key":"","device_id":"dev19"}}`), nil
	}}

	client := newTestClawClient(doer)
	if _, err := client.fetchFrontierKey(); err == nil {
		t.Fatal("空 access_key 应当报错")
	}
}

// TestPostJSONRejectsHTTPError 验证非 2xx 被判为失败。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，传输层错误守卫。
func TestPostJSONRejectsHTTPError(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		return jsonResponse(502, `{"code":0,"msg":""}`), nil
	}}

	client := newTestClawClient(doer)
	if _, err := client.fetchClawID(); err == nil {
		t.Fatal("HTTP 502 应当报错")
	}
}

// TestPostJSONRejectsMalformedBody 验证非 JSON 响应被拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，脏响应守卫。
func TestPostJSONRejectsMalformedBody(t *testing.T) {
	doer := &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		return jsonResponse(200, `<html>登录页</html>`), nil
	}}

	client := newTestClawClient(doer)
	if _, err := client.fetchClawID(); err == nil {
		t.Fatal("非 JSON 响应应当报错")
	}
}
