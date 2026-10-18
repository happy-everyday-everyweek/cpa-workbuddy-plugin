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

// TestParseFrontierMessageExtractsTopLevelFiles 验证顶层 files 里的图片被收取。
//
// 这是 2026-10-18 真实账号实测的生图返回形态：content_v2 的
// agent_message_event 只有一个 text 分片（正文是 `[图片](computer:///...)`
// 占位符），图片地址在**载荷顶层 files 数组**里，形状与 message/list 一致。
// 只解析 content_v2 的 image/file 分片会永远取不到图。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，真实生图取址契约。
func TestParseFrontierMessageExtractsTopLevelFiles(t *testing.T) {
	contentV2 := `{"session":{"phase":"event_final","seq_num":"12","turn_id":"t1","final":{` +
		`"type":"agent.message","agent_message_event":{"content":[` +
		`{"type":"text","text":{"content":"[图片](computer:///Coze/Drive/扣子/image/generate_x.jpg)"}}` +
		`]}}}}`
	payload := `{"id":"m1","message_type":"reply","files":[` +
		`{"file_name":"/Coze/Drive/扣子/image/generate_x.jpg",` +
		`"file_uri":"3735510093922464_0-drive/2372/image/generate_x.jpg",` +
		`"file_url":"https://static.coze.site/generate_x.jpg?sign=X",` +
		`"thumb_url":"https://static.coze.site/generate_x.jpg?sign=T",` +
		`"is_reference":0}],` +
		`"content_v2":` + mustJSONString(t, contentV2) + `}`

	msg, found := parseFrontierMessage([]byte(payload))
	if !found {
		t.Fatal("应当命中 reply")
	}
	if len(msg.Images) != 1 {
		t.Fatalf("图片数 = %d，期望 1（来自顶层 files）", len(msg.Images))
	}
	if msg.Images[0] != "https://static.coze.site/generate_x.jpg?sign=X" {
		t.Errorf("图片地址 = %q，应取 file_url 而不是 thumb_url", msg.Images[0])
	}
	// 正文里的 computer:// 占位符必须原样保留，不能被误当成图片地址。
	if !strings.Contains(msg.Snapshot, "computer://") {
		t.Errorf("正文占位符丢失：%q", msg.Snapshot)
	}
}

// TestParseFrontierMessageImagesDedupeTopLevelAndContentV2 验证两条来源去重。
//
// 上游可能同时在 content_v2 分片与顶层 files 给出同一张图；
// 两个分支都收会导致客户端看到重复图片。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，跨来源去重守卫。
func TestParseFrontierMessageImagesDedupeTopLevelAndContentV2(t *testing.T) {
	const imageURL = "https://static.coze.site/same.jpg?sign=S"
	contentV2 := `{"session":{"phase":"event_final","seq_num":"12","turn_id":"t1","final":{` +
		`"type":"agent.message","agent_message_event":{"content":[` +
		`{"type":"image","image":{"uri":"u","url":"` + imageURL + `"}}` +
		`]}}}}`
	payload := `{"id":"m1","message_type":"reply","files":[` +
		`{"file_name":"/Coze/Drive/x.jpg","file_url":"` + imageURL + `","is_reference":0}],` +
		`"content_v2":` + mustJSONString(t, contentV2) + `}`

	msg, found := parseFrontierMessage([]byte(payload))
	if !found {
		t.Fatal("应当命中 reply")
	}
	if len(msg.Images) != 1 {
		t.Fatalf("图片数 = %d，跨来源同一地址应只保留一张", len(msg.Images))
	}
}

// TestParseFrontierMessageExtractsImageFragments 验证图片分片被提取。
//
// 生图结果的图片以 image 分片出现在 final 的 content 数组里，
// 结构为 {uri, url}；不解析它，用户只看到占位文本而拿不到图。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，图片分片契约。
func TestParseFrontierMessageExtractsImageFragments(t *testing.T) {
	contentV2 := `{"session":{"phase":"event_final","seq_num":"9","turn_id":"t1","final":{` +
		`"type":"agent.message","agent_message_event":{"content":[` +
		`{"type":"text","text":{"content":"已生成图片"}},` +
		`{"type":"image","image":{"uri":"3735_0-drive/2372/image/a.jpg","url":"https://static.coze.site/a.jpg?sign=X"}}` +
		`]}}}}`
	payload := `{"id":"m1","message_type":"reply","content_v2":` + mustJSONString(t, contentV2) + `}`

	msg, found := parseFrontierMessage([]byte(payload))
	if !found {
		t.Fatal("应当命中 reply")
	}
	if len(msg.Images) != 1 {
		t.Fatalf("图片数 = %d，期望 1", len(msg.Images))
	}
	if msg.Images[0] != "https://static.coze.site/a.jpg?sign=X" {
		t.Errorf("图片地址 = %q", msg.Images[0])
	}
	// 文本快照必须不受图片分片影响，否则正文会混入非文本内容。
	if msg.Snapshot != "已生成图片" {
		t.Errorf("snapshot = %q，图片分片不应进入正文", msg.Snapshot)
	}
}

