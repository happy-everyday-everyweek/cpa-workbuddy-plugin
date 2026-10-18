package main

// 本文件负责把 OpenAI 消息转换成 Notion 的 transcript 请求体。
//
// 协议要点（2026-10-18 真实凭据实测）：
//  1. user 步的 value 必须是二维字符串数组 [[文本]]，不是 [{type,content}]；
//  2. 首轮必须带 threadParentPointer，否则上游只回一行 patch-start 就 EOF；
//  3. createThread / isPartialTranscript / generateTitle 三者必须成对设置；
//  4. 另外四个字段（supportsCustomAgentNudgeTranscriptStep、
//     isUserInAnySalesAssistedSpace、isSpaceSalesAssisted、patchResponseVersion）
//     缺失时同样只得到空流。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// transcriptStep 是 transcript 数组里的一个步骤。
//
// 用 map 而不是强类型结构：不同 type 的步骤字段差异极大
// （config 有 50+ 开关，user 有 userId/createdAt，context 有时区），
// 强类型会让零值字段被序列化进请求体，上游会因此拒绝。
type transcriptStep map[string]any

// inferenceRequest 是 runInferenceTranscript 的请求体。
type inferenceRequest struct {
	TraceID                                string           `json:"traceId"`
	SpaceID                                string           `json:"spaceId"`
	ThreadID                               string           `json:"threadId"`
	Transcript                             []transcriptStep `json:"transcript"`
	CreateThread                           bool             `json:"createThread"`
	IsPartialTranscript                    bool             `json:"isPartialTranscript"`
	GenerateTitle                          bool             `json:"generateTitle"`
	SaveAllThreadOperations                bool             `json:"saveAllThreadOperations"`
	SetUnreadState                         bool             `json:"setUnreadState"`
	AsPatchResponse                        bool             `json:"asPatchResponse"`
	PatchResponseVersion                   int              `json:"patchResponseVersion"`
	ThreadType                             string           `json:"threadType"`
	SupportsCustomAgentNudgeTranscriptStep bool             `json:"supportsCustomAgentNudgeTranscriptStep"`
	IsUserInAnySalesAssistedSpace          bool             `json:"isUserInAnySalesAssistedSpace"`
	IsSpaceSalesAssisted                   bool             `json:"isSpaceSalesAssisted"`
	ThreadParentPointer                    *parentPointer   `json:"threadParentPointer,omitempty"`
	DebugOverrides                         debugOverrides   `json:"debugOverrides"`
}

// parentPointer 指向 thread 所属的 space。
type parentPointer struct {
	Table   string `json:"table"`
	ID      string `json:"id"`
	SpaceID string `json:"spaceId"`
}

// debugOverrides 是上游要求的调试开关。
type debugOverrides struct {
	EmitAgentSearchExtractedResults bool           `json:"emitAgentSearchExtractedResults"`
	CachedInferences                map[string]any `json:"cachedInferences"`
	AnnotationInferences            map[string]any `json:"annotationInferences"`
	EmitInferences                  bool           `json:"emitInferences"`
}

// chatMessage 是归一后的对话消息。
type chatMessage struct {
	Role    string
	Content string
}

// newUUID 生成标准 UUID v4。
// [参数] 无。
// [返回] 带连字符的 UUID 字符串。
// 最近修改时间：2026-10-18；改动原因：新增，transcript 各步骤 id。
func newUUID() string {
	var buf [16]byte
	if _, errRead := rand.Read(buf[:]); errRead != nil {
		// 退化为时间派生：UUID 只用于去重，不需要密码学强度。
		seed := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(seed >> (i % 8 * 8))
		}
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(buf[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32])
}

