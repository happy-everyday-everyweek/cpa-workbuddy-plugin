package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 本文件实现 OpenAI 协议与扣子 claw 之间的转换。
//
// 边界很窄：claw 的对话接口只接受「一条纯文本消息」，没有多轮 messages、
// 没有 tools、没有图片（多模态走独立模型，不在 CYCLE-01 范围）。
// 因此这里的转换是**有损**的：把 OpenAI 的 messages 折叠成一段文本。
//
// 为什么折叠而不是报错：客户端（Cherry Studio、new-api 等）总会带
// system + 历史轮次；直接报「不支持」会让模型完全不可用。折叠能让
// 对话先跑通，代价是丢失角色边界——这个取舍记录在 README。

const (
	// openAIObjectChatCompletion 是非流式响应对象名。
	openAIObjectChatCompletion = "chat.completion"

	// openAIObjectChatCompletionChunk 是流式分片对象名。
	openAIObjectChatCompletionChunk = "chat.completion.chunk"

	// openAIFinishStop 是正常结束的 finish_reason。
	openAIFinishStop = "stop"

	// openAIFinishLength 是正文已泄漏但上游断流时的收尾原因。
	//
	// 用 length 而不是 error：客户端已收到部分正文，发 error 会把
	// 已生成内容一起作废，且 new-api 无法据此计费（本仓库既有教训）。
	openAIFinishLength = "length"
)

// chatCompletionRequest 是 OpenAI 聊天请求的最小可用子集。
//
// 只解析本插件会用到的字段：model 决定上游模型，messages 折叠成文本，
// stream 决定返回形态。其余字段（temperature / tools / response_format）
// 上游不支持，静默忽略比报错更实用。
type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// chatMessage 是 OpenAI 单条消息。
//
// content 在 OpenAI 规范里可以是字符串或分片数组（多模态），
// 这里用 json.RawMessage 接住两种形态，再由 messageText 归一。
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// messageText 把 content 归一成纯文本。
//
// 数组形态只取 type=text 的分片：图片/音频分片上游无法消费，
// 拼进去会变成一段无意义的 JSON 噪声。
// [参数] m: 单条消息。
// [返回] 文本内容。
// 最近修改时间：2026-10-18；改动原因：新增，多模态内容归一。
func (m chatMessage) messageText() string {
	raw := strings.TrimSpace(string(m.Content))
	if raw == "" || raw == "null" {
		return ""
	}

	// 字符串形态是最常见情况，先试它。
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		return strings.TrimSpace(text)
	}

	// 数组形态：只收 text 分片。
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			builder.WriteString(part.Text)
		}
	}
	return strings.TrimSpace(builder.String())
}

// buildClawPrompt 把 OpenAI messages 折叠成一段发给 claw 的文本。
//
// 折叠规则：
//   - 只有一条 user 消息时原样发送（最常见，保留最干净的语义）；
//   - 多条时按「角色: 内容」逐行拼接，末尾以 user 内容收尾。
//
// 为什么末尾必须是 user：claw 的 message/chat 语义是「用户发一句话」，
// 以 assistant 结尾会让上游把整段当成新的用户输入，产生错位回答。
// [参数] messages: OpenAI 消息列表。
// [返回] 折叠后的文本与错误。
// 最近修改时间：2026-10-18；改动原因：新增，请求折叠。
func buildClawPrompt(messages []chatMessage) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("请求缺少 messages")
	}

	// 单条 user 是最常见的形态，直接透传，避免加上多余的角色前缀。
	if len(messages) == 1 && messages[0].Role == "user" {
		text := messages[0].messageText()
		if text == "" {
			return "", fmt.Errorf("user 消息内容为空")
		}
		return text, nil
	}

	var lines []string
	for _, msg := range messages {
		text := msg.messageText()
		if text == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		lines = append(lines, role+": "+text)
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("messages 全部为空")
	}
	return strings.Join(lines, "\n"), nil
}

// newCompletionID 生成一个响应 ID。
//
// 前缀与 OpenAI 保持一致，便于客户端解析；内容只需唯一，
// 不需要可反解。
// [参数] 无。
// [返回] 响应 ID。
// 最近修改时间：2026-10-18；改动原因：新增，响应标识。
func newCompletionID() string {
	return fmt.Sprintf("chatcmpl-coze-%d", time.Now().UnixNano())
}