// TestParseFrontierMessageAcceptsFileFragment 验证 file 分片同样被收取。
//
// 扣子把用户上传的参考图标成 file、把生图产出标成 image，但两者
// 共用 {uri, url} 形状；线上出现过混用，只认 image 会漏图。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，file 分片兼容。
func TestParseFrontierMessageAcceptsFileFragment(t *testing.T) {
	contentV2 := `{"session":{"phase":"event_final","seq_num":"10","turn_id":"t1","final":{` +
		`"type":"agent.message","agent_message_event":{"content":[` +
		`{"type":"file","file":{"uri":"u1","url":"https://static.coze.site/b.jpg?sign=Y"}}` +
		`]}}}}`
	payload := `{"id":"m1","message_type":"reply","content_v2":` + mustJSONString(t, contentV2) + `}`

	msg, found := parseFrontierMessage([]byte(payload))
	if !found {
		t.Fatal("应当命中 reply")
	}
	if len(msg.Images) != 1 || msg.Images[0] != "https://static.coze.site/b.jpg?sign=Y" {
		t.Errorf("file 分片未收取，Images = %#v", msg.Images)
	}
}

// TestParseFrontierMessageSkipsEmptyImageURL 验证无 url 的分片被跳过。
//
// uri 是云盘内部标识，不能直接下载；只有 url 可用。
// 空 url 的分片若被收进来，客户端会拿到一个空图片地址。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，空地址守卫。
func TestParseFrontierMessageSkipsEmptyImageURL(t *testing.T) {
	contentV2 := `{"session":{"phase":"event_final","seq_num":"11","turn_id":"t1","final":{` +
		`"type":"agent.message","agent_message_event":{"content":[` +
		`{"type":"image","image":{"uri":"only-uri","url":""}}` +
		`]}}}}`
	payload := `{"id":"m1","message_type":"reply","content_v2":` + mustJSONString(t, contentV2) + `}`

	msg, found := parseFrontierMessage([]byte(payload))
	if !found {
		t.Fatal("应当命中 reply")
	}
	if len(msg.Images) != 0 {
		t.Errorf("空 url 应被跳过，Images = %#v", msg.Images)
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

// TestParseFinalFrameDoesNotDuplicate 验证 event_final 不把全文当增量。
//
// event_final 帧的 content 装的是累积全文；若当 delta 追加，
// 客户端会看到答案完整重复一遍（2026-10-18 生产实测：末帧 578 字
// == 前文拼接，全文 1156 字）。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，正文重复缺陷回归。
func TestParseFinalFrameDoesNotDuplicate(t *testing.T) {
	inner := `{"id":"m1","message_type":"reply","message_data":{"reply":{"content":"完整答案","reply_to_message_id":"u1","stream":true}},"turn_id":"t1"}`
	innerV2 := `{"source":0,"session":{"phase":"event_final","seq_num":"9","turn_id":"t1","final":{"type":"agent.message","agent_message_event":{"content":[{"type":"text","text":{"content":"完整答案"}}]}}}}`
	payload, errMarshal := json.Marshal(map[string]any{
		"id":           "m1",
		"message_type": "reply",
		"content":      inner,
		"content_v2":   innerV2,
	})
	if errMarshal != nil {
		t.Fatalf("构造载荷失败：%v", errMarshal)
	}

	msg, found := parseFrontierMessage(payload)
	if !found {
		t.Fatal("应当命中 reply 帧")
	}
	if msg.Delta != "" {
		t.Errorf("final 帧的 delta 必须为空，实际 %q", msg.Delta)
	}
	if msg.Snapshot != "完整答案" {
		t.Errorf("快照 = %q", msg.Snapshot)
	}
	if !msg.isFinal() {
		t.Error("应当判定为收尾帧")
	}
}

// TestParseDeltaFrameKeepsDelta 验证普通增量帧仍然保留 delta。
//
// 修复不能伤及正常路径：event_delta 的 content 就是增量。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，增量路径守卫。
func TestParseDeltaFrameKeepsDelta(t *testing.T) {
	inner := `{"id":"m1","message_type":"reply","message_data":{"reply":{"content":"片段","reply_to_message_id":"u1","stream":true}},"turn_id":"t1"}`
	innerV2 := `{"session":{"phase":"event_delta","seq_num":"8","turn_id":"t1"}}`
	payload, _ := json.Marshal(map[string]any{
		"id":           "m1",
		"message_type": "reply",
		"content":      inner,
		"content_v2":   innerV2,
	})

	msg, found := parseFrontierMessage(payload)
	if !found {
		t.Fatal("应当命中 reply 帧")
	}
	if msg.Delta != "片段" {
		t.Errorf("增量 = %q，期望 片段", msg.Delta)
	}
}
