package main

import (
	"encoding/json"
	"testing"
)

// TestBuildStreamFinishIncludesUsage 校验终止帧带 usage。
//
// 宿主只从分片/响应体读 token 数；终止帧不带 usage 会让下游用量与计费
// 全部记零（coze 0.4.1 踩过的同类问题）。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，用量输出回归。
func TestBuildStreamFinishIncludesUsage(t *testing.T) {
	raw := buildStreamFinish("chatcmpl-test", 1700000000, "notion/omodi", 100, 20)

	var chunk struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
		t.Fatalf("终止帧不是合法 JSON：%v", errUnmarshal)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].FinishReason == nil || *chunk.Choices[0].FinishReason != "stop" {
		t.Fatalf("终止帧 finish_reason 不正确：%s", raw)
	}
	if chunk.Usage == nil {
		t.Fatal("终止帧缺少 usage")
	}
	if chunk.Usage.PromptTokens != 100 || chunk.Usage.CompletionTokens != 20 || chunk.Usage.TotalTokens != 120 {
		t.Fatalf("usage 数值不正确：%+v", chunk.Usage)
	}
}

// TestBuildStreamDeltaOmitsUsage 校验增量帧不带 usage。
//
// usage 只应在终止帧出现：每帧都带会让下游重复累加。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，避免用量重复累加。
func TestBuildStreamDeltaOmitsUsage(t *testing.T) {
	raw := buildStreamDelta("chatcmpl-test", 1700000000, "notion/omodi", "OK")

	var probe map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &probe); errUnmarshal != nil {
		t.Fatalf("增量帧不是合法 JSON：%v", errUnmarshal)
	}
	if _, has := probe["usage"]; has {
		t.Fatalf("增量帧不应带 usage：%s", raw)
	}
}