// buildConfigValue 构造 config 步骤的 value。
//
// 只填实测必要的开关：字段过多并不会让上游更配合，反而每多一个
// 未知字段就多一次 400 的风险。这里保留的是参考实现与实测共同确认
// 存在的字段。
// [参数] model: 上游内部代号。
// [返回] config 值。
// 最近修改时间：2026-10-18；改动原因：新增，config 最小可用集。
func buildConfigValue(model string) map[string]any {
	return map[string]any{
		"type":                           "workflow",
		"model":                          model,
		"modelFromUser":                  true,
		"enableAgentAutomations":         true,
		"enableAgentIntegrations":        true,
		"enableCustomAgents":             true,
		"enableExperimentalIntegrations": false,
		"enableScriptAgent":              true,
		"enableScriptAgentAdvanced":      false,
		"enableScriptAgentSearchConnectorsInCustomAgent": false,
		"enableScriptAgentGoogleDriveInCustomAgent":      false,
		"enableScriptAgentGoogleDriveOAuthInCustomAgent": false,
		"enableScriptAgentSlack":                         true,
		"enableScriptAgentMcpServers":                    true,
		"enableAgentDiffs":                               true,
		"enableCsvAttachmentSupport":                     true,
		"showDatabaseAgentsDiscoverability":              true,
		"enableAgentThreadTools":                         false,
		"enableCrdtOperations":                           false,
		"enableAgentCardCustomization":                   true,
		"enableSystemPromptAsPage":                       false,
		"enableUserSessionContext":                       false,
		"enableLargeToolResultComputerOffload":           false,
		"enableScriptAgentGtm":                           false,
		"enablePitCrewTableViewTool":                     false,
		"enableComputer":                                 true,
		"enableCustomAgentCreateGuidanceV2":              true,
		"enableSoftwareFactoryPage":                      false,
		"enableAgentGenerateImage":                       true,
		"enableQueryCalendar":                            false,
		"enableQueryMail":                                false,
		"enableMailExplicitToolCalls":                    true,
		"enableMailNotificationPreferences":              false,
		"enableMailAgentMultiProviderSupport":            true,
		"enableNotionMailDeprecated":                     false,
		"enableWebResearch":                              false,
		"useRulePrioritization":                          true,
		"useWebSearch":                                   false,
		"isHipaa":                                        false,
		"internetAccess":                                 false,
		"manageWorkers":                                  false,
		"useReadOnlyMode":                                false,
		"writerMode":                                     false,
		"isCustomAgent":                                  false,
		"isCustomAgentBuilder":                           false,
		"isCustomAgentCreate":                            false,
		"isAgentResearchRequest":                         false,
		"useCustomAgentDraft":                            false,
		"enableMarkdownVNext":                            false,
		"enableAgentSkillsV2":                            false,
		"updatePageStaleViewGuardEnabled":                false,
		"enableUpdatePageOrderUpdates":                   true,
		"enableAgentSupportPropertyReorder":              true,
		"enableAgentAskSurvey":                           true,
		"databaseAgentConfigMode":                        false,
		"isOnboardingAgent":                              false,
		"isMobile":                                       false,
		"availableConnectors":                            []any{},
	}
}

// buildContextValue 构造 context 步骤的 value。
// [参数] userID: 用户 id；spaceID: 空间 id；surface: 界面来源。
// [返回] context 值。
// 最近修改时间：2026-10-18；改动原因：新增，context 构造。
func buildContextValue(userID, spaceID, surface string) map[string]any {
	return map[string]any{
		"timezone":        "Asia/Shanghai",
		"userId":          userID,
		"spaceId":         spaceID,
		"currentDatetime": time.Now().Format(time.RFC3339Nano),
		"surface":         surface,
		"agentAccessory":  "paprika",
		"context_page_id": newUUID(),
	}
}

// buildUserStep 构造 user 步骤。
//
// value 必须是 [][]string：上游按「段落 → 富文本片段」两层解析，
// 传成 [{type,content}] 会被当成非法结构。
// [参数] userID: 用户 id；prompt: 用户文本。
// [返回] user 步骤。
// 最近修改时间：2026-10-18；改动原因：新增，user 步二维数组编码。
func buildUserStep(userID, prompt string) transcriptStep {
	return transcriptStep{
		"id":        newUUID(),
		"type":      "user",
		"value":     [][]string{{prompt}},
		"userId":    userID,
		"createdAt": time.Now().Format(time.RFC3339Nano),
	}
}

// buildInferenceRequest 组装首轮推理请求。
//
// 首轮与续写的差异集中在四个字段上，任何一处不配对都会导致空流：
// 首轮 createThread=true + isPartialTranscript=false + generateTitle=true
// 且必须带 threadParentPointer；续写则全部取反且不带 parentPointer。
// [参数] auth: 账号；model: 上游代号；threadID: 会话线程 id；messages: 对话消息。
// [返回] 请求体。
// 最近修改时间：2026-10-18；改动原因：新增，首轮请求组装。
func buildInferenceRequest(auth *StoredAuth, model, threadID string, messages []chatMessage) *inferenceRequest {
	steps := make([]transcriptStep, 0, len(messages)+2)
	steps = append(steps,
		transcriptStep{"id": newUUID(), "type": "config", "value": buildConfigValue(model)},
		transcriptStep{"id": newUUID(), "type": "context", "value": buildContextValue(auth.UserID, auth.SpaceID, "full_page_chat")},
	)
	for _, msg := range messages {
		steps = append(steps, buildUserStep(auth.UserID, msg.Content))
	}

	return &inferenceRequest{
		TraceID:      newUUID(),
		SpaceID:      auth.SpaceID,
		ThreadID:     threadID,
		Transcript:   steps,
		CreateThread: true,
		// 首轮发送完整 transcript，因此不是 partial。
		IsPartialTranscript:                    false,
		GenerateTitle:                          true,
		SaveAllThreadOperations:                true,
		SetUnreadState:                         true,
		AsPatchResponse:                        true,
		PatchResponseVersion:                   2,
		ThreadType:                             "workflow",
		SupportsCustomAgentNudgeTranscriptStep: true,
		IsUserInAnySalesAssistedSpace:          false,
		IsSpaceSalesAssisted:                   false,
		ThreadParentPointer: &parentPointer{
			Table:   "space",
			ID:      auth.SpaceID,
			SpaceID: auth.SpaceID,
		},
		DebugOverrides: newDebugOverrides(),
	}
}

