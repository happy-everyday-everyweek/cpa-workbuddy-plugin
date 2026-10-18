package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// errTestEmit 是测试用的 emit 错误哨兵。
var errTestEmit = errors.New("emit failed")

// pluginapiRequest 构造一个只填请求体的 ExecutorRequest。
// [参数] original: 原始请求体；payload: 翻译后请求体。
// [返回] 执行请求。
// 最近修改时间：2026-10-18；改动原因：新增，测试辅助。
func pluginapiRequest(original, payload string) pluginapi.ExecutorRequest {
	req := pluginapi.ExecutorRequest{}
	if original != "" {
		req.OriginalRequest = []byte(original)
	}
	if payload != "" {
		req.Payload = []byte(payload)
	}
	return req
}

// 本文件验证 executor 的端到端链路。
//
// 用真实 WS 服务端 + 假 HTTP doer：WS 层必须是真的（帧编解码、心跳、
// 分发都在里面），HTTP 层用假实现可以直接控制返回时序，
// 也能断言请求体字段。

// executorTestEnv 是一次 executor 测试的环境。
type executorTestEnv struct {
	ws   *wsTestServer
	doer *fakeDoer
	// chatAt 记录 message/chat 被调用的时刻，用于验证
	// 「WS 先连、再提交」的顺序。
	chatAt time.Time
	mu     sync.Mutex
}

