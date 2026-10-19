package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件实现扣子的 Executor 能力，把 OpenAI 请求接到 claw 对话链路上。
//
// 一次请求的完整时序（2026-10-18 实测）：
//  1. claw/info       → claw_id
//  2. session/get_main→ session_id
//  3. get_frontier_key→ access_key + device_id（WS 凭证）
//  4. 建立 WS 并开始收帧（必须早于第 5 步，否则会漏掉首帧增量）
//  5. message/chat    → 用户消息 message_id（异步）
//  6. 从 WS 收 reply 帧：event_delta 输出增量，event_final 收尾
//
// 为什么第 4 步必须在第 5 步之前：chat 返回时上游可能已经开始推送，
// 晚连会丢掉开头几个增量，用户看到的第一段文本就缺字。

const (
	// executorReplyTimeout 是等待一轮回复完成的硬上限。
	//
	// 长任务（生图、长文档）可能远超默认 30 秒；这里给 5 分钟，
	// 与插件 timeout_seconds 解耦——后者管单次 HTTP，这里管整轮对话。
	executorReplyTimeout = 5 * time.Minute

	// executorFirstTokenTimeout 是等待首个增量帧的上限。
	//
	// 超时后降级到 message/list 轮询兜底：上游偶发不推 WS 帧，
	// 但消息仍会落库，此时至少能把答案给用户。
	executorFirstTokenTimeout = 90 * time.Second
)

// cozeExecutor 是一次执行的上下文。
//
// 每个请求独立构造，避免跨请求共享可变状态。
type cozeExecutor struct {
	client *clawClient
	sa     *StoredAuth
	model  string
	// frontierServerURL 覆盖 frontier 主机，仅供测试指向 httptest 服务端。
	frontierServerURL string
}

// replyCollector 收集一轮回复的增量与收尾状态。
//
// 为什么需要互斥：WS 回调在独立的读协程里执行，
// 而等待方在主协程读，无锁会产生数据竞争。
type replyCollector struct {
	mu sync.Mutex
	// deltas 按到达顺序保存增量片段。
	deltas []string
	// seen 是去重集合，键为 turn_id|seq_num|phase。
	seen map[string]struct{}
	// snapshot 是 event_final 携带的累积全文。
	snapshot string
	// finished 表示已收到收尾帧。
	finished bool
	// firstTokenAt 记录首个非空增量的到达时间。
	firstTokenAt time.Time
	// pending 是尚未交给下游的增量片段。
	//
	// 与 deltas 分开维护：deltas 是「已收到的全部」，
	// pending 是「还没 emit 的」。分开才能保证每个上游增量
	// 都独立下发一次，而不是被下一次轮询合并成一大块
	// ——后者会让客户端看到「卡一下然后蹦出一大段」。
	pending []string
	// images 是本轮回复里出现的图片地址。
	//
	// 生图结果原子出现（约 18 秒才落库），没有中间态可流，
	// 因此单独收集、在正文结束后一次性补发。
	images []string
	// notify 用于唤醒等待方。
	notify chan struct{}
}

// newReplyCollector 构造收集器。
// [参数] 无。
// [返回] 收集器。
// 最近修改时间：2026-10-18；改动原因：新增，收集器构造。
func newReplyCollector() *replyCollector {
	return &replyCollector{
		seen:   map[string]struct{}{},
		notify: make(chan struct{}, 1),
	}
}

