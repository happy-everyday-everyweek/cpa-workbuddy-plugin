package main

import (
	"encoding/json"
	"testing"
)

func TestAdaptiveMaxTokens(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		requested   int64
		isReasoning bool
		wantTokens  int64
	}{
		{
			name:        "Qwen3.8-Flash 默认非思考 -> 抬升至 32768 最低输出底线",
			model:       "Qwen3.8-Flash",
			requested:   0,
			isReasoning: false,
			wantTokens:  32768,
		},
		{
			name:        "Qwen3.8-Flash 思考模式 + 传 8192 -> 提升至官方推理上限 32768",
			model:       "Qwen3.8-Flash",
			requested:   8192,
			isReasoning: true,
			wantTokens:  32768,
		},
		{
			name:        "GLM-5.3 思考模式 + 传 8192 -> 提升至官方推理上限 128000",
			model:       "GLM-5.3",
			requested:   8192,
			isReasoning: true,
			wantTokens:  128000,
		},
		{
			name:        "超限值 200000 -> 截断至官方允许上限 32768",
			model:       "qfmodel",
			requested:   200000,
			isReasoning: true,
			wantTokens:  32768,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeAdaptiveMaxTokens(tc.model, tc.requested, tc.isReasoning)
			if got != tc.wantTokens {
				t.Fatalf("computeAdaptiveMaxTokens(%s, %d, %v) = %d, want %d",
					tc.model, tc.requested, tc.isReasoning, got, tc.wantTokens)
			}
		})
	}
}

func TestWbModels_OfficialSpecs(t *testing.T) {
	models := exposeModelIDs(wbModels())
	for _, m := range models {
		if m.ID == "GLM-5.3" && m.MaxCompletionTokens != 128000 {
			t.Fatalf("GLM-5.3 MaxCompletionTokens = %d, want 128000", m.MaxCompletionTokens)
		}
		if m.ID == "Qwen3.8-Flash" && m.MaxCompletionTokens < 32768 {
			t.Fatalf("Qwen3.8-Flash MaxCompletionTokens = %d, want >= 32768", m.MaxCompletionTokens)
		}
	}
}

func TestBuildQoderBody_AdaptiveMaxTokens(t *testing.T) {
	// 1. 未传 max_tokens 时抬升至 32768 最低输出底线
	req1 := &openAIRequest{
		Model:    "Qwen3.8-Flash",
		Messages: []openAIMessage{{Role: "user", Content: "hi"}},
	}
	body1, err := buildQoderBody(req1, "qfmodel", "personal_standard")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	var parsed1 map[string]any
	if err := json.Unmarshal(body1, &parsed1); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mt := jsonI64(parsed1["parameters"].(map[string]any), "max_tokens"); mt != 32768 {
		t.Fatalf("parameters.max_tokens = %d, want 32768", mt)
	}

	// 2. 思考模式下传入保守小值 8192 自动放宽至 32768
	req2 := &openAIRequest{
		Model:           "Qwen3.8-Flash",
		Messages:        []openAIMessage{{Role: "user", Content: "hi"}},
		MaxTokens:       8192,
		ReasoningEffort: "high",
	}
	body2, err := buildQoderBody(req2, "qfmodel", "personal_standard")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	var parsed2 map[string]any
	if err := json.Unmarshal(body2, &parsed2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mt := jsonI64(parsed2["parameters"].(map[string]any), "max_tokens"); mt != 32768 {
		t.Fatalf("parameters.max_tokens = %d, want 32768", mt)
	}
}
