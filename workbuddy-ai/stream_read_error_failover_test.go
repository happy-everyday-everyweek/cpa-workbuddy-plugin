package main

// 断流换号回归：上游在响应头已返回后复位连接（scanner.Err 非空）时，
// 只要客户端尚未收到任何分片就必须换号续试；已泄漏分片时换号会产生
// 双份输出，只能透传错误。决策逻辑收敛在 shouldRotateOnStreamReadError，
// 本文件只覆盖该纯函数与错误文本识别，不触 host RPC。

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildStreamTerminalChunk 验证终止分片形态：只带 finish_reason、空 delta，
// 且序列化后能被下游按 OpenAI chunk 解析。
func TestBuildStreamTerminalChunk(t *testing.T) {
	raw, err := buildStreamTerminalChunk("qwen-3.8-flash", streamTruncatedFinishReason)
	if err != nil {
		t.Fatalf("buildStreamTerminalChunk error: %v", err)
	}
	var chunk struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string         `json:"finish_reason"`
			Delta        map[string]any `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &chunk); err != nil {
		t.Fatalf("terminal chunk not valid JSON: %v (%s)", err, raw)
	}
	if chunk.Object != "chat.completion.chunk" {
		t.Fatalf("object = %q, want chat.completion.chunk", chunk.Object)
	}
	if chunk.Model != "qwen-3.8-flash" {
		t.Fatalf("model = %q, want qwen-3.8-flash", chunk.Model)
	}
	if len(chunk.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(chunk.Choices))
	}
	if chunk.Choices[0].FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", chunk.Choices[0].FinishReason)
	}
	if len(chunk.Choices[0].Delta) != 0 {
		t.Fatalf("delta should be empty, got %v", chunk.Choices[0].Delta)
	}
	if streamChunkHasContent(string(raw)) {
		t.Fatal("terminal chunk must not be treated as content leak")
	}
}

// TestBuildStreamTerminalChunkFraming 验证 SSE 入口与非 SSE 入口的前缀差异：
// 跨格式转译要求 payload 自带 "data: "，原生 chat-completions 透传则由宿主补。
func TestBuildStreamTerminalChunkFraming(t *testing.T) {
	raw, err := buildStreamTerminalChunk("qwen-3.8-flash", streamTruncatedFinishReason)
	if err != nil {
		t.Fatalf("buildStreamTerminalChunk error: %v", err)
	}
	if strings.HasPrefix(string(raw), "data: ") {
		t.Fatalf("raw chunk must not carry SSE prefix: %s", raw)
	}
	framed := append([]byte("data: "), raw...)
	if !strings.HasPrefix(string(framed), "data: ") {
		t.Fatalf("framed chunk missing prefix: %s", framed)
	}
	if !json.Valid(framed[len("data: "):]) {
		t.Fatalf("framed payload is not valid JSON: %s", framed)
	}
}

// TestIsStreamReadError 验证断流文本识别命中真实传输层错误，且不误伤业务错误。
func TestIsStreamReadError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"扫描器断流原文", "upstream stream read error: read tcp 10.0.0.1:443: connection reset by peer", true},
		{"仅错误码文本", "INTERNAL_ERROR", true},
		{"标准断流前缀", "upstream stream read error: unexpected EOF", true},
		{"普通4xx文本", "upstream 403: 10605: isQueued", false},
		{"额度耗尽文本", "insufficient credit", false},
		{"已泄漏断流标记", "upstream stream read error: connection reset by peer (" + streamReadErrorLeakedMarker + ")", false},
		{"空响应体", "", false},
		{"纯空白", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStreamReadError(tc.body); got != tc.want {
				t.Fatalf("isStreamReadError(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestShouldRotateOnStreamReadError 验证换号决策的边界：正文零泄漏换号、
// 仅思考分片仍可换号、正文已泄漏不换号、预算耗尽不换号、缺少请求体或候选不换号。
func TestShouldRotateOnStreamReadError(t *testing.T) {
	cases := []struct {
		name           string
		emitted        bool
		contentEmitted bool
		attempt        int
		budget         int
		hasEncodedBody bool
		hasNextAuth    bool
		want           bool
	}{
		{"零泄漏且有预算与候选", false, false, 0, 10, true, true, true},
		{"仅思考分片仍可换号", true, false, 0, 10, true, true, true},
		{"正文已泄漏不换号", true, true, 0, 10, true, true, false},
		{"正文泄漏且无候选不换号", true, true, 0, 10, true, false, false},
		{"预算耗尽不换号", false, false, 10, 10, true, true, false},
		{"超出预算不换号", false, false, 11, 10, true, true, false},
		{"无可用候选不换号", false, false, 0, 10, true, false, false},
		{"无法重建请求体不换号", false, false, 0, 10, false, true, false},
		{"零预算不换号", false, false, 0, 0, true, true, false},
		{"末次尝试仍可换号", false, false, 9, 10, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRotateOnStreamReadError(tc.emitted, tc.contentEmitted, tc.attempt, tc.budget, tc.hasEncodedBody, tc.hasNextAuth)
			if got != tc.want {
				t.Fatalf("shouldRotateOnStreamReadError(emitted=%v, contentEmitted=%v, attempt=%d, budget=%d, body=%v, next=%v) = %v, want %v",
					tc.emitted, tc.contentEmitted, tc.attempt, tc.budget, tc.hasEncodedBody, tc.hasNextAuth, got, tc.want)
			}
		})
	}
}

// TestStreamChunkHasContent 验证分片正文判定：思考分片不算正文，
// 正文、工具调用与 refusal 才算，避免把「仅思考已下发」误判为正文泄漏。
func TestStreamChunkHasContent(t *testing.T) {
	cases := []struct {
		name      string
		chunkJSON string
		want      bool
	}{
		{"仅思考分片", `{"choices":[{"delta":{"reasoning_content":"想一想"}}]}`, false},
		{"仅 reasoning 分片", `{"choices":[{"delta":{"reasoning":"think"}}]}`, false},
		{"空 content 分片", `{"choices":[{"delta":{"content":""}}]}`, false},
		{"null content 分片", `{"choices":[{"delta":{"content":null,"reasoning_content":"想"}}]}`, false},
		{"正文分片", `{"choices":[{"delta":{"content":"答案"}}]}`, true},
		{"正文与思考同时存在", `{"choices":[{"delta":{"content":"答案","reasoning_content":"想"}}]}`, true},
		{"工具调用分片", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`, true},
		{"空工具调用数组", `{"choices":[{"delta":{"tool_calls":[]}}]}`, false},
		{"refusal 分片", `{"choices":[{"delta":{"refusal":"拒绝"}}]}`, true},
		{"无 choices 的 usage 帧", `{"usage":{"total_tokens":10}}`, false},
		{"非法 JSON", `not-json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamChunkHasContent(tc.chunkJSON); got != tc.want {
				t.Fatalf("streamChunkHasContent(%q) = %v, want %v", tc.chunkJSON, got, tc.want)
			}
		})
	}
}
