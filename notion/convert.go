package main

// 本文件负责 OpenAI 请求解析与响应构造。
//
// 请求体一律优先读 Payload：宿主会按插件声明的 executor_input_formats
// 把 Payload 翻译成 OpenAI chat 形态；OriginalRequest 永远是客户端原样，
// 走 /v1/responses 时它只有 input 字段、没有 messages，读它会让入口
// 直接失败，并被宿主放大成账号级不可用。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// openAIChatRequest 是 OpenAI chat/completions 请求的字段子集。
type openAIChatRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages []openAIMessage `json:"messages"`
	// MaxTokens 与 MaxCompletionTokens 都是常见字段名，取先出现的非零值。
	MaxTokens           int `json:"max_tokens"`
	MaxCompletionTokens int `json:"max_completion_tokens"`
	// MessagesText 是拍平后的纯文本消息，不参与 JSON 编解码。
	MessagesText []chatMessage `json:"-"`
}

// openAIMessage 是单条消息。
//
// content 既可能是字符串，也可能是多模态数组；本插件只处理文本，
// 数组形态由 extractTextContent 拍平后取纯文本部分。
type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseChatRequest 解析请求体。
//
// 优先 Payload，仅在它为空时回退 OriginalRequest。
// [参数] payload: 宿主下发的优先请求体；original: 客户端原始请求体。
// [返回] 解析后的请求与错误。
// 最近修改时间：2026-10-18；改动原因：新增，请求解析入口。
func parseChatRequest(payload, original []byte) (*openAIChatRequest, error) {
	body := payload
	if len(body) == 0 {
		body = original
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}

	var req openAIChatRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("解析请求体失败：%w", errUnmarshal)
	}

	var messages []chatMessage
	for _, msg := range req.Messages {
		text := extractTextContent(msg.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		messages = append(messages, chatMessage{Role: msg.Role, Content: text})
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("请求缺少 messages")
	}
	// 归一后回写，后续逻辑只认纯文本消息。
	req.Messages = nil
	req.MessagesText = messages
	return &req, nil
}

// extractTextContent 把消息 content 拍平成纯文本。
//
// 支持三种形态：字符串、{"type":"text","text":"..."} 数组、
// 以及含 image_url 的混合数组（图片部分直接忽略，上游不支持附件）。
// [参数] raw: 原始 content。
// [返回] 纯文本。
// 最近修改时间：2026-10-18；改动原因：新增，多模态拍平。
func extractTextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil {
		return text
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if errUnmarshal := json.Unmarshal(raw, &parts); errUnmarshal == nil {
		var builder strings.Builder
		for _, part := range parts {
			if part.Type == "text" || part.Text != "" {
				builder.WriteString(part.Text)
			}
		}
		return builder.String()
	}
	return ""
}

// openAIUsage 是响应里的 token 统计。
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// completionResponse 是非流式响应体。
type completionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage openAIUsage `json:"usage"`
}

// buildCompletionResponse 构造非流式响应。
// [参数] model: 对外模型名；text: 正文；inputTokens: 输入 token；outputTokens: 输出 token。
// [返回] 响应结构与序列化字节。
// 最近修改时间：2026-10-18；改动原因：新增，非流式响应构造。
func buildCompletionResponse(model, text string, inputTokens, outputTokens int) ([]byte, error) {
	resp := completionResponse{
		ID:      "chatcmpl-" + strings.TrimPrefix(newUUID(), "-")[:24],
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Usage: openAIUsage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
	}
	choice := struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}{Index: 0, FinishReason: "stop"}
	choice.Message.Role = "assistant"
	choice.Message.Content = text
	resp.Choices = append(resp.Choices, choice)

	return marshalNoHTMLEscape(resp)
}

// streamChunk 是 SSE 分片。
//
// Usage 只在终止帧出现（OpenAI 规范用 `stream_options.include_usage` 控制，
// 但宿主只从分片里读 token 数，不输出就会导致下游用量全记为 0）。
type streamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role,omitempty"`
			Content          string `json:"content,omitempty"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			Reasoning        string `json:"reasoning,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage,omitempty"`
}

// buildStreamDelta 构造正文增量分片。
// [参数] id: 分片 id（整轮保持一致）；created: 创建时间；model: 模型名；text: 增量文本。
// [返回] 分片字节。
// 最近修改时间：2026-10-18；改动原因：新增，正文增量分片。
func buildStreamDelta(id string, created int64, model, text string) []byte {
	chunk := newStreamChunk(id, created, model)
	chunk.Choices[0].Delta.Content = text
	return mustMarshal(chunk)
}

// buildStreamReasoning 构造思考增量分片。
//
// 同时填 reasoning_content 与 reasoning 两个字段：不同客户端认的键不同，
// 双写能让主流客户端都识别出推理内容。
// [参数] id: 分片 id；created: 创建时间；model: 模型名；text: 增量文本。
// [返回] 分片字节。
// 最近修改时间：2026-10-18；改动原因：新增，思考增量分片。
func buildStreamReasoning(id string, created int64, model, text string) []byte {
	chunk := newStreamChunk(id, created, model)
	chunk.Choices[0].Delta.ReasoningContent = text
	chunk.Choices[0].Delta.Reasoning = text
	return mustMarshal(chunk)
}

// buildStreamFinish 构造收尾分片。
//
// 同时带上 usage：宿主只从分片/响应体取 token 数，终止帧不带用量会让
// 下游统计与计费全部记零。
// [参数] id: 分片 id；created: 创建时间；model: 模型名；inputTokens / outputTokens: 用量。
// [返回] 分片字节。
// 最近修改时间：2026-10-19；改动原因：补 usage，修复下游用量记零。
func buildStreamFinish(id string, created int64, model string, inputTokens, outputTokens int) []byte {
	chunk := newStreamChunk(id, created, model)
	reason := "stop"
	chunk.Choices[0].FinishReason = &reason
	chunk.Usage = &openAIUsage{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
	}
	return mustMarshal(chunk)
}

// newStreamChunk 构造带首帧角色的分片骨架。
// [参数] id: 分片 id；created: 创建时间；model: 模型名。
// [返回] 分片。
// 最近修改时间：2026-10-18；改动原因：新增，分片骨架构造。
func newStreamChunk(id string, created int64, model string) *streamChunk {
	chunk := &streamChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
	}
	chunk.Choices = append(chunk.Choices, struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role,omitempty"`
			Content          string `json:"content,omitempty"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			Reasoning        string `json:"reasoning,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}{Index: 0})
	return chunk
}

// buildStreamRole 构造首帧角色分片。
// [参数] id: 分片 id；created: 创建时间；model: 模型名。
// [返回] 分片字节。
// 最近修改时间：2026-10-18；改动原因：新增，首帧 role 声明。
func buildStreamRole(id string, created int64, model string) []byte {
	chunk := newStreamChunk(id, created, model)
	chunk.Choices[0].Delta.Role = "assistant"
	return mustMarshal(chunk)
}

// mustMarshal 序列化且忽略错误（仅用于内部已知结构）。
// [参数] v: 待序列化值。
// [返回] JSON 字节。
// 最近修改时间：2026-10-18；改动原因：新增，内部序列化辅助。
func mustMarshal(v any) []byte {
	raw, _ := marshalNoHTMLEscape(v)
	return raw
}
