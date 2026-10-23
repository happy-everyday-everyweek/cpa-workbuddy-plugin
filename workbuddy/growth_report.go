// growth_report.go 是成长中心的「对话活跃上报」客户端：POST {billingBase}/v2/report。
//
// 这是旧版缺失、也是任务进度能否推进的关键能力。成长任务的进度不由客户端直改，
// 而是由服务端行为事件点亮：一条 chat_request_send 上报即可推进 chat_5 类任务的
// current 计数（2026-10-24 实测：上报后 3 秒内 0/5 → 1/5）。
//
// 事件形状对齐完整客户端报文（含 conversationId / mode / inputLength 等全字段），
// 不用最小 3 字段，防止上游后续加严。userId 必填（= 账号 uid）：
// 缺失时上游返回 200 但静默丢弃，本文件在发送前直接拦截。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// reportPath 是活跃上报通道（billingBase 根路径下，不在 growth 前缀里）。
const reportPath = "/v2/report"

// chatRequestEvent 是客户端 chat_request_send 事件的完整形状。
// 最近修改时间：2026-10-24 新增行为上报（TASK-02.2）
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// reportChatActivity 上报一条通用对话活跃事件。
// [参数] sa：账号凭据；conversationID：会话标识（可自造）；requestID：本轮请求标识（空时回落 conversationID）。
// [返回] 成功返回 nil；账号缺少 uid 或上游拒绝时返回错误。
// 最近修改时间：2026-10-24 新增行为上报（TASK-02.2）
func reportChatActivity(sa *storedAuth, conversationID, requestID string) error {
	return reportChatActivityModel(sa, conversationID, requestID, "deepseek-v4-flash", "DeepSeek V4 Flash")
}

// reportChatActivityModel 同上，但可指定上报携带的模型。
// 供「体验某模型」类任务对齐实际模型字段。
// [参数] sa：账号凭据；conversationID/requestID：会话与请求标识；
//
//	modelID/modelName：上报模型标识与名称（空时回落默认值）。
//
// [返回] 成功返回 nil；账号缺少 uid 或上游拒绝时返回错误。
// 最近修改时间：2026-10-24 新增行为上报（TASK-02.2）
func reportChatActivityModel(sa *storedAuth, conversationID, requestID, modelID, modelName string) error {
	// 1. 前置校验：userId 缺失会被上游静默丢弃，必须在发送前拦截
	uid := strings.TrimSpace(sa.Account.UID)
	if uid == "" {
		return fmt.Errorf("report: account uid is empty")
	}
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = modelID
	}

	// 2. 组装完整事件并包成数组（上游要求 JSON 数组）
	now := time.Now().UnixMilli()
	event := chatRequestEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		Mode:                 "craft",
		ConversationID:       conversationID,
		RequestID:            requestID,
		InputLength:          12,
		RequestModelID:       modelID,
		RequestModelName:     modelName,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		PresentAt:            now,
		RootRequestID:        requestID,
		ParentConversationID: conversationID,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               uid,
	}

	// 3. 经 billing 鉴权头发到 billingBase 根路径；写操作单发不重试
	raw, err := json.Marshal([]chatRequestEvent{event})
	if err != nil {
		return err
	}
	return billingPostRaw(sa, reportPath, raw)
}

// billingPostRaw 向 billingBase 发送一次原始 JSON 请求并解 envelope。
// 与 billingCallOnce 同族，但直接接收已序列化的请求体，供上报数组使用。
// [参数] sa：账号凭据；path：billingBase 下的路径；body：已序列化的 JSON。
// [返回] 成功返回 nil；5xx / 解析失败 / 业务 code != 0 时返回错误。
// 最近修改时间：2026-10-24 新增行为上报（TASK-02.2）
func billingPostRaw(sa *storedAuth, path string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, billingBaseFor(sa)+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return err
	}
	raw := resp.Body
	// 1. 5xx 与无正文的 2xx 均按成功语义处理：上报本身幂等且不返回数据
	if resp.StatusCode >= 500 {
		return fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, growthSnippet(raw))
	}
	if len(raw) == 0 {
		return nil
	}
	// 2. 解 {code,msg} envelope；data 恒为 null，不参与判定
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse failed: %w (body: %s)", err, growthSnippet(raw))
	}
	if env.Code != 0 {
		return fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return nil
}