// buildFollowUpRequest 组装续写请求。
//
// 续写只带 config + context + 新增 user 步，历史由上游线程保存；
// 因此 isPartialTranscript=true，且不能带 threadParentPointer
// （带上会被视为重复创建线程）。
// [参数] auth: 账号；model: 上游代号；threadID: 已有线程 id；prompt: 新一轮用户输入。
// [返回] 请求体。
// 最近修改时间：2026-10-18；改动原因：新增，续写请求组装。
func buildFollowUpRequest(auth *StoredAuth, model, threadID, prompt string) *inferenceRequest {
	return &inferenceRequest{
		TraceID:  newUUID(),
		SpaceID:  auth.SpaceID,
		ThreadID: threadID,
		Transcript: []transcriptStep{
			{"id": newUUID(), "type": "config", "value": buildConfigValue(model)},
			{"id": newUUID(), "type": "context", "value": buildContextValue(auth.UserID, auth.SpaceID, "full_page_chat")},
			buildUserStep(auth.UserID, prompt),
		},
		CreateThread:                           false,
		IsPartialTranscript:                    true,
		GenerateTitle:                          false,
		SaveAllThreadOperations:                true,
		SetUnreadState:                         true,
		AsPatchResponse:                        true,
		PatchResponseVersion:                   2,
		ThreadType:                             "workflow",
		SupportsCustomAgentNudgeTranscriptStep: true,
		IsUserInAnySalesAssistedSpace:          false,
		IsSpaceSalesAssisted:                   false,
		DebugOverrides:                         newDebugOverrides(),
	}
}

// newDebugOverrides 返回调试开关默认值。
// [参数] 无。
// [返回] 调试开关。
// 最近修改时间：2026-10-18；改动原因：新增，避免空 map 被序列化成 null。
func newDebugOverrides() debugOverrides {
	return debugOverrides{
		EmitAgentSearchExtractedResults: true,
		CachedInferences:                map[string]any{},
		AnnotationInferences:            map[string]any{},
		EmitInferences:                  false,
	}
}

// mergeMessages 把 OpenAI 消息归一为上游可用的对话序列。
//
// system 消息没有独立的 transcript 类型，按参考实现的做法并入
// 第一条 user 消息；连续多条 user 会各自成为独立步骤，上游按顺序读取。
// [参数] messages: OpenAI 消息列表。
// [返回] 归一后的消息与错误。
// 最近修改时间：2026-10-18；改动原因：新增，消息归一。
func mergeMessages(messages []chatMessage) ([]chatMessage, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("请求缺少 messages")
	}

	var systemParts []string
	out := make([]chatMessage, 0, len(messages))
	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system", "developer":
			systemParts = append(systemParts, content)
		case "user":
			out = append(out, chatMessage{Role: "user", Content: content})
		case "assistant":
			out = append(out, chatMessage{Role: "assistant", Content: content})
		}
	}

	if len(systemParts) > 0 {
		merged := strings.Join(systemParts, "\n")
		if len(out) == 0 {
			out = append(out, chatMessage{Role: "user", Content: merged})
		} else {
			out[0].Content = merged + "\n\n" + out[0].Content
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("请求缺少有效的消息内容")
	}
	return out, nil
}

// firstTurnMessages 返回首轮需要发送的完整对话。
//
// assistant 历史不会作为独立步骤发给上游：实测上游只接受 user 步骤，
// 把 assistant 内容拼进下一条 user 文本，既保留上下文又不引入未知步骤类型。
// [参数] messages: 归一后的消息。
// [返回] 首轮步骤内容。
// 最近修改时间：2026-10-18；改动原因：新增，历史对话折叠。
func firstTurnMessages(messages []chatMessage) []chatMessage {
	out := make([]chatMessage, 0, len(messages))
	var pendingAssistant []string

	for _, msg := range messages {
		if msg.Role == "assistant" {
			pendingAssistant = append(pendingAssistant, msg.Content)
			continue
		}
		content := msg.Content
		if len(pendingAssistant) > 0 {
			content = "（历史助手回复）\n" + strings.Join(pendingAssistant, "\n\n") + "\n\n（用户）\n" + content
			pendingAssistant = nil
		}
		out = append(out, chatMessage{Role: "user", Content: content})
	}

	// 末尾只剩 assistant 时补一条 user，否则上游没有待回答的问题。
	if len(out) == 0 {
		out = append(out, chatMessage{Role: "user", Content: strings.Join(pendingAssistant, "\n\n")})
	}
	return out
}

// lastUserMessage 返回最后一条 user 消息内容。
// [参数] messages: 归一后的消息。
// [返回] 最后一条 user 文本；没有时返回空串。
// 最近修改时间：2026-10-18；改动原因：新增，续写输入提取。
func lastUserMessage(messages []chatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// marshalInferenceRequest 序列化请求体。
// [参数] req: 请求体。
// [返回] JSON 字节与错误。
// 最近修改时间：2026-10-18；改动原因：新增，序列化出口。
func marshalInferenceRequest(req *inferenceRequest) ([]byte, error) {
	return json.Marshal(req)
}