// newExecutorTestEnv 构造测试环境。
//
// onWSConnect 决定 WS 建立后推送哪些帧；HTTP 端点固定返回
// 握手三步所需的最小响应。
// [参数] t: 测试上下文；onWSConnect: WS 行为。
// [返回] 环境。
// 最近修改时间：2026-10-18；改动原因：新增，端到端夹具。
func newExecutorTestEnv(t *testing.T, onWSConnect func(conn *websocket.Conn)) *executorTestEnv {
	t.Helper()
	env := &executorTestEnv{}

	env.ws = newWSTestServer(t, onWSConnect)
	env.doer = &fakeDoer{handler: func(req *http.Request, body []byte) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, clawInfoPath):
			return jsonResponse(200, `{"code":0,"data":{"user_claw_info":{"claw_id":"c1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMainSessionPath):
			return jsonResponse(200, `{"code":0,"data":{"session":{"id":"s1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, frontierKeyPath):
			return jsonResponse(200, `{"code":0,"data":{"access_key":"k","device_id":"d"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawChatPath):
			env.mu.Lock()
			env.chatAt = time.Now()
			env.mu.Unlock()
			return jsonResponse(200, `{"code":0,"data":{"message_id":"mid-1"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMessageListPath):
			return jsonResponse(200, `{"code":0,"data":{"messages":[]}}`), nil
		default:
			t.Errorf("未预期的请求路径: %s", req.URL.Path)
			return jsonResponse(404, `{}`), nil
		}
	}}
	return env
}

// newExecutor 构造指向测试环境的 executor。
// [参数] env: 测试环境。
// [返回] executor。
// 最近修改时间：2026-10-18；改动原因：新增，测试辅助。
func (env *executorTestEnv) newExecutor() *cozeExecutor {
	exec := newCozeExecutor(&StoredAuth{CookieHeader: "sessionid=x"}, "coze/m", env.doer)
	return exec
}

// replyFrame 构造一条 reply 帧载荷。
// [参数] phase: 阶段；seq: 序号；delta: 增量；snapshot: 累积全文。
// [返回] 帧载荷 JSON。
// 最近修改时间：2026-10-18；改动原因：新增，测试夹具。
func replyFrame(phase, seq, delta, snapshot string) []byte {
	deltaJSON, _ := json.Marshal(delta)
	content := `{"message_data":{"reply":{"content":` + string(deltaJSON) +
		`,"reply_to_message_id":"mid-1","stream":true}},"turn_id":"t1"}`
	contentV2 := `{"session":{"phase":"` + phase + `","seq_num":"` + seq + `","turn_id":"t1"`
	if snapshot != "" {
		snapshotJSON, _ := json.Marshal(snapshot)
		contentV2 += `,"final":{"type":"agent.message","agent_message_event":{"content":[{"type":"text","text":{"content":` +
			string(snapshotJSON) + `}}]}}`
	}
	contentV2 += `}}`

	payload := map[string]any{
		"id":           "m1",
		"session_id":   "s1",
		"message_type": "reply",
		"content":      content,
		"content_v2":   contentV2,
	}
	raw, _ := json.Marshal(payload)
	return raw
}

// TestExecutorStreamsDeltas 验证增量被实时下发。
//
// 服务端依次推送 start / delta / delta / final，
// 期望 emit 收到两段非空增量，最终正文等于拼接结果。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，流式主链路。
func TestExecutorStreamsDeltas(t *testing.T) {
	frames := [][]byte{
		replyFrame("event_start", "7", "", ""),
		replyFrame("event_delta", "8", "第一段", ""),
		replyFrame("event_delta", "9", "第二段", ""),
		replyFrame("event_final", "10", "", "第一段第二段"),
	}

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		for _, payload := range frames {
			frame := encodeFrontierFrame(&frontierFrame{
				Service:     frontierServiceClaw,
				PayloadType: "message",
				Payload:     payload,
			})
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			time.Sleep(60 * time.Millisecond)
		}
	})

	exec := env.newExecutor()
	exec.client.baseURL = cozeSiteHost
	// 让 executor 连测试 WS：直接替换 URL 构造。
	exec.frontierServerURL = env.ws.server.URL

	var mu sync.Mutex
	var emitted []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	content, err := exec.runStream(ctx, "你好", func(delta string) error {
		mu.Lock()
		emitted = append(emitted, delta)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if content != "第一段第二段" {
		t.Errorf("正文 = %q，期望 第一段第二段", content)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(emitted) != 2 {
		t.Fatalf("emit 次数 = %d，期望 2（%v）", len(emitted), emitted)
	}
	if emitted[0] != "第一段" || emitted[1] != "第二段" {
		t.Errorf("emit 内容 = %v", emitted)
	}
}

// TestExecutorDeduplicatesFrames 验证重复帧被丢弃。
//
// 上游会重复投递同一帧；不去重会让正文出现重复片段。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，去重守卫。
func TestExecutorDeduplicatesFrames(t *testing.T) {
	delta := replyFrame("event_delta", "8", "内容", "")
	final := replyFrame("event_final", "9", "", "内容")

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		// 同一帧推两次，模拟上游重复投递。
		for _, payload := range [][]byte{delta, delta, final, final} {
			frame := encodeFrontierFrame(&frontierFrame{
				Service:     frontierServiceClaw,
				PayloadType: "message",
				Payload:     payload,
			})
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			time.Sleep(50 * time.Millisecond)
		}
	})

	exec := env.newExecutor()
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var emitted []string
	content, err := exec.runStream(ctx, "你好", func(d string) error {
		emitted = append(emitted, d)
		return nil
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if content != "内容" {
		t.Errorf("正文 = %q，期望 内容（不应重复）", content)
	}
	if len(emitted) != 1 {
		t.Errorf("emit 次数 = %d，期望 1（%v）", len(emitted), emitted)
	}
}

// TestExecutorFinalSnapshotFallback 验证只有 final 时也能拿到正文。
//
// 极端情况下增量帧全丢，只剩 event_final；此时必须用快照兜底，
// 而不是返回空字符串。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，快照兜底。
func TestExecutorFinalSnapshotFallback(t *testing.T) {
	final := replyFrame("event_final", "10", "", "只有快照的正文")

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		frame := encodeFrontierFrame(&frontierFrame{
			Service:     frontierServiceClaw,
			PayloadType: "message",
			Payload:     final,
		})
		_ = conn.WriteMessage(websocket.BinaryMessage, frame)
		time.Sleep(100 * time.Millisecond)
	})

	exec := env.newExecutor()
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	content, err := exec.runStream(ctx, "你好", func(string) error { return nil })
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if content != "只有快照的正文" {
		t.Errorf("正文 = %q，期望 只有快照的正文", content)
	}
}

// TestExecutorEmitErrorAborts 验证下游断开时立即中止。
//
// 客户端断开后继续拉上游是纯浪费，也会让账号被无谓消耗。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，中止守卫。
func TestExecutorEmitErrorAborts(t *testing.T) {
	delta := replyFrame("event_delta", "8", "内容", "")
	final := replyFrame("event_final", "9", "", "内容")

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		for _, payload := range [][]byte{delta, final} {
			frame := encodeFrontierFrame(&frontierFrame{
				Service:     frontierServiceClaw,
				PayloadType: "message",
				Payload:     payload,
			})
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			time.Sleep(60 * time.Millisecond)
		}
	})

	exec := env.newExecutor()
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	emitErr := errTestEmit
	_, err := exec.runStream(ctx, "你好", func(string) error { return emitErr })
	if err == nil {
		t.Fatal("emit 报错时 runStream 应当返回错误")
	}
	if !strings.Contains(err.Error(), "emit") {
		t.Errorf("错误未透传: %v", err)
	}
}

// TestExecutorIgnoresNonReplyFrames 验证非 reply 帧不影响正文。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，噪声帧隔离。
func TestExecutorIgnoresNonReplyFrames(t *testing.T) {
	noise := []byte(`{"claw_id":"c1","session_id":"s1","progress":1}`)
	delta := replyFrame("event_delta", "8", "正文", "")
	final := replyFrame("event_final", "9", "", "正文")

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		for _, payload := range [][]byte{noise, delta, noise, final} {
			frame := encodeFrontierFrame(&frontierFrame{
				Service:     frontierServiceClaw,
				PayloadType: "message",
				Payload:     payload,
			})
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			time.Sleep(50 * time.Millisecond)
		}
	})

	exec := env.newExecutor()
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	content, err := exec.runStream(ctx, "你好", func(string) error { return nil })
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if content != "正文" {
		t.Errorf("正文 = %q，期望 正文", content)
	}
}