// accept 接收一条已解析的 reply 消息。
//
// 返回值表示是否接受了新内容：重复帧与空增量都不算。
// [参数] msg: 已解析消息。
// [返回] 是否产生了新内容。
// 最近修改时间：2026-10-18；改动原因：新增，增量接收与去重。
func (c *replyCollector) accept(msg *frontierMessage) bool {
	if msg == nil {
		return false
	}

	c.mu.Lock()
	key := msg.dedupeKey()
	if key != "" {
		if _, exists := c.seen[key]; exists {
			c.mu.Unlock()
			return false
		}
		c.seen[key] = struct{}{}
	}

	changed := false
	if msg.Delta != "" {
		c.deltas = append(c.deltas, msg.Delta)
		c.pending = append(c.pending, msg.Delta)
		if c.firstTokenAt.IsZero() {
			c.firstTokenAt = time.Now()
		}
		changed = true
	}
	if msg.Snapshot != "" {
		c.snapshot = msg.Snapshot
		changed = true
	}
	// 图片与正文增量分开收集：它们不走 pending 队列，
	// 而是攒到收尾时一次性下发（实测生图结果原子出现，无中间态）。
	if len(msg.Images) > 0 {
		c.addImagesLocked(msg.Images)
		changed = true
	}
	if msg.isFinal() {
		c.finished = true
		changed = true
	}
	c.mu.Unlock()
	if changed {
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
	return changed
}

// addImagesLocked 去重追加图片地址。
//
// 调用方必须已持有 c.mu：accept 在同一临界区内完成收集，
// 避免为一次追加再走一次加解锁。
// [参数] urls: 待追加地址（空串与重复项被忽略）。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，图片收集。
func (c *replyCollector) addImagesLocked(urls []string) {
	c.images = appendUniqueAll(c.images, urls...)
}

// imagesSnapshot 返回本轮已收集的图片地址副本。
// [参数] 无。
// [返回] 图片地址列表（调用方可安全遍历）。
// 最近修改时间：2026-10-18；改动原因：新增，收尾下发读取。
func (c *replyCollector) imagesSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.images) == 0 {
		return nil
	}
	out := make([]string, len(c.images))
	copy(out, c.images)
	return out
}

// drainPending 取出所有尚未下发的增量片段。
//
// 返回切片而不是拼接字符串：每个片段对应一个上游帧，
// 逐条下发才能让客户端按到达节奏渲染。
// [参数] 无。
// [返回] 待下发片段（调用后清空）。
// 最近修改时间：2026-10-18；改动原因：新增，逐帧下发支持。
func (c *replyCollector) drainPending() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	out := c.pending
	c.pending = nil
	return out
}

// text 返回当前已收集的正文。
//
// 优先用增量拼接结果：它与 event_final 的累积全文实测完全一致
// （10 个 delta 拼接 371 字符 == final 371 字符），且能在流式过程中
// 持续增长；snapshot 只在收尾后才有值，用作兜底校对。
// [参数] 无。
// [返回] 当前正文。
// 最近修改时间：2026-10-18；改动原因：新增，正文读取。
func (c *replyCollector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.deltas) > 0 {
		return strings.Join(c.deltas, "")
	}
	return c.snapshot
}

// isFinished 判断是否已收尾。
// [参数] 无。
// [返回] 已收尾时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，收尾状态读取。
func (c *replyCollector) isFinished() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finished
}

// hasFirstToken 判断是否已收到首个增量。
// [参数] 无。
// [返回] 已收到时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，首字判定。
func (c *replyCollector) hasFirstToken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.firstTokenAt.IsZero()
}

// newCozeExecutor 构造一次执行的上下文。
// [参数] sa: 账号；model: 对外模型 ID；client: HTTP 客户端。
// [返回] 执行上下文。
// 最近修改时间：2026-10-18；改动原因：新增，执行上下文构造。
func newCozeExecutor(sa *StoredAuth, model string, client httpDoer) *cozeExecutor {
	return &cozeExecutor{
		client: newClawClient(sa, client),
		sa:     sa,
		model:  model,
	}
}

// prepare 完成对话前的握手：取 claw_id、主会话与 WS 凭证。
// [参数] 无。
// [返回] 会话 ID、WS 凭证与错误。
// 最近修改时间：2026-10-18；改动原因：新增，握手三步。
func (e *cozeExecutor) prepare() (string, *frontierKeyResponse, error) {
	clawID, errClaw := e.client.fetchClawID()
	if errClaw != nil {
		return "", nil, errClaw
	}

	sessionID, errSession := e.client.fetchMainSession(clawID)
	if errSession != nil {
		return "", nil, errSession
	}

	key, errKey := e.client.fetchFrontierKey()
	if errKey != nil {
		return "", nil, errKey
	}
	return sessionID, key, nil
}

