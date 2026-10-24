// body.go constructs the Qoder AI agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// keepTemplatePrompt 控制是否保留模板内嵌的 Qoder system 提示词。
//
// 模板 baseprompt.json 里带一段约 10657 token 的 Qoder CLI system prompt，
// 原实现无条件把它放在 messages 最前面，再把客户端消息追加在后面。
// 后果是客户端自己的 system prompt 根本发不到上游：模型先看到 Qoder 的角色
// 设定，而且实际上存在两份 system，通常以前面那份为准。
//
// 默认改为穿透（丢弃模板 system，只用客户端 messages）。如果上游因为缺少
// 那段提示词而行为异常（例如基础对话能力下降），把环境变量
// QODER_AI_KEEP_TEMPLATE_PROMPT 设为 1 / true / yes / on 即可恢复原行为。
func keepTemplatePrompt() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QODER_AI_KEEP_TEMPLATE_PROMPT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// cpaToUpstreamKey maps CPA-facing model names to upstream keys.
// Unknown names pass through unchanged (server silently routes to auto).
func cpaToUpstreamKey(cpaModel string) string {
	switch strings.ToLower(strings.TrimSpace(cpaModel)) {
	case "qoder-auto", "auto":
		return "auto"
	// Cantus
	case "cantus", "cmodel":
		return "cmodel"
	// 国际版套餐档位代号
	case "ultimate":
		return "ultimate"
	case "performance":
		return "performance"
	case "efficient":
		return "efficient"
	case "sonus", "smodel":
		return "smodel"
	// Qwen
	case "qwen3.8-max", "qwen3.8-max-preview", "qmodel_38max", "qmodel_preview":
		return "qmodel_38max"
	case "qwen3.8-flash", "qwen3.6-flash", "qfmodel", "q36fmodel":
		return "qfmodel"
	case "qwen3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qmodel":
		return "qmodel"
	// GLM
	case "glm-5.3", "gmodel", "glm-5.2", "gm51model":
		return "gmodel"
	case "glm-5.3-flash", "gfmodel":
		return "gfmodel"
	// Kimi
	case "kimi-k3", "kmodel_latest":
		return "kmodel_latest"
	case "kimi-k2.8-preview", "kimi-k2.7-code", "kmodel":
		return "kmodel"
	// DeepSeek
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-flash", "deepseek-v4-flash", "dfmodel":
		return "dfmodel"
	// MiniMax
	case "minimax-m3", "mmodel", "minimax-m2.7":
		return "mmodel"
	}
	return cpaModel
}

// openAIMessage is one message in the OpenAI chat completion format.
// Content 归一为字符串：纯文本请求原样接收，多模态部件数组由
// UnmarshalJSON 提取文本后拼接，使下游 buildQoderBody 无需感知差异。
// ToolCalls / ToolCallID / Name 承载工具调用与工具结果，供重建上游 messages 时保持配对。
type openAIMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCalls  []any  `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// openAIContentPart 是 OpenAI 多模态 content 数组中的一个部件。
type openAIContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// UnmarshalJSON 兼容 OpenAI 的两种 content 形态：纯字符串与部件数组。
// 上游 agent_chat_generation 的 messages 只接受文本 content，因此数组形态
// 仅提取 text / input_text 部件的文本（图片等非文本部件无对应位置，按空处理）。
// [参数] raw: 单条消息的原始 JSON。
// [返回] 解析成功返回 nil；content 为非法形态时返回错误。
func (m *openAIMessage) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []any           `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	text, err := decodeOpenAIContent(wire.Content)
	if err != nil {
		return err
	}
	m.Role = wire.Role
	m.Content = text
	m.ToolCalls = wire.ToolCalls
	m.ToolCallID = wire.ToolCallID
	m.Name = wire.Name
	return nil
}