// TestReplyCollectorTextPrefersDeltas 验证优先用增量拼接。
//
// 增量在流式过程中持续增长，快照只在收尾后才有值；
// 若优先快照，流式过程中会一直拿到空串。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，正文取值优先级。
func TestReplyCollectorTextPrefersDeltas(t *testing.T) {
	collector := newReplyCollector()
	collector.accept(&frontierMessage{TurnID: "t", SeqNum: "1", Phase: "event_delta", Delta: "A"})
	collector.accept(&frontierMessage{TurnID: "t", SeqNum: "2", Phase: "event_delta", Delta: "B"})

	if got := collector.text(); got != "AB" {
		t.Errorf("text() = %q，期望 AB", got)
	}
}

// TestReplyCollectorDedupeKey 验证同 id 不同序号不算重复。
//
// event_start 没有内容，accept 返回 false 是设计使然（无变化）；
// 关键是它不能被记为「已见过」而挡掉后续同 id 的 delta。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，去重边界。
func TestReplyCollectorDedupeKey(t *testing.T) {
	collector := newReplyCollector()
	collector.accept(&frontierMessage{ID: "same", TurnID: "t", SeqNum: "7", Phase: "event_start"})
	second := collector.accept(&frontierMessage{ID: "same", TurnID: "t", SeqNum: "8", Phase: "event_delta", Delta: "x"})

	if !second {
		t.Error("同 id 不同序号应被接受")
	}
	if got := collector.text(); got != "x" {
		t.Errorf("text() = %q，期望 x", got)
	}
}

// TestParseStoredAuthRejectsMissingCookie 验证缺 Cookie 被拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，账号守卫。
func TestParseStoredAuthRejectsMissingCookie(t *testing.T) {
	if _, err := parseStoredAuth(nil); err == nil {
		t.Error("空存储应当报错")
	}
	raw, _ := json.Marshal(StoredAuth{AuthID: "a1"})
	if _, err := parseStoredAuth(raw); err == nil {
		t.Error("缺 Cookie 应当报错")
	}

	valid, _ := json.Marshal(StoredAuth{AuthID: "a1", CookieHeader: "sessionid=x"})
	sa, err := parseStoredAuth(valid)
	if err != nil {
		t.Fatalf("有效账号解析失败: %v", err)
	}
	if sa.AuthID != "a1" {
		t.Errorf("auth_id = %q", sa.AuthID)
	}
}

// TestExecutorRequestBodyPreference 验证优先用 Payload。
//
// 宿主只对 Payload 做协议翻译；OriginalRequest 保留客户端原始请求。
// 走 /v1/responses 时原始体里只有 input，优先读它会报「请求缺少 messages」。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：修复 Responses 入口失败，优先级反转。
func TestExecutorRequestBodyPreference(t *testing.T) {
	if got := executorRequestBody(pluginapiRequest("orig", "payload")); string(got) != "payload" {
		t.Errorf("应当优先 Payload，实际 %q", got)
	}
	if got := executorRequestBody(pluginapiRequest("orig", "")); string(got) != "orig" {
		t.Errorf("应当回退 OriginalRequest，实际 %q", got)
	}
}

// TestExecutorResponsesEntryParsesTranslatedPayload 验证 Responses 入口可用。
//
// 生产实测（2026-10-18）：经 /v1/responses 或 new-api 转发时，客户端原始体
// 只有 input，翻译后的 messages 只在 Payload 里。旧实现优先读 OriginalRequest，
// 导致所有这类请求报「请求缺少 messages」，错误还会让宿主把账号标记不可用。
// 这里锁定契约：必须从 Payload 解析出 messages 并折叠成 prompt。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，Responses 入口缺陷回归。
func TestExecutorResponsesEntryParsesTranslatedPayload(t *testing.T) {
	req := pluginapiRequest(
		`{"model":"doubao-seed-2-0-lite","input":"你好"}`,
		`{"model":"doubao-seed-2-0-lite","messages":[{"role":"user","content":"你好"}]}`,
	)
	chatReq, errChat := parseChatRequest(executorRequestBody(req))
	if errChat != nil {
		t.Fatalf("应当从 Payload 解析成功，实际 %v", errChat)
	}
	prompt, errPrompt := buildClawPrompt(chatReq.Messages)
	if errPrompt != nil {
		t.Fatalf("折叠失败：%v", errPrompt)
	}
	if prompt != "你好" {
		t.Errorf("prompt = %q", prompt)
	}
}

