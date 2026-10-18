package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 本文件验证 frontier WS 客户端。
//
// 用 httptest 起一个真实的 WS 服务端，而不是 mock gorilla：
// 心跳、帧编解码、事件分发三者耦合在连接生命周期里，
// 只 mock 其中一层无法证明「真实字节能被正确拆成事件」。

// wsTestServer 是一个可编排的 frontier 假服务端。
type wsTestServer struct {
	server *httptest.Server
	// onConnect 在握手完成后调用，测试用它决定推送什么帧。
	onConnect func(conn *websocket.Conn)
	// gotSubprotocol 记录客户端协商的子协议。
	gotSubprotocol string
	// gotQuery 记录客户端连接参数。
	gotQuery url.Values
	// gotCookie 记录握手时的 Cookie 头。
	gotCookie string
	mu        sync.Mutex
	conns     int
}

// newWSTestServer 启动一个假 frontier 服务端。
// [参数] t: 测试上下文；onConnect: 连接建立后的行为。
// [返回] 服务端与清理函数。
// 最近修改时间：2026-10-18；改动原因：新增，WS 测试夹具。
func newWSTestServer(t *testing.T, onConnect func(conn *websocket.Conn)) *wsTestServer {
	t.Helper()
	ts := &wsTestServer{onConnect: onConnect}
	upgrader := websocket.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{frontierWSProtocol},
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.conns++
		ts.gotSubprotocol = r.Header.Get("Sec-WebSocket-Protocol")
		ts.gotQuery = r.URL.Query()
		ts.gotCookie = r.Header.Get("Cookie")
		ts.mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if ts.onConnect != nil {
			ts.onConnect(conn)
		}
		// 保持连接直到客户端关闭：读循环会在这里阻塞。
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

// newTestFrontierClient 构造指向假服务端的客户端。
// [参数] ts: 假服务端；onEvent: 事件回调。
// [返回] 客户端。
// 最近修改时间：2026-10-18；改动原因：新增，测试夹具。
func newTestFrontierClient(ts *wsTestServer, onEvent func(frontierEvent)) *frontierClient {
	client := newFrontierClient(
		&frontierKeyResponse{AccessKey: "test-key", DeviceID: "test-device"},
		"sessionid=test",
		onEvent,
	)
	client.logf = func(level, message string, fields map[string]any) {}
	// 把 wss 换成 httptest 的 ws 地址。
	client.dialer = &websocket.Dialer{}
	client.serverURL = ts.server.URL
	// 测试里缩短读超时：生产 20 秒是为容忍上游抖动，
	// 测试等不起，也不需要等。
	client.readTimeout = 500 * time.Millisecond
	return client
}

// TestFrontierURLContainsClawParams 验证连接参数用 claw 那一套。
//
// 参数错了会连上但收不到业务帧（aid=578287 是另一条通道），
// 这是本轮踩过的坑，必须有断言守住。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，参数契约守卫。
func TestFrontierURLContainsClawParams(t *testing.T) {
	client := newFrontierClient(&frontierKeyResponse{AccessKey: "k", DeviceID: "d"}, "", nil)
	raw := client.url()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL 解析失败: %v", err)
	}
	query := parsed.Query()

	expected := map[string]string{
		"aid":             frontierAppID,
		"fpid":            frontierProductID,
		"xsack":           "0",
		"access_key":      "k",
		"device_id":       "d",
		"device_platform": "web",
		"qos_sdk_version": "2",
	}
	for key, want := range expected {
		if got := query.Get(key); got != want {
			t.Errorf("参数 %s = %q，期望 %q", key, got, want)
		}
	}
	if strings.Contains(raw, "578287") {
		t.Error("URL 不应包含 fullstack 通道的 app_id 578287")
	}
	if parsed.Scheme != "wss" {
		t.Errorf("scheme = %q，期望 wss", parsed.Scheme)
	}
}

