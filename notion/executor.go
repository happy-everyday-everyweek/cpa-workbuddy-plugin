package main

// 本文件实现 Executor 能力，把 OpenAI 请求接到 Notion 的推理链路。
//
// 一次请求的完整时序：
//  1. 解析请求体（Payload 优先），拍平成纯文本消息；
//  2. 取账号、定位 thread（首轮新建，后续复用 session）；
//  3. 构造 transcript 请求体（首轮与续写字段不同）；
//  4. POST /runInferenceTranscript，按 NDJSON patch 增量转发；
//  5. 收尾时校验正文非空，空流按软失败上抛以触发换号。
//
// 为什么流式必须异步：宿主拿到响应后立即开始向客户端转发，
// 同步阻塞会让客户端在整轮对话期间收不到任何字节。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// executorReplyTimeout 是一轮对话的硬上限。
	//
	// 长任务（深度检索、长文档）可能远超普通请求；这里与单次 HTTP 超时
	// 解耦，后者管建连与响应头，这里管整轮生成。
	executorReplyTimeout = 5 * time.Minute
)

// executorStreamRequest 是流式执行请求。
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// executorRequest 是非流式执行请求。
type executorRequest struct {
	pluginapi.ExecutorRequest
}

// streamResponse 是同步路径的流式响应。
//
// 宿主的 RPC 桥把 Chunks 定义成 slice 再转 channel；
// 直接返回 channel 会让 JSON 序列化失败。
type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// handleExecExecute 处理非流式执行请求。
// [参数] raw: 宿主请求载荷。
// [返回] 执行响应信封与错误。
// 最近修改时间：2026-10-18；改动原因：新增，非流式出口。
func handleExecExecute(raw []byte) ([]byte, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	auth, errAuth := resolveAuth(req.StorageJSON, req.AuthID)
	if errAuth != nil {
		return nil, errAuth
	}

	chatReq, errParse := parseChatRequest(req.Payload, req.OriginalRequest)
	if errParse != nil {
		return nil, errParse
	}

	result, errRun := runInferenceWithFailover(context.Background(), auth, chatReq, nil)
	if errRun != nil {
		return nil, errRun
	}

	body, errBuild := buildCompletionResponse(displayModel(chatReq.Model), result.text, result.inputTokens, result.outputTokens)
	if errBuild != nil {
		return nil, errBuild
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: body,
		Headers: http.Header{"Content-Type": {"application/json"}},
	})
}