// TestStreamResponseSerializable 验证流式响应能被 JSON 序列化。
//
// 生产实测（2026-10-18）：直接把 channel 塞进 ExecutorStreamResponse
// 会让宿主 RPC 报 `json: unsupported type: <-chan ...`，请求直接 500。
// 这里锁定契约：信封里的 Chunks 必须是 slice。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，生产缺陷回归。
func TestStreamResponseSerializable(t *testing.T) {
	resp := streamResponse{
		Headers: streamHeaders(),
		Chunks: []pluginapi.ExecutorStreamChunk{
			{Payload: []byte(`{"id":"c1"}`)},
			{Payload: []byte(`{"id":"c2"}`)},
		},
	}
	raw, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		t.Fatalf("流式响应必须可序列化，实际报错：%v", errMarshal)
	}
	var back struct {
		Chunks []pluginapi.ExecutorStreamChunk `json:"chunks"`
	}
	if errUnmarshal := json.Unmarshal(raw, &back); errUnmarshal != nil {
		t.Fatalf("回读失败：%v", errUnmarshal)
	}
	if len(back.Chunks) != 2 {
		t.Errorf("chunks 数量 = %d，期望 2", len(back.Chunks))
	}
	if got := string(back.Chunks[0].Payload); got != `{"id":"c1"}` {
		t.Errorf("首帧 = %q", got)
	}
}

// TestStreamHeadersCarryNoBufferHint 验证流式响应头关闭中间层缓冲。
//
// 缺 X-Accel-Buffering 时 nginx 会攒够缓冲才下发，
// 客户端表现为「卡住很久再一次性蹦出全部内容」。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，真流式前置条件。
func TestStreamHeadersCarryNoBufferHint(t *testing.T) {
	h := streamHeaders()
	if got := h.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q，期望 no", got)
	}
}