// TestFrontierHandshakeCarriesCookie 验证 WS 握手携带会话 Cookie。
//
// 不带 Cookie 时网关照样返回 101，但只推 hi 心跳、不给任何业务帧，
// 表现为「连上了却收不到回复」，只能等首字超时走轮询兜底。
// 2026-10-18 生产对照实验：不带 6 帧全 hi；带上 25 帧含 21 条业务帧。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，握手凭证契约守卫。
func TestFrontierHandshakeCarriesCookie(t *testing.T) {
	ts := newWSTestServer(t, func(conn *websocket.Conn) {
		time.Sleep(200 * time.Millisecond)
	})

	client := newTestFrontierClient(ts, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go client.Run(ctx)

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		ts.mu.Lock()
		got := ts.gotCookie
		ts.mu.Unlock()
		if got != "" {
			if got != "sessionid=test" {
				t.Errorf("Cookie = %q，期望 sessionid=test", got)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("握手未携带 Cookie：上游只会推心跳，收不到业务帧")
}

// TestFrontierConnectsWithSubprotocol 验证握手使用 pbbp2 子协议。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，子协议守卫。
func TestFrontierConnectsWithSubprotocol(t *testing.T) {
	ts := newWSTestServer(t, func(conn *websocket.Conn) {
		time.Sleep(200 * time.Millisecond)
	})

	client := newTestFrontierClient(ts, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go client.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ts.mu.Lock()
		got := ts.gotSubprotocol
		ts.mu.Unlock()
		if got != "" {
			if got != frontierWSProtocol {
				t.Errorf("子协议 = %q，期望 %q", got, frontierWSProtocol)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("连接未建立")
}

// TestFrontierDispatchesClawFrames 验证业务帧被解析成事件。
//
// 服务端推送真实结构的帧（service=33555657 + payloadType=message），
// 客户端应回调一次且载荷完整。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，端到端帧分发。
func TestFrontierDispatchesClawFrames(t *testing.T) {
	payload := `{"id":"m1","session_id":"s1","message_type":"reply",` +
		`"content":"{\"message_data\":{\"reply\":{\"content\":\"hi\",\"reply_to_message_id\":\"mid-1\",\"stream\":true}}}",` +
		`"content_v2":"{\"session\":{\"phase\":\"event_delta\",\"seq_num\":\"8\",\"turn_id\":\"t1\"}}"}`

	frame := encodeFrontierFrame(&frontierFrame{
		Service:     frontierServiceClaw,
		Method:      0,
		PayloadType: "message",
		Payload:     []byte(payload),
	})

	events := make(chan frontierEvent, 4)
	ts := newWSTestServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.BinaryMessage, frame)
		time.Sleep(300 * time.Millisecond)
	})

	client := newTestFrontierClient(ts, func(ev frontierEvent) {
		events <- ev
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go client.Run(ctx)

	select {
	case ev := <-events:
		if ev.PayloadType != "message" {
			t.Errorf("payloadType = %q", ev.PayloadType)
		}
		msg, found := parseFrontierMessage(ev.Payload)
		if !found {
			t.Fatal("未能解析出 reply 消息")
		}
		if msg.Delta != "hi" {
			t.Errorf("delta = %q，期望 hi", msg.Delta)
		}
		if msg.Phase != "event_delta" {
			t.Errorf("phase = %q，期望 event_delta", msg.Phase)
		}
		if msg.SeqNum != "8" {
			t.Errorf("seq_num = %q，期望 8", msg.SeqNum)
		}
		if msg.ReplyToMessageID != "mid-1" {
			t.Errorf("reply_to = %q，期望 mid-1", msg.ReplyToMessageID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到事件")
	}
}

// TestFrontierIgnoresOtherServices 验证非 claw service 的帧被丢弃。
//
// 同一连接上还有控制帧；把它们当业务事件会污染输出。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，service 过滤守卫。
func TestFrontierIgnoresOtherServices(t *testing.T) {
	frame := encodeFrontierFrame(&frontierFrame{
		Service:     999999,
		PayloadType: "message",
		Payload:     []byte(`{"message_type":"reply"}`),
	})

	events := make(chan frontierEvent, 4)
	ts := newWSTestServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.BinaryMessage, frame)
		time.Sleep(400 * time.Millisecond)
	})

	client := newTestFrontierClient(ts, func(ev frontierEvent) { events <- ev })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go client.Run(ctx)

	select {
	case ev := <-events:
		t.Fatalf("不应收到其它 service 的事件: %+v", ev)
	case <-time.After(600 * time.Millisecond):
	}
}

// TestFrontierIgnoresTextFrames 验证文本心跳不被当作业务帧。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，心跳帧隔离。
func TestFrontierIgnoresTextFrames(t *testing.T) {
	events := make(chan frontierEvent, 4)
	ts := newWSTestServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("hi"))
		time.Sleep(400 * time.Millisecond)
	})

	client := newTestFrontierClient(ts, func(ev frontierEvent) { events <- ev })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go client.Run(ctx)

	select {
	case ev := <-events:
		t.Fatalf("文本帧不应产生事件: %+v", ev)
	case <-time.After(600 * time.Millisecond):
	}
}

// TestParseFrontierMessageSkipsNonReply 验证非 reply 帧被跳过。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，消息类型过滤。
func TestParseFrontierMessageSkipsNonReply(t *testing.T) {
	cases := []string{
		`{"claw_id":"c","session_id":"s","progress":1}`,
		`{"message_type":"session.status_running"}`,
		`{"message_type":"agent.thinking"}`,
	}
	for _, payload := range cases {
		if _, found := parseFrontierMessage([]byte(payload)); found {
			t.Errorf("非 reply 帧不应命中: %s", payload)
		}
	}
}

// TestParseFrontierMessageFinalSnapshot 验证 event_final 的累积全文提取。
//
// final 的 agent_message_event.content 是数组，只取 type=text 的项，
// 否则会把卡片类内容拼进正文。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，收尾快照契约。
func TestParseFrontierMessageFinalSnapshot(t *testing.T) {
	contentV2 := `{"session":{"phase":"event_final","seq_num":"17","turn_id":"t1","final":{` +
		`"type":"agent.message","agent_message_event":{"content":[` +
		`{"type":"text","text":{"content":"完整回复"}},` +
		`{"type":"card","text":{"content":"不应拼接"}}]}}}}`
	payload := `{"id":"m1","message_type":"reply","content_v2":` + mustJSONString(t, contentV2) + `}`

	msg, found := parseFrontierMessage([]byte(payload))
	if !found {
		t.Fatal("应当命中 reply")
	}
	if !msg.isFinal() {
		t.Errorf("phase = %q，期望 event_final", msg.Phase)
	}
	if msg.Snapshot != "完整回复" {
		t.Errorf("snapshot = %q，期望只含 text 项", msg.Snapshot)
	}
}

// TestDedupeKeyIncludesSeqNum 验证去重键含序号。
//
// 同一轮的 event_start 与首个 event_delta 共享 id，
// 仅按 id 去重会丢掉真正的首个增量。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，去重键守卫。
func TestDedupeKeyIncludesSeqNum(t *testing.T) {
	a := &frontierMessage{ID: "same", TurnID: "t1", SeqNum: "7", Phase: "event_start"}
	b := &frontierMessage{ID: "same", TurnID: "t1", SeqNum: "8", Phase: "event_delta"}

	if a.dedupeKey() == b.dedupeKey() {
		t.Error("同 id 不同序号应产生不同去重键")
	}
}

// TestFrontierReconnectsAfterClose 验证连接断开后会重连。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，重连守卫。
func TestFrontierReconnectsAfterClose(t *testing.T) {
	var mu sync.Mutex
	connects := 0
	ts := newWSTestServer(t, func(conn *websocket.Conn) {
		mu.Lock()
		connects++
		n := connects
		mu.Unlock()
		// 首次连接立即关闭，逼客户端重连。
		if n == 1 {
			_ = conn.Close()
			return
		}
		time.Sleep(600 * time.Millisecond)
	})

	client := newTestFrontierClient(ts, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go client.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := connects
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("断开后未重连")
}

// mustJSONString 把字符串编码成 JSON 字符串字面量。
// [参数] t: 测试上下文；s: 原始字符串。
// [返回] 带引号的 JSON 字符串。
// 最近修改时间：2026-10-18；改动原因：新增，测试辅助。
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("JSON 编码失败: %v", err)
	}
	return string(raw)
}
