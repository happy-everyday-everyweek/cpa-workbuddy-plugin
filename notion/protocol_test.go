package main

// 本文件验证 transcript 请求体的协议契约。
//
// 重点是那 5 个「缺了就只回 patch-start」的顶层字段：它们是整条
// 推理链路能否产出正文的前提，必须由测试锁死。

import (
	"encoding/json"
	"strings"
	"testing"
)

// testAuth 构造测试账号。
// [参数] 无。
// [返回] 账号。
// 最近修改时间：2026-10-18；改动原因：新增，测试夹具。
func testAuth() *StoredAuth {
	return &StoredAuth{
		AuthID:       "notion-test",
		CookieHeader: "token_v2=t; notion_user_id=u",
		UserID:       "user-1",
		SpaceID:      "space-1",
	}
}

// TestFirstTurnRequestContract 校验首轮请求的关键字段。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，首轮协议契约回归。
func TestFirstTurnRequestContract(t *testing.T) {
	req := buildInferenceRequest(testAuth(), "omodi", "thread-1", []chatMessage{
		{Role: "user", Content: "你好"},
	})

	if !req.CreateThread {
		t.Fatal("首轮 createThread 必须为 true")
	}
	if req.IsPartialTranscript {
		t.Fatal("首轮 isPartialTranscript 必须为 false")
	}
	if !req.GenerateTitle {
		t.Fatal("首轮 generateTitle 必须为 true")
	}
	if !req.AsPatchResponse {
		t.Fatal("asPatchResponse 必须为 true")
	}
	if req.PatchResponseVersion != 2 {
		t.Fatalf("patchResponseVersion = %d, 期望 2", req.PatchResponseVersion)
	}
	if !req.SupportsCustomAgentNudgeTranscriptStep {
		t.Fatal("supportsCustomAgentNudgeTranscriptStep 必须为 true（缺失会只回 patch-start）")
	}
	if req.IsUserInAnySalesAssistedSpace || req.IsSpaceSalesAssisted {
		t.Fatal("sales-assisted 标记必须为 false")
	}
	if req.ThreadType != "workflow" {
		t.Fatalf("threadType = %q, 期望 workflow", req.ThreadType)
	}
	if req.ThreadParentPointer == nil {
		t.Fatal("首轮必须带 threadParentPointer（缺失会只回 patch-start）")
	}
	if req.ThreadParentPointer.Table != "space" || req.ThreadParentPointer.SpaceID != "space-1" {
		t.Fatalf("threadParentPointer 不正确: %+v", req.ThreadParentPointer)
	}
	if !req.SaveAllThreadOperations || !req.SetUnreadState {
		t.Fatal("saveAllThreadOperations / setUnreadState 必须为 true")
	}

	// transcript 结构：config + context + user。
	if len(req.Transcript) != 3 {
		t.Fatalf("transcript 步骤数 = %d, 期望 3", len(req.Transcript))
	}
	if req.Transcript[0]["type"] != "config" || req.Transcript[1]["type"] != "context" || req.Transcript[2]["type"] != "user" {
		t.Fatal("transcript 步骤类型顺序不正确")
	}
}

// TestUserStepUsesMatrixValue 校验 user 步使用二维字符串数组。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，value 编码回归。
func TestUserStepUsesMatrixValue(t *testing.T) {
	step := buildUserStep("user-1", "你好")

	value, ok := step["value"].([][]string)
	if !ok {
		t.Fatalf("value 必须是 [][]string, 实际 %T", step["value"])
	}
	if len(value) != 1 || len(value[0]) != 1 || value[0][0] != "你好" {
		t.Fatalf("value 内容不正确: %+v", value)
	}
	if step["userId"] != "user-1" {
		t.Fatalf("userId = %v", step["userId"])
	}
	if step["createdAt"] == nil || step["createdAt"] == "" {
		t.Fatal("createdAt 不能为空")
	}
}

// TestFollowUpRequestContract 校验续写请求与首轮的差异。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，续写协议契约回归。
func TestFollowUpRequestContract(t *testing.T) {
	req := buildFollowUpRequest(testAuth(), "omodi", "thread-1", "继续")

	if req.CreateThread {
		t.Fatal("续写 createThread 必须为 false")
	}
	if !req.IsPartialTranscript {
		t.Fatal("续写 isPartialTranscript 必须为 true")
	}
	if req.GenerateTitle {
		t.Fatal("续写 generateTitle 必须为 false")
	}
	if req.ThreadParentPointer != nil {
		t.Fatal("续写不应带 threadParentPointer")
	}
	if req.ThreadID != "thread-1" {
		t.Fatalf("threadId = %q, 期望 thread-1", req.ThreadID)
	}
	if len(req.Transcript) != 3 {
		t.Fatalf("续写 transcript 步骤数 = %d, 期望 3（config/context/新 user）", len(req.Transcript))
	}

	// 快照里必须仍带 5 个协议字段。
	if !req.SupportsCustomAgentNudgeTranscriptStep || req.PatchResponseVersion != 2 {
		t.Fatal("续写同样需要 supportsCustomAgentNudgeTranscriptStep 与 patchResponseVersion")
	}
}