// TestWaitReadySignalsAfterHandshake 验证连接就绪信号。
//
// 之前调用方用固定 300ms 猜连接是否就绪；网络慢时 chat 会抢在连接
// 之前发出，快速回复的 event_delta 全部丢失，只剩重放的 event_final，
// 最终正文为空（2026-10-18 多轮场景实测）。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，就绪信号回归。
func TestWaitReadySignalsAfterHandshake(t *testing.T) {
	ts := newWSTestServer(t, func(conn *websocket.Conn) {
		time.Sleep(100 * time.Millisecond)
	})
	client := newTestFrontierClient(ts, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go client.Run(ctx)

	readyCtx, cancelReady := context.WithTimeout(ctx, 2*time.Second)
	defer cancelReady()
	if err := client.WaitReady(readyCtx); err != nil {
		t.Fatalf("连接应在握手后立即就绪：%v", err)
	}

	ts.mu.Lock()
	conns := ts.conns
	ts.mu.Unlock()
	if conns == 0 {
		t.Error("WaitReady 返回时应当已经建立过连接")
	}
}

// TestEmptyReplyFallsBackToPolling 验证「收尾但正文为空」时降级轮询。
//
// 场景：WS 只重放了 event_final、增量帧在连接就绪前就推完了。
// 直接返回会让客户端拿到空回复；必须去 message/list 按 message_id 取正文。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，空回复兜底回归。
func TestEmptyReplyFallsBackToPolling(t *testing.T) {
	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		// 只发收尾帧，不带任何正文——模拟增量全部错过。
		payload := replyFrame("event_final", "9", "", "")
		frame := encodeFrontierFrame(&frontierFrame{
			Service:     frontierServiceClaw,
			PayloadType: "message",
			Payload:     payload,
		})
		_ = conn.WriteMessage(websocket.BinaryMessage, frame)
	})
	// 轮询返回一条配对本轮 message_id 的完整回复。
	env.doer.handler = func(req *http.Request, body []byte) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, clawInfoPath):
			return jsonResponse(200, `{"code":0,"data":{"user_claw_info":{"claw_id":"c1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMainSessionPath):
			return jsonResponse(200, `{"code":0,"data":{"session":{"id":"s1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, frontierKeyPath):
			return jsonResponse(200, `{"code":0,"data":{"access_key":"k","device_id":"d"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawChatPath):
			return jsonResponse(200, `{"code":0,"data":{"message_id":"mid-1"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMessageListPath):
			return jsonResponse(200, `{"code":0,"data":{"messages":[{"message_type":"reply","content":"{\"message_data\":{\"reply\":{\"content\":\"轮询补回的正文\",\"reply_to_message_id\":\"mid-1\"}}}"}]}}`), nil
		default:
			t.Errorf("未预期的请求路径: %s", req.URL.Path)
			return jsonResponse(404, `{}`), nil
		}
	}

	exec := env.newExecutor()
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	content, err := exec.runStream(ctx, "你好", func(string) error { return nil })
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if content != "轮询补回的正文" {
		t.Errorf("正文 = %q，期望 轮询补回的正文", content)
	}
}

// TestCollectorAddsImagesWithDedupe 验证图片收集与去重。
//
// 上游可能在 start 与 final 两帧重复给出同一张图；
// 不去重会让客户端看到重复图片，也会让响应体无谓翻倍。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，图片去重契约。
func TestCollectorAddsImagesWithDedupe(t *testing.T) {
	collector := newReplyCollector()

	collector.accept(&frontierMessage{Phase: "event_delta", TurnID: "t1", SeqNum: "1", Images: []string{"https://a/1.jpg"}})
	collector.accept(&frontierMessage{Phase: "event_final", TurnID: "t1", SeqNum: "2", Images: []string{"https://a/1.jpg", "https://a/2.jpg"}})

	images := collector.imagesSnapshot()
	if len(images) != 2 {
		t.Fatalf("图片数 = %d，期望 2（重复地址应被去掉）: %#v", len(images), images)
	}
	if images[0] != "https://a/1.jpg" || images[1] != "https://a/2.jpg" {
		t.Errorf("图片顺序 = %#v，期望保持出现顺序", images)
	}

	// 快照必须是副本：调用方改动不能污染收集器内部状态。
	images[0] = "tampered"
	if collector.imagesSnapshot()[0] != "https://a/1.jpg" {
		t.Error("imagesSnapshot 应返回副本")
	}
}

// replyFrameWithImages 构造一条带图片的 reply 帧载荷。
//
// 结构按 2026-10-18 真实账号实测还原：图片在**载荷顶层 files**，
// content_v2 里只有一个 text 分片，正文是 `[图片](computer:///...)` 占位符。
// 早期版本把这个夹具写成 content_v2 的 image 分片，与真实上游不符，
// 导致「WS 生图取不到图」的缺陷在单测里完全不可见（见测试文档 6.3 缺陷 A）。
// [参数] phase: 阶段；seq: 序号；snapshot: 正文快照；imageURL: 图片地址。
// [返回] 帧载荷 JSON。
// 最近修改时间：2026-10-18；改动原因：改为真实上游形态，避免锁定不存在的契约。
func replyFrameWithImages(phase, seq, snapshot, imageURL string) []byte {
	snapshotJSON, _ := json.Marshal(snapshot)
	content := `{"message_data":{"reply":{"content":"","reply_to_message_id":"mid-1","stream":true}},"turn_id":"t1"}`
	contentV2 := `{"session":{"phase":"` + phase + `","seq_num":"` + seq + `","turn_id":"t1","final":{` +
		`"type":"agent.message","agent_message_event":{"content":[` +
		`{"type":"text","text":{"content":` + string(snapshotJSON) + `}}` +
		`]}}}}`

	payload := map[string]any{
		"id":           "m1",
		"session_id":   "s1",
		"message_type": "reply",
		"content":      content,
		"content_v2":   contentV2,
		"files": []map[string]any{{
			"file_name":    "/Coze/Drive/扣子/image/generate_test.jpg",
			"file_uri":     "3735510093922464_0-drive/2372/image/generate_test.jpg",
			"file_url":     imageURL,
			"thumb_url":    imageURL + "&thumb=1",
			"is_reference": 0,
		}},
	}
	raw, _ := json.Marshal(payload)
	return raw
}

// TestRunStreamEmitsImageMarkdown 验证收尾时下发图片 markdown。
//
// 生图结果不在正文增量里；runStream 必须在正文之后、收尾之前
// 把图片以 Markdown 形式交给 emit，否则客户端只看到占位文本。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，图片下发闭环。
func TestRunStreamEmitsImageMarkdown(t *testing.T) {
	frames := [][]byte{
		replyFrameWithImages("event_delta", "8", "正在生成", "https://static.coze.site/a.jpg?sign=X"),
		replyFrameWithImages("event_final", "9", "已生成", "https://static.coze.site/a.jpg?sign=X"),
	}

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		for _, payload := range frames {
			frame := encodeFrontierFrame(&frontierFrame{
				Service:     frontierServiceClaw,
				PayloadType: "message",
				Payload:     payload,
			})
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			time.Sleep(60 * time.Millisecond)
		}
	})

	exec := env.newExecutor()
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var emitted []string
	content, err := exec.runStream(ctx, "画只猫", func(delta string) error {
		emitted = append(emitted, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	joined := strings.Join(emitted, "")
	if !strings.Contains(joined, "![image](https://static.coze.site/a.jpg?sign=X)") {
		t.Fatalf("未下发图片 markdown，实际输出: %q", joined)
	}
	// 同一地址在 delta 与 final 各出现一次，只应下发一张。
	if strings.Count(joined, "![image](") != 1 {
		t.Errorf("图片重复下发: %q", joined)
	}
	// 图片段必须落在正文之后，客户端才能先看到文字再看到图。
	if idx := strings.Index(joined, "![image]("); idx >= 0 && idx < strings.Index(joined, "已生成") {
		t.Errorf("图片应位于正文之后，实际: %q", joined)
	}
	if content == "" {
		t.Error("正文不应为空")
	}
}

// TestExecuteDoesNotDuplicateContent 验证非流式正文不被拼接两遍。
//
// runStream 已把每个增量交给 emit，返回值又是同一份全文；
// execute 若把 emit 收到的内容再拼一次，非流式响应就变成
// 「正文 + 正文」，客户端看到重复答案。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，非流式正文去重守卫。
func TestExecuteDoesNotDuplicateContent(t *testing.T) {
	frames := [][]byte{
		replyFrame("event_delta", "8", "第一段", ""),
		replyFrame("event_delta", "9", "第二段", ""),
		replyFrame("event_final", "10", "", "第一段第二段"),
	}

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		for _, payload := range frames {
			frame := encodeFrontierFrame(&frontierFrame{
				Service:     frontierServiceClaw,
				PayloadType: "message",
				Payload:     payload,
			})
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			time.Sleep(60 * time.Millisecond)
		}
	})

	exec := env.newExecutor()
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	content, err := exec.execute(ctx, "你好")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if content != "第一段第二段" {
		t.Fatalf("非流式正文 = %q，期望恰好一份全文", content)
	}
}

// TestExecuteAppendsImageMarkdown 验证非流式也会带上图片 markdown。
//
// 非流式没有下游可以收尾补帧，图片只能拼进返回正文；
// 缺了它，客户端不流式请求时就看不到生图结果。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，非流式图片闭环。
func TestExecuteAppendsImageMarkdown(t *testing.T) {
	imageURL := "https://static.coze.site/b.jpg?sign=Y"
	frames := [][]byte{
		replyFrameWithImages("event_delta", "8", "已生成", imageURL),
		replyFrameWithImages("event_final", "9", "已生成", imageURL),
	}

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		for _, payload := range frames {
			frame := encodeFrontierFrame(&frontierFrame{
				Service:     frontierServiceClaw,
				PayloadType: "message",
				Payload:     payload,
			})
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			time.Sleep(60 * time.Millisecond)
		}
	})

	exec := env.newExecutor()
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	content, err := exec.execute(ctx, "画只猫")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if content != "已生成\n![image]("+imageURL+")" {
		t.Fatalf("非流式正文 = %q，期望正文后恰好跟一张图", content)
	}
}