// handleExecStream 处理流式执行请求。
//
// 宿主给了 stream_id 走异步 emit；没给则收集完一次性返回，
// 后者只用于测试或宿主降级路径。
// [参数] raw: 宿主请求载荷。
// [返回] 流式响应信封与错误。
// 最近修改时间：2026-10-18；改动原因：新增，流式出口。
func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	auth, errAuth := resolveAuth(req.StorageJSON, req.AuthID)
	if errAuth != nil {
		return nil, errAuth
	}

	chatReq, errParse := parseChatRequest(req.Payload, req.OriginalRequest)
	if errParse != nil {
		return nil, errParse
	}

	headers := streamHeaders()
	if req.StreamID == "" {
		chunks, errCollect := collectStreamChunks(auth, chatReq)
		if errCollect != nil {
			return nil, errCollect
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	go pumpNotionStream(req.StreamID, auth, chatReq)
	return okEnvelope(streamResponse{Headers: headers})
}

// resolveAuth 从宿主载荷里取出账号。
//
// AuthID 由宿主按凭据文件相对路径派生，形如 `notion-xxx.json`；
// 本地账号标识不带扩展名，因此比较前必须剥离，否则永远匹配不上。
// [参数] storageJSON: 账号 JSON；authID: 账号标识。
// [返回] 账号与错误。
// 最近修改时间：2026-10-19；改动原因：剥离 .json 后缀并跳过停用账号，修复别名路由后取不到号。
func resolveAuth(storageJSON []byte, authID string) (*StoredAuth, error) {
	if len(storageJSON) > 0 {
		var auth StoredAuth
		if errUnmarshal := json.Unmarshal(storageJSON, &auth); errUnmarshal == nil && auth.CookieHeader != "" {
			if auth.AuthID == "" {
				auth.AuthID = strings.TrimSuffix(strings.TrimSpace(authID), notionAuthFileSuffix)
			}
			if auth.SpaceID == "" && len(auth.Spaces) > 0 {
				auth.SpaceID = auth.Spaces[0].ID
			}
			if auth.SpaceID == "" {
				return nil, fmt.Errorf("账号缺少 space_id，请重新导入 Cookie")
			}
			return &auth, nil
		}
	}

	authID = strings.TrimSuffix(strings.TrimSpace(authID), notionAuthFileSuffix)
	if authID != "" {
		for _, auth := range listHostAuthFiles() {
			if auth.Disabled {
				continue
			}
			if auth.AuthID == authID {
				return auth, nil
			}
		}
	}
	return nil, fmt.Errorf("未找到可用账号")
}

// inferenceResult 是一次推理的汇总结果。
type inferenceResult struct {
	text         string
	reasoning    string
	inputTokens  int
	outputTokens int
	threadID     string
	model        string
}

// runInference 执行一次推理并按需转发增量。
//
// sink 为 nil 时只做收集；非 nil 时每产出一个增量就调用一次。
// 正文为空时不返回成功：上游偶发 200 + 空流，必须让调用方换号，
// 否则客户端会收到一条空回复并被当成正常结果。
// [参数] ctx: 上下文；auth: 账号；chatReq: 请求；sink: 增量回调。
// [返回] 汇总结果与错误。
// 最近修改时间：2026-10-18；改动原因：新增，推理主链路。
func runInference(ctx context.Context, auth *StoredAuth, chatReq *openAIChatRequest, sink func(streamEvent) error) (*inferenceResult, error) {
	messages, errMerge := mergeMessages(chatReq.MessagesText)
	if errMerge != nil {
		return nil, errMerge
	}

	cfg := currentConfig()
	model := normalizeNotionModel(chatReq.Model)

	// 续写判定：宿主会话已有 thread 且能匹配到同一账号时才复用。
	threadID, followUp := lookupSession(auth, chatReq.Model)
	var payload *inferenceRequest
	if followUp {
		prompt := lastUserMessage(messages)
		if strings.TrimSpace(prompt) == "" {
			prompt = "continue"
		}
		payload = buildFollowUpRequest(auth, model, threadID, prompt)
	} else {
		threadID = newUUID()
		payload = buildInferenceRequest(auth, model, threadID, firstTurnMessages(messages))
	}

	body, errMarshal := marshalInferenceRequest(payload)
	if errMarshal != nil {
		return nil, errMarshal
	}

	// 整轮超时与单次 HTTP 超时解耦：流式响应头很快返回，
	// 但正文可能生成很久，因此这里用 context 控制总时长。
	timeout := time.Duration(cfg.StreamTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = executorReplyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	scope := scopeForAuth(auth, cfg.ClientVersion)
	req, errReq := newNotionRequest(ctx, scope, "/runInferenceTranscript", body)
	if errReq != nil {
		return nil, errReq
	}

	// 流式请求不能设 Client.Timeout：它会按整个响应体计时，
	// 长回复会被中途切断。总时长由上面的 context 控制。
	client := newNotionHTTPClient(0)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("连接 Notion 失败：%w", errDo)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &notionAPIError{StatusCode: resp.StatusCode, Body: truncateForError(rawBody)}
	}

	// 上游可能在任意一轮下发滚动 Cookie；在这里捕获并写回，重启后仍有效。
	applySetCookies(auth, resp)

	parser := newStreamParser()
	var errParse error
	if sink == nil {
		_, _, errParse = parser.Parse(resp.Body)
	} else {
		_, errParse = parser.ParseStream(resp.Body, sink)
	}
	if errParse != nil && !isClientGone(errParse) {
		return nil, errParse
	}

	result := &inferenceResult{
		text:      parser.completionText(),
		reasoning: parser.reasoningText(),
		threadID:  firstNonEmpty(parser.threadID, threadID),
		model:     parser.finalModel(),
	}
	result.inputTokens, result.outputTokens = parser.usage()

	// 正文为空时区分两种情形：解析失败（参数正确但没有 agent-inference）
	// 与真正空流。两者都应换号，但错误文案要能区分以便排查。
	if strings.TrimSpace(result.text) == "" {
		// 续写空流多半是上游回收了线程：清掉会话，让下一轮重新建 thread，
		// 而不是让同一个失效 id 持续失败。
		if followUp {
			forgetSession(auth, chatReq.Model)
		}
		if !parser.sawInference {
			return nil, fmt.Errorf("Notion 未返回正文（上游只回了会话初始化）")
		}
		return nil, fmt.Errorf("Notion 返回空正文")
	}

	// 记录 thread 供后续轮复用。
	rememberSession(auth, chatReq.Model, result.threadID)
	return result, nil
}

// collectStreamChunks 收集完整流并切成宿主可用的分片。
// [参数] auth: 账号；chatReq: 请求。
// [返回] 分片列表与错误。
// 最近修改时间：2026-10-18；改动原因：新增，同步流式降级路径。
func collectStreamChunks(auth *StoredAuth, chatReq *openAIChatRequest) ([]pluginapi.ExecutorStreamChunk, error) {
	result, errRun := runInferenceWithFailover(context.Background(), auth, chatReq, nil)
	if errRun != nil {
		return nil, errRun
	}
	payload, errBuild := buildCompletionResponse(displayModel(chatReq.Model), result.text, result.inputTokens, result.outputTokens)
	if errBuild != nil {
		return nil, errBuild
	}
	return []pluginapi.ExecutorStreamChunk{{Payload: payload}}, nil
}

// pumpNotionStream 在后台跑完一轮对话并逐帧推送。
// [参数] streamID: 宿主流标识；auth: 账号；chatReq: 请求。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，异步流式主循环。
func pumpNotionStream(streamID string, auth *StoredAuth, chatReq *openAIChatRequest) {
	defer streamClose(streamID)

	model := displayModel(chatReq.Model)
	chunkID := "chatcmpl-" + strings.TrimPrefix(newUUID(), "-")[:24]
	created := time.Now().Unix()

	if errRole := streamEmit(streamID, buildStreamRole(chunkID, created, model)); errRole != nil {
		return
	}

	result, errRun := runInferenceWithFailover(context.Background(), auth, chatReq, func(event streamEvent) error {
		switch event.Kind {
		case "delta":
			return streamEmit(streamID, buildStreamDelta(chunkID, created, model, event.Text))
		case "thinking":
			return streamEmit(streamID, buildStreamReasoning(chunkID, created, model, event.Text))
		}
		return nil
	})
	if errRun != nil {
		// 错误帧必须显式下发：客户端只有收到 error 才会停止等待，
		// 否则会一直挂到自身超时。
		_ = streamEmit(streamID, buildStreamError(model, errRun.Error()))
		return
	}

	_ = streamEmit(streamID, buildStreamFinish(chunkID, created, model, result.inputTokens, result.outputTokens))
}

// buildStreamError 构造错误分片。
// [参数] model: 模型名；message: 错误说明。
// [返回] 分片字节。
// 最近修改时间：2026-10-18；改动原因：新增，流内错误下发。
func buildStreamError(model, message string) []byte {
	return mustMarshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    "notion_upstream_error",
		},
		"model": model,
	})
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

