package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件验证 OpenAI 协议转换。
//
// 重点不在「JSON 能不能 marshal」，而在折叠规则与终止语义：
// 这两处一旦出错，用户看到的是错位回答或凭空消失的内容。

// TestBuildClawPromptSingleUser 验证单条 user 原样透传。
//
// 单条 user 是最常见形态；加角色前缀会让上游把「user: 」当成
// 正文的一部分，回答里出现莫名其妙的复述。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，透传规则。
func TestBuildClawPromptSingleUser(t *testing.T) {
	prompt, err := buildClawPrompt([]chatMessage{
		{Role: "user", Content: json.RawMessage(`"你好"`)},
	})
	if err != nil {
		t.Fatalf("折叠失败: %v", err)
	}
	if prompt != "你好" {
		t.Errorf("prompt = %q，期望 你好", prompt)
	}
}

// TestBuildClawPromptMultiTurn 验证多轮按角色拼接。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，多轮折叠。
func TestBuildClawPromptMultiTurn(t *testing.T) {
	prompt, err := buildClawPrompt([]chatMessage{
		{Role: "system", Content: json.RawMessage(`"你是助手"`)},
		{Role: "user", Content: json.RawMessage(`"第一问"`)},
		{Role: "assistant", Content: json.RawMessage(`"第一答"`)},
		{Role: "user", Content: json.RawMessage(`"第二问"`)},
	})
	if err != nil {
		t.Fatalf("折叠失败: %v", err)
	}
	for _, want := range []string{"system: 你是助手", "user: 第一问", "assistant: 第一答", "user: 第二问"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt 缺少 %q，实际:\n%s", want, prompt)
		}
	}
	// 末尾必须是 user，否则上游会把整段当成新的用户输入。
	if !strings.HasSuffix(prompt, "user: 第二问") {
		t.Errorf("prompt 未以 user 结尾:\n%s", prompt)
	}
}

// TestBuildClawPromptMultimodalContent 验证数组形态只取 text 分片。
//
// 图片分片拼进去会变成一段无意义 JSON，上游无法消费。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，多模态归一。
func TestBuildClawPromptMultimodalContent(t *testing.T) {
	content := json.RawMessage(`[{"type":"text","text":"描述这张图"},{"type":"image_url","image_url":{"url":"http://x"}}]`)
	prompt, err := buildClawPrompt([]chatMessage{{Role: "user", Content: content}})
	if err != nil {
		t.Fatalf("折叠失败: %v", err)
	}
	if prompt != "描述这张图" {
		t.Errorf("prompt = %q，期望只含 text 分片", prompt)
	}
}

// TestBuildClawPromptRejectsEmpty 验证空请求被拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，空输入守卫。
func TestBuildClawPromptRejectsEmpty(t *testing.T) {
	cases := [][]chatMessage{
		nil,
		{{Role: "user", Content: json.RawMessage(`""`)}},
		{{Role: "assistant", Content: json.RawMessage(`""`)}},
	}
	for i, messages := range cases {
		if _, err := buildClawPrompt(messages); err == nil {
			t.Errorf("用例 %d 应当报错", i)
		}
	}
}

// TestBuildChunkShapes 验证分片的三种形态。
//
// 带内容不结束、带内容且结束、无内容只结束——三者语义不同，
// 混淆会导致客户端提前终止或永远等不到结束。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，分片语义。
func TestBuildChunkShapes(t *testing.T) {
	stop := openAIFinishStop

	t.Run("内容帧未结束", func(t *testing.T) {
		raw, err := buildChunk("id1", "coze/m", "hi", nil)
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason *string        `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if chunk.Object != openAIObjectChatCompletionChunk {
			t.Errorf("object = %q", chunk.Object)
		}
		if chunk.Choices[0].Delta["content"] != "hi" {
			t.Errorf("delta.content = %v", chunk.Choices[0].Delta["content"])
		}
		if chunk.Choices[0].FinishReason != nil {
			t.Errorf("finish_reason 应为 null，实际 %v", *chunk.Choices[0].FinishReason)
		}
	})

	t.Run("空内容终止帧", func(t *testing.T) {
		raw, err := buildChunk("id1", "coze/m", "", &stop)
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if !strings.Contains(string(raw), `"finish_reason":"stop"`) {
			t.Errorf("缺少 finish_reason: %s", raw)
		}
		// 空 delta 不应带 content 键。
		if strings.Contains(string(raw), `"content"`) {
			t.Errorf("空 delta 不应含 content: %s", raw)
		}
	})
}

// TestBuildFirstChunkHasRole 验证首帧声明 assistant 角色。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，首帧契约。
func TestBuildFirstChunkHasRole(t *testing.T) {
	raw, err := buildFirstChunk("id1", "coze/m")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if !strings.Contains(string(raw), `"role":"assistant"`) {
		t.Errorf("首帧缺少 role: %s", raw)
	}
}

// TestBuildCompletionShape 验证非流式响应结构。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，非流式契约。
func TestBuildCompletionShape(t *testing.T) {
	raw, err := buildCompletion("id1", "coze/m", "完整回答")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Object != openAIObjectChatCompletion {
		t.Errorf("object = %q", resp.Object)
	}
	if resp.Choices[0].Message.Content != "完整回答" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != openAIFinishStop {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

// TestParseChatRequest 验证请求解析。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，请求解析。
func TestParseChatRequest(t *testing.T) {
	raw := []byte(`{"model":"coze/glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := parseChatRequest(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if req.Model != "coze/glm-5.3-flash" || !req.Stream || len(req.Messages) != 1 {
		t.Errorf("解析结果不符: %+v", req)
	}

	if _, errEmpty := parseChatRequest(nil); errEmpty == nil {
		t.Error("空请求体应当报错")
	}
	if _, errBad := parseChatRequest([]byte("not json")); errBad == nil {
		t.Error("非法 JSON 应当报错")
	}
}

// TestBuildImageMarkdown 验证图片段落拼接规则。
//
// 图片必须是 Markdown 语法：客户端与 new-api 聊天页都按 Markdown 渲染，
// 裸 URL 不会自动变成可预览图片。多图必须各占一行，否则会挤成一串。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，图片下发格式契约。
func TestBuildImageMarkdown(t *testing.T) {
	if got := buildImageMarkdown(nil); got != "" {
		t.Errorf("无图片应返回空串，实际 %q", got)
	}
	if got := buildImageMarkdown([]string{"  "}); got != "" {
		t.Errorf("空白地址应被忽略，实际 %q", got)
	}

	single := buildImageMarkdown([]string{"https://a/1.jpg"})
	if single != "![image](https://a/1.jpg)" {
		t.Errorf("单图 = %q", single)
	}

	multi := buildImageMarkdown([]string{"https://a/1.jpg", "", "https://a/2.jpg"})
	want := "![image](https://a/1.jpg)\n![image](https://a/2.jpg)"
	if multi != want {
		t.Errorf("多图 = %q，期望 %q", multi, want)
	}
}