// runStream 执行一次流式对话，把增量交给 emit 回调。
//
// emit 返回错误时立即中止（通常是下游客户端断开）。
// [参数] ctx: 生命周期；prompt: 折叠后的用户文本；emit: 增量回调。
// [返回] 完整正文与错误。
// 最近修改时间：2026-10-18；改动原因：新增，流式执行主流程。
func (e *cozeExecutor) runStream(ctx context.Context, prompt string, emit func(string) error) (string, error) {
	content, images, errRun := e.runConversation(ctx, prompt, emit)

	// 图片在正文之后一次性补发：实测生图结果原子出现（约 18 秒才落库），
	// 没有中间态可流；放在收尾前发出，客户端就能在正文结束后立即看到图。
	if markdown := buildImageMarkdown(images); markdown != "" {
		if errEmit := emit(markdown); errEmit != nil && errRun == nil {
			errRun = errEmit
		}
	}
	return content, errRun
}

// runConversation 执行一轮对话，返回正文与本轮出现的图片地址。
//
// 图片只返回不发射：流式与非流式的交付位置不同（前者收尾补一帧，
// 后者拼进响应体），由调用方决定怎么给。返回图片列表而不是拼好的
// markdown，是为了让非流式路径不必再从回调里反解字符串。
// [参数] ctx: 生命周期；prompt: 折叠后的用户文本；emit: 正文增量回调。
// [返回] 完整正文、图片地址列表与错误。
// 最近修改时间：2026-10-18；改动原因：抽出共用执行体，修复非流式正文重复。
func (e *cozeExecutor) runConversation(ctx context.Context, prompt string, emit func(string) error) (string, []string, error) {
	sessionID, key, errPrepare := e.prepare()
	if errPrepare != nil {
		return "", nil, errPrepare
	}

	collector := newReplyCollector()

	// WS 必须在提交消息前建立：chat 返回时上游可能已在推送，
	// 晚连会丢掉开头几个增量。
	wsCtx, stopWS := context.WithCancel(ctx)
	defer stopWS()

	client := newFrontierClient(key, e.sa.CookieHeader, func(ev frontierEvent) {
		msg, found := parseFrontierMessage(ev.Payload)
		if !found {
			return
		}
		collector.accept(msg)
	})
	if e.frontierServerURL != "" {
		client.serverURL = e.frontierServerURL
	}

	wsErrCh := make(chan error, 1)
	go func() {
		wsErrCh <- client.Run(wsCtx)
	}()
	defer client.Close()

	// 等连接真正建立再提交消息：chat 一旦发出上游就开始推送，
	// 快速回复（约 2 秒）的增量会在连接就绪前全部丢掉，只剩重放的
	// event_final，最终正文为空（2026-10-18 多轮场景实测）。
	readyCtx, cancelReady := context.WithTimeout(ctx, frontierHandshakeTimeout)
	errReady := client.WaitReady(readyCtx)
	cancelReady()
	if errReady != nil {
		client.Close()
		return "", nil, fmt.Errorf("frontier 连接未就绪：%w", errReady)
	}

	messageID, errChat := e.client.sendChat(sessionID, prompt)
	if errChat != nil {
		return "", nil, errChat
	}

	// 图片来自 WS 实时与 message/list 兜底两条路径，都汇到同一个 collector；
	// 取快照必须在这之后，否则兜底补进来的图会漏。
	content, errWait := e.waitForReply(ctx, collector, emit, messageID)
	return content, collector.imagesSnapshot(), errWait
}