// buildChunk 构造一个流式分片。
//
// delta 为 nil 时产出空 delta（用于终止帧），finishReason 为 nil 时
// 表示流未结束。两者独立，因为 OpenAI 允许「带内容的帧不结束」
// 与「不带内容的帧结束」。
// usage 只在终止帧携带：OpenAI 约定流式用量放在最后一帧，
// 中间帧带 usage 会被客户端当作重复计费。
// [参数] id: 响应 ID；model: 模型名；delta: 增量文本，空串表示无内容；
// finishReason: 终止原因，nil 表示未结束；usage: 用量对象，nil 表示不带。
// [返回] JSON 分片与错误。
// 最近修改时间：2026-10-18；改动原因：新增 usage 输出，修复下游全部记零。
func buildChunk(id, model, delta string, finishReason *string, usage map[string]any) ([]byte, error) {
	deltaPayload := map[string]any{}
	if delta != "" {
		deltaPayload["content"] = delta
	}

	chunk := map[string]any{
		"id":      id,
		"object":  openAIObjectChatCompletionChunk,
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         deltaPayload,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return json.Marshal(chunk)
}

// buildFirstChunk 构造首个分片（带 assistant 角色声明）。
//
// 部分客户端依赖首帧的 role 字段决定渲染方式；缺失时会退化成
// 「只有文本」的降级路径，表现为没有头像或无法折叠。
// [参数] id: 响应 ID；model: 模型名。
// [返回] JSON 分片与错误。
// 最近修改时间：2026-10-18；改动原因：新增，首帧角色声明。
func buildFirstChunk(id, model string) ([]byte, error) {
	chunk := map[string]any{
		"id":      id,
		"object":  openAIObjectChatCompletionChunk,
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{"role": "assistant"},
				"finish_reason": nil,
			},
		},
	}
	return json.Marshal(chunk)
}

// buildCompletion 构造非流式响应。
//
// usage 必须带上：宿主与 new-api 都从响应体的 usage 字段取 token 数，
// 缺失时整条请求记 0 用量、new-api 显示 `Tokens: -` 且无法计费。
// [参数] id: 响应 ID；model: 模型名；content: 完整正文；usage: 用量对象。
// [返回] JSON 响应与错误。
// 最近修改时间：2026-10-18；改动原因：新增 usage 输出，修复下游全部记零。
func buildCompletion(id, model, content string, usage map[string]any) ([]byte, error) {
	response := map[string]any{
		"id":      id,
		"object":  openAIObjectChatCompletion,
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": openAIFinishStop,
			},
		},
	}
	if usage != nil {
		response["usage"] = usage
	}
	return json.Marshal(response)
}

// estimateTokens 估算一段文本的 token 数。
//
// 扣子没有 token 计数端点，帧里也不带用量，只能按字符特征估算：
// CJK 约 1 字 1 token，拉丁文约 4 字符 1 token。
// [参数] s: 文本。
// [返回] 估算 token 数；空串返回 0。
// 最近修改时间：2026-10-18；改动原因：新增，上游无真实用量。
func estimateTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// buildUsage 构造 OpenAI 规范的 usage 对象。
//
// estimated 是扩展字段：宿主与客户端都会忽略，但让人工排查能一眼区分
// 「上游返回的真实用量」与「本插件的估算值」，避免把估算值当实测数据。
// [参数] prompt: 提示词；completion: 回复正文。
// [返回] usage 对象。
// 最近修改时间：2026-10-18；改动原因：新增，用量对象构造。
func buildUsage(prompt, completion string) map[string]any {
	p := estimateTokens(prompt)
	c := estimateTokens(completion)
	return map[string]any{
		"prompt_tokens":     p,
		"completion_tokens": c,
		"total_tokens":      p + c,
		"estimated":         true,
	}
}

// buildImageMarkdown 把图片地址拼成 Markdown 图片段。
//
// 为什么用 Markdown 而不是自定义结构：宿主与下游客户端（Cherry Studio、
// new-api 聊天页）都按 Markdown 渲染，图片语法是唯一被普遍支持的形态；
// 返回 url 这种原始链接反而要客户端自己判断能否预览。
// 每张图独占一行，避免多图挤在同一行导致排版错乱。
// [参数] urls: 图片地址列表（顺序即展示顺序）。
// [返回] 拼接结果；无图片时返回空串。
// 最近修改时间：2026-10-18；改动原因：新增，图片下发格式。
func buildImageMarkdown(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, url := range urls {
		trimmed := strings.TrimSpace(url)
		if trimmed == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString("![image](")
		builder.WriteString(trimmed)
		builder.WriteString(")")
	}
	return builder.String()
}

// parseChatRequest 解析 OpenAI 聊天请求。
// [参数] raw: 请求体。
// [返回] 解析结果与错误。
// 最近修改时间：2026-10-18；改动原因：新增，请求解析入口。
func parseChatRequest(raw []byte) (*chatCompletionRequest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}
	var req chatCompletionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("请求解析失败：%w", err)
	}
	return &req, nil
}