// decodeOpenAIContent 把 OpenAI content 字段归一为文本。
func decodeOpenAIContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content must be a string or content parts array")
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" || part.Type == "input_text" {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
	}
	return strings.Join(texts, "\n"), nil
}

// alignToolsWithClient 按客户端真实声明对齐上游 body 的 tools 字段。
func alignToolsWithClient(base map[string]any, req *openAIRequest) {
	if req == nil || len(req.Tools) == 0 {
		delete(base, "tools")
		delete(base, "tool_choice")
		return
	}
	base["tools"] = req.Tools
	choice, suppress := normalizeToolChoice(req.ToolChoice)
	if suppress {
		delete(base, "tools")
		delete(base, "tool_choice")
		return
	}
	if choice != "" {
		base["tool_choice"] = choice
	} else {
		delete(base, "tool_choice")
	}
}

// normalizeToolChoice 把 OpenAI 各形态的 tool_choice 归一为上游可识别的字符串。
func normalizeToolChoice(tc any) (choice string, suppress bool) {
	switch v := tc.(type) {
	case nil:
		return "", false
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		switch s {
		case "none":
			return "", true
		case "auto", "required":
			return s, false
		case "":
			return "", false
		default:
			return strings.TrimSpace(v), false
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			return "", true
		case "auto", "required":
			return typ, false
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			name = strings.TrimSpace(name)
			if name != "" {
				return name, false
			}
			return "auto", false
		default:
			return "", false
		}
	default:
		return "", false
	}
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model           string          `json:"model"`
	Messages        []openAIMessage `json:"messages"`
	Stream          bool            `json:"stream"`
	MaxTokens       int64           `json:"max_tokens"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Tools           []any           `json:"tools"`
	ToolChoice      any             `json:"tool_choice"`
}

// extractLatestUserPrompt returns the content of the last user message,
// with safe fallback to the latest non-empty message or "continue" if none found.
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			return messages[i].Content
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Content) != "" {
			return messages[i].Content
		}
	}
	return "continue"
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
//
// messages 组装策略：
//   默认（穿透）：完全丢弃模板里的 system 条目，先放客户端自己的 system 消息，
//     再按原顺序放其余对话。客户端 system prompt 直接抵达上游。
//   QODER_AI_KEEP_TEMPLATE_PROMPT=1（旧行为）：模板 system 打头，客户端消息追加在后。
func buildQoderBody(req *openAIRequest, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"

	// model_config
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
	}

	// parameters: 按模型官方规格自适应计算放宽 max_tokens，避免长思维链被截断
	if params, ok := base["parameters"].(map[string]any); ok {
		mLower := strings.ToLower(modelKey)
		isReasoning := strings.Contains(mLower, "reasoner") || strings.Contains(mLower, "thinking") || (req != nil && req.ReasoningEffort != "" && req.ReasoningEffort != "none")
		val := int64(0)
		if req != nil && req.MaxTokens > 0 {
			val = int64(req.MaxTokens)
		}
		params["max_tokens"] = computeAdaptiveMaxTokens(modelKey, val, isReasoning)
	}

	// tools: 把工具声明权归还请求方，消除模板硬编码工具集导致的 agent 劫持。
	alignToolsWithClient(base, req)

	// chat_context.text.text + chat_context.extra.originalContent.text
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
			}
		}
	}

	// messages 组装
	var out []any
	if keepTemplatePrompt() {
		// 旧行为：保留模板 system，再追加客户端消息。
		if msgs, ok := base["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					if role, _ := mm["role"].(string); role == "system" {
						out = append(out, m)
					}
				}
			}
		}
	}
	// Append the actual conversation（穿透模式下这是唯一的消息来源）
	for _, m := range req.Messages {
		msg := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		out = append(out, msg)
	}
	base["messages"] = out

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		runes := []rune(prompt)
		if len(runes) > 30 {
			biz["name"] = string(runes[:30])
		} else {
			biz["name"] = prompt
		}
	}

	return json.Marshal(base)
}