// TestFallbackPollCollectsImagesFromFiles 验证兜底轮询也能取到图片。
//
// WS 不推帧时走 message/list；图片挂在该接口返回消息的 files 数组上，
// 若只解析内层 payload（正文是占位符）就永远补不出图。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，兜底图片路径。
func TestFallbackPollCollectsImagesFromFiles(t *testing.T) {
	replyContent := `{"id":"m1","message_data":{"reply":{"content":"[图片](computer://%2Fx.jpg)","reply_to_message_id":"mid-1","stream":false}}}`
	escaped, _ := json.Marshal(replyContent)

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		// 不推任何业务帧：逼出首字超时后的轮询兜底。
		time.Sleep(3 * time.Second)
	})
	env.doer.handler = func(req *http.Request, _ []byte) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, clawInfoPath):
			return jsonResponse(200, `{"code":0,"data":{"user_claw_info":{"claw_id":"c1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMainSessionPath):
			return jsonResponse(200, `{"code":0,"data":{"session":{"id":"s1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, frontierKeyPath):
			return jsonResponse(200, `{"code":0,"data":{"access_key":"k","device_id":"d"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawChatPath):
			return jsonResponse(200, `{"code":0,"data":{"message_id":"mid-1"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMessageListPath):
			body := `{"code":0,"data":{"messages":[{"id":"m1","message_type":"reply","content":` + string(escaped) +
				`,"files":[{"file_name":"/Coze/Drive/扣子/image/x.jpg","file_url":"https://static.coze.site/x.jpg?sign=Z"}]}]}}`
			return jsonResponse(200, body), nil
		default:
			return jsonResponse(404, `{}`), nil
		}
	}

	exec := env.newExecutor()
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	collector := newReplyCollector()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := exec.fallbackPoll(ctx, collector, "mid-1"); err != nil {
		t.Fatalf("兜底轮询失败: %v", err)
	}

	images := collector.imagesSnapshot()
	if len(images) != 1 || images[0] != "https://static.coze.site/x.jpg?sign=Z" {
		t.Fatalf("未从 files 取到图片，Images = %#v", images)
	}
}

// TestStreamTerminalReportsFailureBeforeFirstPayload 验证首帧前的失败必须上报。
//
// 生产实测（2026-10-19）：账号积分耗尽时上游 message/chat 直接返回
// code=1000521，插件补了一个空正文的 length 终止帧，宿主据此判成功，
// 下游记录「成功但 0 输出」且不计费。这里锁定契约：一个 payload 都没发
// 出去时必须走错误通道，不能伪造终止帧。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，上游失败被当成功的回归。
func TestStreamTerminalReportsFailureBeforeFirstPayload(t *testing.T) {
	upstreamErr := errors.New("提交消息失败：该用户积分已耗尽（code=1000521）")

	finishReason, errTerminal := streamTerminal(upstreamErr, false)
	if errTerminal == nil {
		t.Fatal("首帧前失败必须返回错误，不能补终止帧")
	}
	if errTerminal.Error() != upstreamErr.Error() {
		t.Errorf("错误被改写：%v", errTerminal)
	}
	if finishReason != "" {
		t.Errorf("失败路径不应给出终止原因，实际 %q", finishReason)
	}

	// 正文已泄漏后失败仍用 length 收尾，客户端保留已生成内容。
	finishReason, errTerminal = streamTerminal(upstreamErr, true)
	if errTerminal != nil {
		t.Errorf("已发内容后失败不应再报错：%v", errTerminal)
	}
	if finishReason != openAIFinishLength {
		t.Errorf("finish = %q，期望 length", finishReason)
	}

	finishReason, errTerminal = streamTerminal(nil, true)
	if errTerminal != nil || finishReason != openAIFinishStop {
		t.Errorf("正常结束应为 stop，实际 %q / %v", finishReason, errTerminal)
	}
}

// TestMarshalStreamErrorEnvelope 验证错误信封走 error 字段。
//
// 宿主的 rpcStreamEmitRequest 只有 error 字段能表达失败；写成 payload
// 会被当成正常内容帧，请求最终以「成功」告终。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，错误通道契约。
func TestMarshalStreamErrorEnvelope(t *testing.T) {
	raw, errMarshal := marshalStreamErrorEnvelope("sid-1", "积分已耗尽")
	if errMarshal != nil {
		t.Fatalf("构造失败：%v", errMarshal)
	}
	var body struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error"`
		Payload  string `json:"payload"`
	}
	if errUnmarshal := json.Unmarshal(raw, &body); errUnmarshal != nil {
		t.Fatalf("解析失败：%v", errUnmarshal)
	}
	if body.StreamID != "sid-1" {
		t.Errorf("stream_id = %q", body.StreamID)
	}
	if body.Error != "积分已耗尽" {
		t.Errorf("error = %q", body.Error)
	}
	if body.Payload != "" {
		t.Errorf("错误信封不得携带 payload，实际 %q", body.Payload)
	}
}

// TestRunStreamFailsWhenChatRejected 验证上游拒收消息时返回错误。
//
// 这是本轮生产缺陷的完整复现：握手正常、WS 就绪，但 message/chat
// 返回 code=1000521。旧实现返回空正文 + nil，调用方只能记成功。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，端到端失败透传。
func TestRunStreamFailsWhenChatRejected(t *testing.T) {
	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		// 不推业务帧：本轮上游根本没接受消息。
		time.Sleep(200 * time.Millisecond)
	})
	env.doer.handler = func(req *http.Request, _ []byte) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, clawInfoPath):
			return jsonResponse(200, `{"code":0,"data":{"user_claw_info":{"claw_id":"c1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMainSessionPath):
			return jsonResponse(200, `{"code":0,"data":{"session":{"id":"s1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, frontierKeyPath):
			return jsonResponse(200, `{"code":0,"data":{"access_key":"k","device_id":"d"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawChatPath):
			return jsonResponse(200, `{"code":1000521,"msg":"该用户积分已耗尽"}`), nil
		default:
			return jsonResponse(404, `{}`), nil
		}
	}

	exec := env.newExecutor()
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	content, err := exec.runStream(ctx, "你好", func(string) error { return nil })
	if err == nil {
		t.Fatalf("上游拒收必须返回错误，实际 content=%q", content)
	}
	if !strings.Contains(err.Error(), "积分已耗尽") {
		t.Errorf("错误应透传上游文案，实际 %v", err)
	}
}

// TestWaitForReplyFailsOnEmptyFinished 验证「收尾但无内容」不再返回 nil。
//
// WS 推了 event_final 却没有正文与图片、兜底轮询也拿不到消息时，
// 旧实现返回空串 + nil，下游记为成功。必须返回错误。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，空正文缺陷回归。
func TestWaitForReplyFailsOnEmptyFinished(t *testing.T) {
	collector := newReplyCollector()
	collector.accept(&frontierMessage{Phase: "event_final", TurnID: "t1", SeqNum: "1"})
	if !collector.isFinished() {
		t.Fatal("前置条件：收集器应已收尾")
	}

	exec := newCozeExecutor(&StoredAuth{CookieHeader: "sessionid=x"}, "coze/m", &fakeDoer{
		handler: func(*http.Request, []byte) (*http.Response, error) {
			return jsonResponse(200, `{"code":0,"data":{"messages":[]}}`), nil
		},
	})

	// 兜底轮询会一直跑，用短超时让它尽快放弃。
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	content, err := exec.waitForReply(ctx, collector, func(string) error { return nil }, "mid-1")
	if err == nil {
		t.Fatalf("收尾但无内容必须返回错误，实际 content=%q", content)
	}
}

// captureHostStream 拦截 stream.emit，记录发出的每一个信封。
//
// 返回的 restore 必须在测试结束前调用，否则会影响后续用例。
// [参数] t: 测试上下文。
// [返回] 记录切片（按发送顺序）。
// 最近修改时间：2026-10-19；改动原因：新增，流式出口注入。
func captureHostStream(t *testing.T) *[]map[string]any {
	t.Helper()
	var sent []map[string]any
	original := hostStreamSend
	hostStreamSend = func(_ string, body []byte) error {
		var item map[string]any
		_ = json.Unmarshal(body, &item)
		sent = append(sent, item)
		return nil
	}
	t.Cleanup(func() { hostStreamSend = original })
	return &sent
}

// TestPumpClawStreamEmitsErrorBeforeFirstPayload 验证首帧前失败走错误通道。
//
// 生产实测（2026-10-19）：积分耗尽时上游拒收消息，旧实现仍补一个
// 空正文终止帧，宿主据此判成功——下游记录「成功但 0 输出」且不计费。
// 这里锁定契约：首帧前失败只能发 error 信封，绝不能发 payload。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，端到端失败透传回归。
func TestPumpClawStreamEmitsErrorBeforeFirstPayload(t *testing.T) {
	restore := stubConversation(t, "", errors.New("提交消息失败：该用户积分已耗尽（code=1000521）"))
	defer restore()

	sent := captureHostStream(t)

	pumpClawStream("sid-test", &StoredAuth{CookieHeader: "sessionid=x"}, "coze/m", "你好")

	for _, item := range *sent {
		if _, hasPayload := item["payload"]; hasPayload {
			t.Fatalf("首帧前失败不得发出任何 payload，实际 %#v", item)
		}
	}
	found := false
	for _, item := range *sent {
		if msg, ok := item["error"].(string); ok && strings.Contains(msg, "积分已耗尽") {
			found = true
		}
	}
	if !found {
		t.Fatalf("必须发出带上游错误文案的 error 信封，实际 %#v", *sent)
	}
}

// TestPumpClawStreamKeepsContentAfterPartialFailure 验证正文已泄漏后仍正常收尾。
//
// 已经发出过内容的失败不能改走错误通道：客户端此时已渲染正文，
// 再报错会让整轮结果作废。约定是用 length 收尾并补齐 usage。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，防过度上报。
func TestPumpClawStreamKeepsContentAfterPartialFailure(t *testing.T) {
	restore := stubConversation(t, "部分正文", errors.New("上游连接中断"))
	defer restore()

	sent := captureHostStream(t)

	pumpClawStream("sid-test", &StoredAuth{CookieHeader: "sessionid=x"}, "coze/m", "你好")

	var sawError, sawLength, sawUsage bool
	for _, item := range *sent {
		if _, hasError := item["error"]; hasError {
			sawError = true
		}
		payload, ok := item["payload"].(string)
		if !ok {
			continue
		}
		raw, errDecode := base64.StdEncoding.DecodeString(payload)
		if errDecode != nil {
			continue
		}
		if strings.Contains(string(raw), `"finish_reason":"length"`) {
			sawLength = true
		}
		if strings.Contains(string(raw), `"usage"`) {
			sawUsage = true
		}
	}
	if sawError {
		t.Error("正文已泄漏后失败不应再发错误帧")
	}
	if !sawLength {
		t.Error("正文已泄漏后失败应以 length 收尾")
	}
	if !sawUsage {
		t.Error("终止帧必须带 usage")
	}
}

// stubConversation 把对话执行替换成固定结果。
//
// 直接注入执行结果，跳过真实 WS 与 HTTP：用例只关心收尾契约，
// 上游细节已由 Executor 系列用例覆盖。
// [参数] t: 测试上下文；reply: 假正文；err: 假错误。
// [返回] 还原函数。
// 最近修改时间：2026-10-19；改动原因：新增，收尾契约注入。
func stubConversation(t *testing.T, reply string, errRun error) func() {
	t.Helper()
	original := runExecutorConversation
	runExecutorConversation = func(_ context.Context, _ *StoredAuth, _, _ string, emit func(string) error) (string, error) {
		if reply != "" {
			if errEmit := emit(reply); errEmit != nil {
				return reply, errEmit
			}
		}
		return reply, errRun
	}
	restore := func() { runExecutorConversation = original }
	t.Cleanup(restore)
	return restore
}