// streamEmit 向宿主推送一帧。
//
// payload 必须是裸 JSON 对象：宿主按请求格式决定是否补 `data: ` 前缀，
// 插件提前加前缀会让客户端收到双重包裹。
// [参数] streamID: 宿主流标识；payload: 一帧内容。
// [返回] 推送错误；客户端断开时宿主会返回错误。
// 最近修改时间：2026-10-18；改动原因：新增，异步流式唯一出口。
func streamEmit(streamID string, payload []byte) error {
	body, errMarshal := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	if errMarshal != nil {
		return errMarshal
	}
	_, errCall := hostCall(pluginabi.MethodHostStreamEmit, body)
	return errCall
}

// streamClose 关闭宿主流。
// [参数] streamID: 宿主流标识。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，异步流式收尾。
func streamClose(streamID string) {
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

// displayModel 返回对外展示的模型名。
// [参数] model: 宿主传入的模型名。
// [返回] 带 notion/ 前缀的模型 id。
// 最近修改时间：2026-10-18；改动原因：新增，响应模型名归一。
func displayModel(model string) string {
	return "notion/" + normalizeNotionModel(model)
}

// isClientGone 判断错误是否来自客户端断开。
// [参数] err: 错误。
// [返回] 是否为客户端取消。
// 最近修改时间：2026-10-18；改动原因：新增，避免把断开当上游失败记日志。
func isClientGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "context canceled") || strings.Contains(msg, "client disconnected")
}