// waitForReply 等待回复完成，并把增量实时交给 emit。
//
// 双通道等待：collector 的 notify 负责「有新内容」，
// ctx 负责「超时/取消」。首字超时后降级到轮询兜底。
// [参数] ctx: 生命周期；collector: 收集器；emit: 增量回调。
// [返回] 完整正文与错误。
// 最近修改时间：2026-10-18；改动原因：新增，等待与降级。
func (e *cozeExecutor) waitForReply(ctx context.Context, collector *replyCollector, emit func(string) error, messageID string) (string, error) {
	deadline := time.Now().Add(executorReplyTimeout)
	firstTokenDeadline := time.Now().Add(executorFirstTokenTimeout)

	// 逐条下发尚未交付的增量：每条对应一个上游帧，
	// 合并成一大块会让客户端「卡一下然后蹦出一大段」。
	flush := func() error {
		for _, delta := range collector.drainPending() {
			if err := emit(delta); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		if collector.isFinished() {
			// 收尾了但一个字都没有：说明增量帧在连接就绪前就推完了，
			// 只剩重放的 final。此时降级轮询把落库正文补齐。
			if strings.TrimSpace(collector.text()) == "" && messageID != "" {
				if errPoll := e.fallbackPoll(ctx, collector, messageID); errPoll == nil {
					if errFlush := flush(); errFlush != nil {
						return collector.text(), errFlush
					}
					return collector.text(), nil
				} else {
					// 兜底也失败时留痕：否则空正文会被当成「正常完成」
					// 一路带到下游，表现为「成功但 0 输出」。
					hostLogf("warn", "空正文兜底轮询失败", map[string]any{
						"message_id": messageID,
						"error":      errPoll.Error(),
					})
				}
			}
			// 收尾了却既没有正文也没有图片：上游确实没产出内容。
			// 必须返回错误，否则调用方会把它当「空正文的成功响应」
			// 一路带到下游，表现为「成功但 0 输出」（2026-10-19 生产实测）。
			if strings.TrimSpace(collector.text()) == "" && len(collector.imagesSnapshot()) == 0 {
				hostLogf("warn", "回复正文为空", map[string]any{
					"message_id": messageID,
					"finished":   true,
				})
				return "", fmt.Errorf("回复正文为空")
			}
			if err := flush(); err != nil {
				return collector.text(), err
			}
			return collector.text(), nil
		}

		if time.Now().After(deadline) {
			return collector.text(), fmt.Errorf("等待回复超时")
		}

		// 首字长时间不来：上游偶发不推 WS 帧，但消息会落库。
		// 降级到轮询能把答案给用户，代价是失去实时性。
		if !collector.hasFirstToken() && time.Now().After(firstTokenDeadline) {
			if err := e.fallbackPoll(ctx, collector, messageID); err != nil {
				return collector.text(), err
			}
			if err := flush(); err != nil {
				return collector.text(), err
			}
			// 轮询成功但既无正文也无图片：这一轮上游确实没产出，
			// 返回 nil 会被下游记成「成功但 0 输出」。
			if strings.TrimSpace(collector.text()) == "" && len(collector.imagesSnapshot()) == 0 {
				return "", fmt.Errorf("回复正文为空")
			}
			return collector.text(), nil
		}

		select {
		case <-ctx.Done():
			return collector.text(), ctx.Err()
		case <-collector.notify:
			if err := flush(); err != nil {
				return collector.text(), err
			}
		case <-time.After(200 * time.Millisecond):
			if err := flush(); err != nil {
				return collector.text(), err
			}
		}
	}
}

// fallbackPoll 用 message/list 轮询补齐回复。
//
// 只在「首字超时」或「已收尾但正文为空」时使用：它拿到的是已落库的
// 完整消息（实测约 4 秒延迟），没有增量语义，但能保证「至少有答案」。
// 必须按 userMessageID 精确配对，否则会把上一轮的回复当成本轮结果。
// [参数] ctx: 生命周期；collector: 收集器；userMessageID: 本轮用户消息 ID。
// [返回] 错误。
// 最近修改时间：2026-10-18；改动原因：补 message_id 配对，修复空回复。
func (e *cozeExecutor) fallbackPoll(ctx context.Context, collector *replyCollector, userMessageID string) error {
	// 这里不重取 sessionID：调用方已确认会话可用，
	// 重取只会增加一次无谓的上游往返。
	sessionID, _, errPrepare := e.prepare()
	if errPrepare != nil {
		return fmt.Errorf("兜底轮询前握手失败：%w", errPrepare)
	}

	pollDeadline := time.Now().Add(executorReplyTimeout)
	for time.Now().Before(pollDeadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		messages, errList := e.client.listMessages(sessionID)
		if errList == nil {
			// 用整条消息而不是只取内层 payload：图片附件在消息外层的
			// Files 上，只看 payload 会漏图（生图回复的正文只是占位符）。
			message, found := findReplyMessageFor(messages, userMessageID)
			if found {
				var payload clawReplyPayload
				if errUnmarshal := json.Unmarshal([]byte(message.Content), &payload); errUnmarshal == nil {
					if payload.MessageData.Reply.Content != "" || len(message.Files) > 0 {
						collector.accept(&frontierMessage{
							ID:       payload.ID,
							Phase:    "event_final",
							Snapshot: payload.MessageData.Reply.Content,
							Images:   clawFileURLs(message.Files),
						})
						return nil
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(clawPollInterval):
		}
	}
	return fmt.Errorf("兜底轮询超时")
}

// execute 执行一次非流式对话。
//
// 非流式没有下游可以逐步收帧，图片只能拼进返回正文；
// 正文取 runConversation 的返回值，不能把 emit 收到的增量再拼一次，
// 否则响应里会出现两份正文。
// [参数] ctx: 生命周期；prompt: 折叠后的文本。
// [返回] 完整正文（含图片 markdown）与错误。
// 最近修改时间：2026-10-18；改动原因：非流式补齐图片下发，避免正文重复。
func (e *cozeExecutor) execute(ctx context.Context, prompt string) (string, error) {
	content, images, errRun := e.runConversation(ctx, prompt, func(string) error { return nil })
	markdown := buildImageMarkdown(images)
	switch {
	case markdown == "":
		return content, errRun
	case content == "":
		return markdown, errRun
	default:
		return content + "\n" + markdown, errRun
	}
}

// handleExecExecute 处理非流式执行请求。
// [参数] raw: 宿主下发的 ExecutorRequest JSON。
// [返回] 信封 JSON 与错误。
// 最近修改时间：2026-10-18；改动原因：新增，非流式入口。
func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	sa, errParse := parseStoredAuth(req.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}

	// 标准图片端点与对话端点共用同一个 executor，必须按来源格式分流：
	// images 请求没有 messages，按 chat 解析必然报「请求缺少 messages」。
	if isImagesExecution(req) {
		return handleImagesExecute(sa, req)
	}

	chatReq, errChat := parseChatRequest(executorRequestBody(req))
	if errChat != nil {
		return nil, errChat
	}
	prompt, errPrompt := buildClawPrompt(chatReq.Messages)
	if errPrompt != nil {
		return nil, errPrompt
	}

	model := req.Model
	if chatReq.Model != "" {
		model = chatReq.Model
	}

	// 与流式入口同样的体积预检：超限内容在上游只换来 code=1000003，
	// 却会被宿主记成账号故障并冷却账号。
	if errSize := verifyChatContent(prompt); errSize != nil {
		return nil, errSize
	}

	ctx, cancel := context.WithTimeout(context.Background(), executorReplyTimeout)
	defer cancel()

	exec := newCozeExecutor(sa, model, nil)
	content, errRun := exec.execute(ctx, prompt)
	if errRun != nil && content == "" {
		return nil, errRun
	}

	payload, errBuild := buildCompletion(newCompletionID(), model, content, buildUsage(prompt, content))
	if errBuild != nil {
		return nil, errBuild
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

// executorStreamRequest 是流式执行请求。
//
// 宿主在 ExecutorRequest 之外追加 stream_id / host_callback_id；
// 没有 stream_id 说明宿主走同步收集路径，需要返回完整 chunk 列表。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，异步流式需要宿主 stream_id。
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// streamResponse 是同步路径的流式响应。
//
// 宿主的 RPC 桥把 Chunks 定义成 slice 再转 channel；
// 直接返回 channel 会让 JSON 序列化报
// `json: unsupported type: <-chan pluginapi.ExecutorStreamChunk`。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，同步收集路径的响应结构。
type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// handleExecStream 处理流式执行请求。
//
// 宿主提供 stream_id 时走异步：立刻返回响应头，再由后台协程用
// `host.stream.emit` 逐帧推送，客户端才能看到真流式。没有 stream_id
// 时退化为同步收集，一次性返回全部 chunk（宿主 RPC 的 slice 形态）。
// [参数] raw: 宿主下发的 ExecutorRequest JSON。
// [返回] 流式响应信封与错误。
// 最近修改时间：2026-10-18；改动原因：改为宿主异步 emit，修复生产 JSON 序列化失败。
func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	sa, errParse := parseStoredAuth(req.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}

	chatReq, errChat := parseChatRequest(executorRequestBody(req.ExecutorRequest))
	if errChat != nil {
		return nil, errChat
	}
	prompt, errPrompt := buildClawPrompt(chatReq.Messages)
	if errPrompt != nil {
		return nil, errPrompt
	}

	model := req.Model
	if chatReq.Model != "" {
		model = chatReq.Model
	}

	// 体积预检必须在同步入口完成：这里是唯一能把 413 经 RPC 信封
	// 回传宿主的位置。后台协程里的失败只能走 stream.emit 的纯文本
	// error 字段，宿主看不到状态码，会把它当账号级故障冷却整个池。
	if errSize := verifyChatContent(prompt); errSize != nil {
		return nil, errSize
	}

	headers := streamHeaders()

	// 同步路径：宿主没有给 stream_id，只能收集完再一次性返回。
	if req.StreamID == "" {
		chunks, errCollect := collectStreamChunks(sa, model, prompt)
		if errCollect != nil {
			return nil, errCollect
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	go pumpClawStream(req.StreamID, sa, model, prompt)
	return okEnvelope(streamResponse{Headers: headers})
}

// streamHeaders 返回流式响应的公共头。
// [参数] 无。
// [返回] 响应头。
// 最近修改时间：2026-10-18；改动原因：新增，异步与同步路径共用。
func streamHeaders() http.Header {
	return http.Header{
		"Content-Type":      {"text/event-stream"},
		"Cache-Control":     {"no-cache"},
		"X-Accel-Buffering": {"no"},
	}
}

// hostStreamSend 是 stream.emit 的唯一出口。
//
// 抽成变量是为了测试可注入：宿主 API 只在 .so 加载后才可用，
// 单测无法构造真实宿主，只能替换这一层断言「到底发了什么帧」。
var hostStreamSend = func(streamID string, body []byte) error {
	_, errCall := hostCall(pluginabi.MethodHostStreamEmit, body)
	return errCall
}

// runExecutorConversation 跑一整轮对话，是流式路径的唯一执行入口。
//
// 抽成变量便于测试注入失败：宿主 API 与上游都只在运行时可用，
// 单测需要直接模拟「上游拒收消息、一个字节都没产出」这一分支。
var runExecutorConversation = func(ctx context.Context, sa *StoredAuth, model, prompt string, emit func(string) error) (string, error) {
	exec := newCozeExecutor(sa, model, nil)
	return exec.runStream(ctx, prompt, emit)
}

// streamEmit 向宿主推送一帧。
//
// payload 必须是裸 JSON 对象：宿主按请求格式决定是否补 `data: ` 前缀，
// 插件提前加前缀会让 chat/completions 客户端收到双重包裹。
// [参数] streamID: 宿主流标识；payload: 一帧内容。
// [返回] 推送错误；客户端断开时宿主会返回错误，调用方据此停止上游读取。
// 最近修改时间：2026-10-18；改动原因：新增，异步流式唯一出口。
func streamEmit(streamID string, payload []byte) error {
	body, errMarshal := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	if errMarshal != nil {
		return errMarshal
	}
	return hostStreamSend(streamID, body)
}

// marshalStreamErrorEnvelope 构造带 error 字段的 stream.emit 信封。
//
// 宿主的 rpcStreamEmitRequest 只有 error 字段能表达失败：写成 payload
// 会被当成正常内容帧，请求最终以「成功」告终。
// [参数] streamID: 宿主流标识；message: 终态错误文本。
// [返回] 信封 JSON 与错误。
// 最近修改时间：2026-10-19；改动原因：新增，上游失败透传通道。
func marshalStreamErrorEnvelope(streamID, message string) ([]byte, error) {
	return json.Marshal(map[string]any{"stream_id": streamID, "error": message})
}

// emitStreamError 通过宿主 stream.emit 的 error 字段上报终态失败。
//
// 只有真正的错误帧才会让宿主把请求判为失败；下游因此能看到明确的
// 错误文案，而不是「成功但 0 输出」。
// [参数] streamID: 宿主流标识；message: 终态错误文本。
// [返回] 推送错误。
// 最近修改时间：2026-10-19；改动原因：新增，上游失败不再伪装成功。
func emitStreamError(streamID, message string) error {
	body, errMarshal := marshalStreamErrorEnvelope(streamID, message)
	if errMarshal != nil {
		return errMarshal
	}
	return hostStreamSend(streamID, body)
}

// streamTerminal 决定一轮流式执行结束时的收尾方式。
//
// 失败且从未发出过任何 payload 时必须返回错误：宿主把「首个 payload」
// 当作流已成功的判据，此时补一个空终止帧会被记成「成功但 0 输出」，
// 下游既看不到错误也无法计费（2026-10-19 生产实测：积分耗尽被记成功）。
// 正文已泄漏后失败仍用 length 收尾，客户端保留已生成内容。
// [参数] errRun: 执行错误；emitted: 是否已发出过 payload。
// [返回] 终止原因（空表示改用错误帧）与需要上报的错误。
// 最近修改时间：2026-10-19；改动原因：新增，失败收尾判定。
func streamTerminal(errRun error, emitted bool) (string, error) {
	if errRun == nil {
		return openAIFinishStop, nil
	}
	if !emitted {
		return "", errRun
	}
	return openAIFinishLength, nil
}

// streamClose 关闭宿主流。
// [参数] streamID: 宿主流标识。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，异步流式收尾。
func streamClose(streamID string) {
	body, errMarshal := json.Marshal(map[string]any{"stream_id": streamID})
	if errMarshal != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

// pumpClawStream 在后台跑完一轮对话并逐帧推送。
//
// 为什么必须异步：宿主在拿到响应后立即开始向客户端转发，
// 同步阻塞会让客户端在整轮对话期间收不到任何字节。
//
// 首帧必须延迟到真正有内容时再发：宿主把「首个 payload」当作流已成功的
// 判据，抢先发角色帧会让随后所有的上游失败都退化成「成功但 0 输出」，
// 下游既看不到错误也无法计费（2026-10-19 生产实测：积分耗尽被记成功）。
// [参数] streamID: 宿主流标识；sa: 账号；model: 模型名；prompt: 用户提示。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：首帧延迟，修复上游失败被当成功。
func pumpClawStream(streamID string, sa *StoredAuth, model, prompt string) {
	defer streamClose(streamID)

	ctx, cancel := context.WithTimeout(context.Background(), executorReplyTimeout)
	defer cancel()

	id := newCompletionID()
	emitted := false

	// emitContent 在首个内容帧前补发角色声明，部分客户端据此决定渲染方式。
	emitContent := func(payload []byte) error {
		if !emitted {
			first, errFirst := buildFirstChunk(id, model)
			if errFirst != nil {
				return errFirst
			}
			if errEmit := streamEmit(streamID, first); errEmit != nil {
				return errEmit
			}
			emitted = true
		}
		return streamEmit(streamID, payload)
	}

	emit := func(delta string) error {
		chunk, errBuild := buildChunk(id, model, delta, nil, nil)
		if errBuild != nil {
			return errBuild
		}
		return emitContent(chunk)
	}

	reply, errRun := runExecutorConversation(ctx, sa, model, prompt, emit)

	finishReason, errTerminal := streamTerminal(errRun, emitted)
	if errTerminal != nil {
		// 一个字节都没发出去：必须走错误通道，不能补空终止帧。
		hostLogf("warn", "对话执行失败", map[string]any{
			"model":      model,
			"prompt_len": len(prompt),
			"error":      errTerminal.Error(),
		})
		_ = emitStreamError(streamID, errTerminal.Error())
		return
	}
	if errRun != nil {
		// 正文已泄漏后失败：用 length 收尾，客户端保留已生成内容。
		hostLogf("warn", "对话执行中断", map[string]any{
			"model":      model,
			"prompt_len": len(prompt),
			"reply_len":  len(reply),
			"finish":     finishReason,
			"error":      errRun.Error(),
		})
	}

	// 用量挂在终止帧上：宿主与 new-api 都从这一帧取 token 数，
	// 缺了会让整轮请求记零用量、下游无法计费。
	terminal, errBuild := buildChunk(id, model, "", &finishReason, buildUsage(prompt, reply))
	if errBuild == nil {
		_ = emitContent(terminal)
	}
}

// collectStreamChunks 同步收集整轮对话的所有 chunk。
//
// 仅用于宿主没有提供 stream_id 的同步路径；代价是失去实时性，
// 但比直接报错让请求失败更好。
//
// 首帧同样延迟到有内容时再补：宿主把首个 chunk 当成功判据，
// 抢先发角色帧会让上游失败退化成「成功但 0 输出」。
// 一个字节都没产生时直接返回错误，由宿主按失败处理。
// [参数] sa: 账号；model: 模型名；prompt: 用户提示。
// [返回] chunk 列表与错误。
// 最近修改时间：2026-10-19；改动原因：失败透传，不再伪造空终止帧。
func collectStreamChunks(sa *StoredAuth, model, prompt string) ([]pluginapi.ExecutorStreamChunk, error) {
	ctx, cancel := context.WithTimeout(context.Background(), executorReplyTimeout)
	defer cancel()

	id := newCompletionID()
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	emitted := false

	emitContent := func(payload []byte) error {
		if !emitted {
			first, errFirst := buildFirstChunk(id, model)
			if errFirst != nil {
				return errFirst
			}
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: first})
			emitted = true
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
		return nil
	}

	emit := func(delta string) error {
		chunk, errBuild := buildChunk(id, model, delta, nil, nil)
		if errBuild != nil {
			return errBuild
		}
		return emitContent(chunk)
	}

	reply, errRun := runExecutorConversation(ctx, sa, model, prompt, emit)

	finishReason, errTerminal := streamTerminal(errRun, emitted)
	if errTerminal != nil {
		return nil, errTerminal
	}
	if terminal, errBuild := buildChunk(id, model, "", &finishReason, buildUsage(prompt, reply)); errBuild == nil {
		if errAppend := emitContent(terminal); errAppend != nil {
			return nil, errAppend
		}
	}
	return chunks, nil
}

// executorRequestBody 取执行请求的 OpenAI 聊天请求体。
//
// 必须优先用 Payload：宿主只对 Payload 做协议翻译，插件声明
// executor_input_formats=["openai"] 后，Payload 一定是 OpenAI chat 形态。
// OriginalRequest 保存客户端原始请求，走 /v1/responses 时里面只有 input、
// 没有 messages，优先读它会让所有非 chat 入口的请求直接失败。
// [参数] req: 执行请求。
// [返回] 请求体字节。
// 最近修改时间：2026-10-18；改动原因：修复 Responses 入口必然报「请求缺少 messages」。
func executorRequestBody(req pluginapi.ExecutorRequest) []byte {
	if len(req.Payload) > 0 {
		return req.Payload
	}
	return req.OriginalRequest
}

// parseStoredAuth 从宿主下发的 StorageJSON 还原账号。
//
// 宿主把落盘文件原样透传在 StorageJSON 里；这里必须校验
// cookie_header 非空——没有 Cookie 的请求打到上游只会拿到登录页，
// 在入口处失败比让用户等到超时更容易定位。
// [参数] raw: StorageJSON。
// [返回] 账号与错误。
// 最近修改时间：2026-10-18；改动原因：新增，执行请求的账号还原。
func parseStoredAuth(raw []byte) (*StoredAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("执行请求缺少账号存储")
	}
	var sa StoredAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("账号存储解析失败：%w", err)
	}
	if strings.TrimSpace(sa.CookieHeader) == "" {
		return nil, fmt.Errorf("账号缺少会话 Cookie，请重新授权")
	}
	return &sa, nil
}