// TestConfigValueHasWorkflowType 校验 config 关键字段。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，config 回归。
func TestConfigValueHasWorkflowType(t *testing.T) {
	cfg := buildConfigValue("omodi")

	if cfg["type"] != "workflow" {
		t.Fatalf("config.type = %v, 期望 workflow", cfg["type"])
	}
	if cfg["model"] != "omodi" {
		t.Fatalf("config.model = %v", cfg["model"])
	}
	if cfg["modelFromUser"] != true {
		t.Fatal("modelFromUser 必须为 true")
	}
	if _, ok := cfg["availableConnectors"]; !ok {
		t.Fatal("缺少 availableConnectors")
	}
}

// TestInferenceRequestSerializesAllContractFields 校验序列化后字段名正确。
//
// 单测只看 Go 字段是不够的：json tag 写错会让字段在网络层消失，
// 而 Go 侧完全看不出来。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，线格式回归。
func TestInferenceRequestSerializesAllContractFields(t *testing.T) {
	req := buildInferenceRequest(testAuth(), "omodi", "thread-1", []chatMessage{
		{Role: "user", Content: "hi"},
	})
	raw, errMarshal := marshalInferenceRequest(req)
	if errMarshal != nil {
		t.Fatalf("序列化失败: %v", errMarshal)
	}

	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("反序列化失败: %v", errUnmarshal)
	}

	requiredKeys := []string{
		"traceId", "spaceId", "threadId", "transcript",
		"createThread", "isPartialTranscript", "generateTitle",
		"saveAllThreadOperations", "setUnreadState", "asPatchResponse",
		"patchResponseVersion", "threadType",
		"supportsCustomAgentNudgeTranscriptStep",
		"isUserInAnySalesAssistedSpace", "isSpaceSalesAssisted",
		"threadParentPointer", "debugOverrides",
	}
	for _, key := range requiredKeys {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("序列化后缺少字段 %s", key)
		}
	}
}

// TestMergeMessagesFoldsSystemIntoFirstUser 校验 system 消息并入首条 user。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，消息合并回归。
func TestMergeMessagesFoldsSystemIntoFirstUser(t *testing.T) {
	messages, err := mergeMessages([]chatMessage{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "问题一"},
	})
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("消息数 = %d, 期望 1", len(messages))
	}
	if !strings.Contains(messages[0].Content, "你是助手") || !strings.Contains(messages[0].Content, "问题一") {
		t.Fatalf("system 未并入首条 user: %q", messages[0].Content)
	}
}

// TestMergeMessagesRejectsEmpty 校验空消息被拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，入口校验回归。
func TestMergeMessagesRejectsEmpty(t *testing.T) {
	if _, err := mergeMessages(nil); err == nil {
		t.Fatal("空消息应报错")
	}
	if _, err := mergeMessages([]chatMessage{{Role: "system", Content: "只有系统提示"}}); err != nil {
		t.Fatalf("只有 system 时应补成 user，实际报错: %v", err)
	}
}

// TestFirstTurnMessagesFoldsAssistant 校验历史助手回复折叠进下一条 user。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，历史折叠回归。
func TestFirstTurnMessagesFoldsAssistant(t *testing.T) {
	out := firstTurnMessages([]chatMessage{
		{Role: "user", Content: "问题一"},
		{Role: "assistant", Content: "回答一"},
		{Role: "user", Content: "问题二"},
	})

	if len(out) != 2 {
		t.Fatalf("步骤数 = %d, 期望 2", len(out))
	}
	if !strings.Contains(out[1].Content, "回答一") || !strings.Contains(out[1].Content, "问题二") {
		t.Fatalf("助手回复未折叠: %q", out[1].Content)
	}
}

// TestParseChatRequestPrefersPayload 校验请求体取值优先级。
//
// 读 OriginalRequest 会让 /v1/responses 入口必然失败，
// 并被宿主放大成账号级不可用。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，Payload 优先回归。
func TestParseChatRequestPrefersPayload(t *testing.T) {
	payload := []byte(`{"model":"omodi","messages":[{"role":"user","content":"来自 Payload"}]}`)
	original := []byte(`{"input":"来自 OriginalRequest"}`)

	req, errParse := parseChatRequest(payload, original)
	if errParse != nil {
		t.Fatalf("解析失败: %v", errParse)
	}
	if len(req.MessagesText) != 1 || !strings.Contains(req.MessagesText[0].Content, "Payload") {
		t.Fatalf("未优先使用 Payload: %+v", req.MessagesText)
	}
}

// TestParseChatRequestFallsBackToOriginal 校验 Payload 为空时回退。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，回退回归。
func TestParseChatRequestFallsBackToOriginal(t *testing.T) {
	original := []byte(`{"model":"omodi","messages":[{"role":"user","content":"来自原体"}]}`)
	req, errParse := parseChatRequest(nil, original)
	if errParse != nil {
		t.Fatalf("解析失败: %v", errParse)
	}
	if len(req.MessagesText) != 1 || !strings.Contains(req.MessagesText[0].Content, "原体") {
		t.Fatalf("未回退 OriginalRequest: %+v", req.MessagesText)
	}
}

// TestExtractTextContentMultimodal 校验多模态 content 拍平。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，多模态回归。
func TestExtractTextContentMultimodal(t *testing.T) {
	cases := map[string]string{
		`"纯文本"`: "纯文本",
		`[{"type":"text","text":"A"},{"type":"text","text":"B"}]`: "AB",
		`[{"type":"image_url","image_url":{"url":"http://x"}}]`:   "",
	}
	for input, want := range cases {
		if got := extractTextContent(json.RawMessage(input)); got != want {
			t.Fatalf("extractTextContent(%s) = %q, 期望 %q", input, got, want)
		}
	}
}
