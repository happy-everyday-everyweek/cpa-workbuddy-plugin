package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 本文件实现扣子 claw 的会话与消息 HTTP 协议。
//
// 链路（2026-10-18 真实账号实测）：
//   claw/info({})          → claw_id
//   session/get_main       → 固定主会话 session_id
//   message/chat           → 异步提交，只返回用户消息 message_id
//   message/list           → 已落库的完整消息（兜底，约 4 秒延迟）
//
// 为什么 chat 之后不能直接读响应：它是异步提交，响应里没有任何回复内容。
// 实时增量只从 frontier WebSocket 来（见 frontier.go），本文件的 list 只用于
// 断线兜底与最终校对。

const (
	// clawInfoPath 返回当前账号的 claw_id。
	//
	// body 必须是 `{}` 而不是空 body，否则参数校验直接失败。
	clawInfoPath = "/api/coze_claw/claw/info"

	// clawMainSessionPath 返回 claw 的固定主会话。
	//
	// 每个 claw 的主会话是稳定的，可长期复用，不必每次 session/create。
	clawMainSessionPath = "/api/coze_claw/session/get_main"

	// clawChatPath 提交一条用户消息（异步）。
	clawChatPath = "/api/coze_claw/message/chat"

	// clawMessageListPath 拉取已落库的消息。
	clawMessageListPath = "/api/coze_claw/message/list"

	// frontierKeyPath 取 frontier WebSocket 的连接凭证。
	frontierKeyPath = "/api/coze_claw/resource/get_frontier_key"

	// clawRequestTimeout 是 claw HTTP 接口的单次超时。
	//
	// chat 是异步提交，正常 1 秒内返回；15 秒覆盖上游排队。
	clawRequestTimeout = 15 * time.Second

	// clawMessageListSize 是兜底轮询单次拉取条数。
	//
	// 只需覆盖「本轮用户消息 + 本轮回复」，取 6 条足够；
	// 取太多会放大响应体积，对首字节时间没有收益。
	clawMessageListSize = 6

	// clawPollInterval 是兜底轮询的间隔。
	//
	// message/list 只返回已落库的完整消息，实测约 4 秒才出现；
	// 500ms 是「不漏首次出现」与「不打爆上游」之间的折中。
	clawPollInterval = 500 * time.Millisecond

	// clawMaxContentBytes 是 message/chat 的 content 硬上限。
	//
	// 上游把消息落库时按 65536 字节截断边界判定，越界直接返回
	// code=1000003「数据库错误」而不是参数错误。二分实测（2026-10-19）：
	// 65535 字节成功、65536 字节失败。
	clawMaxContentBytes = 65535
)

// clawEnvelope 是 claw 接口的通用响应外壳。
//
// 所有 claw 端点都是 HTTP 200 + code 表达业务结果，
// 只看状态码会把失败判成成功（实测：缺参数时 HTTP 200 + code 1000001）。
type clawEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// isSuccess 判断业务是否成功。
// [参数] e: 响应外壳。
// [返回] code 为 0 时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，业务成败统一判定。
func (e *clawEnvelope) isSuccess() bool {
	return e != nil && e.Code == 0
}

// describe 把失败响应转成可展示的错误说明。
// [参数] e: 响应外壳。
// [返回] 错误说明；成功时返回空串。
// 最近修改时间：2026-10-18；改动原因：新增，错误文案统一。
func (e *clawEnvelope) describe() string {
	if e == nil {
		return "上游无响应"
	}
	msg := strings.TrimSpace(e.Msg)
	if msg != "" {
		return fmt.Sprintf("%s（code=%d）", msg, e.Code)
	}
	return fmt.Sprintf("上游返回 code=%d", e.Code)
}

// clawInfoResponse 是 claw/info 的 data 段。
type clawInfoResponse struct {
	UserClawInfo struct {
		ClawID string `json:"claw_id"`
		Name   string `json:"name"`
		// ModelName 是内层 JSON 字符串，需二次解析；这里只保留原始值。
		ModelName string `json:"model_name"`
	} `json:"user_claw_info"`
}

// clawSessionResponse 是 session/get_main 的 data 段。
type clawSessionResponse struct {
	Session struct {
		ID     string `json:"id"`
		ClawID string `json:"claw_id"`
		Status int    `json:"status"`
	} `json:"session"`
}

// clawChatResponse 是 message/chat 的 data 段。
type clawChatResponse struct {
	MessageID string `json:"message_id"`
}

// clawFile 是消息附带的文件（图片、文档等）。
//
// 生图结果的图片就放在回复消息的 Files 里：Content 只有
// `[图片](computer://...)` 这种占位符，不能直接当 URL 用；
// 真正的可取图地址是 FileURL（带 sign 的签名直链，实测有效期 30 天）。
// 签名参数等价于临时凭据，日志与证据只记 FileName，不记完整 URL。
type clawFile struct {
	// FileName 是云盘内的绝对路径，如 /Coze/Drive/扣子/image/generate_xxx.jpg。
	FileName string `json:"file_name"`
	// FileURI 是云盘内部标识，不能直接访问。
	FileURI string `json:"file_uri"`
	// FileURL 是签名直链，可直接下载。
	FileURL string `json:"file_url"`
	// ThumbURL 是缩略图直链（webp），比原图小但经过格式转换。
	ThumbURL string `json:"thumb_url"`
	// IsReference 标记该文件是否为用户的参考图而非产出。
	IsReference int `json:"is_reference"`
}

// clawMessage 是 message/list 返回的一条消息。
//
// 注意 Content 是**内层 JSON 字符串**：AI 回复的正文在
// message_data.reply.content，用户消息的 Content 则是纯文本。
// Files 只在带附件时非空；纯文本消息该字段为 null，解析后是 nil。
type clawMessage struct {
	ID          string     `json:"id"`
	Source      int        `json:"source"`
	Content     string     `json:"content"`
	MessageType string     `json:"message_type"`
	CreateTime  string     `json:"create_time"`
	Files       []clawFile `json:"files"`
	Sender      struct {
		UserType int    `json:"user_type"`
		ClawID   string `json:"claw_id"`
		UserID   string `json:"user_id"`
	} `json:"sender"`
	ReferenceMessage json.RawMessage `json:"reference_message"`
}

// clawMessageListResponse 是 message/list 的 data 段。
type clawMessageListResponse struct {
	Messages   []clawMessage `json:"messages"`
	NextCursor string        `json:"next_cursor"`
}

// clawReplyPayload 是 AI 回复消息 content 内层 JSON 的结构。
type clawReplyPayload struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	MessageType string `json:"message_type"`
	MessageData struct {
		Reply struct {
			Content string `json:"content"`
			// ReplyToMessageID 是配对请求与回复的唯一可靠键。
			ReplyToMessageID string `json:"reply_to_message_id"`
			Stream           bool   `json:"stream"`
		} `json:"reply"`
	} `json:"message_data"`
	TurnID string `json:"turn_id"`
}

// frontierKeyResponse 是 get_frontier_key 的 data 段。
type frontierKeyResponse struct {
	AccessKey string `json:"access_key"`
	DeviceID  string `json:"device_id"`
}

// clawClient 是 claw HTTP 接口的调用入口。
//
// cookieHeader 是当前账号的会话 Cookie；client 允许测试注入假实现。
type clawClient struct {
	client       httpDoer
	cookieHeader string
	baseURL      string
}

// newClawClient 构造 claw 客户端。
// [参数] sa: 已落盘账号（提供 Cookie）；client: HTTP 客户端，nil 时用默认。
// [返回] 客户端。
// 最近修改时间：2026-10-18；改动原因：新增，客户端构造入口。
func newClawClient(sa *StoredAuth, client httpDoer) *clawClient {
	if client == nil {
		client = newUpstreamClient(clawRequestTimeout)
	}
	cookie := ""
	if sa != nil {
		cookie = sa.CookieHeader
	}
	return &clawClient{client: client, cookieHeader: cookie, baseURL: cozeSiteHost}
}

// postJSON 向 claw 端点发一次 POST 并解析外壳。
// [参数] path: 端点路径；payload: 请求体。
// [返回] 解析后的外壳与错误。
// 最近修改时间：2026-10-18；改动原因：新增，HTTP 调用唯一出口。
func (c *clawClient) postJSON(path string, payload any) (*clawEnvelope, error) {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("构造 %s 请求体失败：%w", path, errMarshal)
	}

	req, errReq := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("构造 %s 请求失败：%w", path, errReq)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Referer", cozeSiteHost+"/")
	if c.cookieHeader != "" {
		req.Header.Set("Cookie", c.cookieHeader)
	}

	resp, errDo := c.client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("%s 请求失败：%w", path, errDo)
	}
	defer resp.Body.Close()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("读取 %s 响应失败：%w", path, errRead)
	}

	var env clawEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("%s 响应解析失败（HTTP %d）：%w", path, resp.StatusCode, errUnmarshal)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &env, fmt.Errorf("%s 返回 HTTP %d：%s", path, resp.StatusCode, env.describe())
	}
	return &env, nil
}

// getJSON 向 claw 端点发一次 GET 并解析外壳。
// [参数] path: 端点路径。
// [返回] 解析后的外壳与错误。
// 最近修改时间：2026-10-18；改动原因：新增，get_frontier_key 调用入口。
func (c *clawClient) getJSON(path string) (*clawEnvelope, error) {
	req, errReq := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if errReq != nil {
		return nil, fmt.Errorf("构造 %s 请求失败：%w", path, errReq)
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Referer", cozeSiteHost+"/")
	if c.cookieHeader != "" {
		req.Header.Set("Cookie", c.cookieHeader)
	}

	resp, errDo := c.client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("%s 请求失败：%w", path, errDo)
	}
	defer resp.Body.Close()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("读取 %s 响应失败：%w", path, errRead)
	}

	var env clawEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("%s 响应解析失败（HTTP %d）：%w", path, resp.StatusCode, errUnmarshal)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &env, fmt.Errorf("%s 返回 HTTP %d：%s", path, resp.StatusCode, env.describe())
	}
	return &env, nil
}

// fetchClawID 取当前账号的 claw_id。
// [参数] 无。
// [返回] claw_id 与错误。
// 最近修改时间：2026-10-18；改动原因：新增，对话链路第一步。
func (c *clawClient) fetchClawID() (string, error) {
	env, err := c.postJSON(clawInfoPath, map[string]any{})
	if err != nil {
		return "", err
	}
	if !env.isSuccess() {
		return "", fmt.Errorf("取 claw_id 失败：%s", env.describe())
	}

	var parsed clawInfoResponse
	if errUnmarshal := json.Unmarshal(env.Data, &parsed); errUnmarshal != nil {
		return "", fmt.Errorf("claw/info 解析失败：%w", errUnmarshal)
	}
	clawID := strings.TrimSpace(parsed.UserClawInfo.ClawID)
	if clawID == "" {
		return "", fmt.Errorf("claw/info 未返回 claw_id")
	}
	return clawID, nil
}

// fetchMainSession 取 claw 的主会话 ID。
// [参数] clawID: claw 标识。
// [返回] 会话 ID 与错误。
// 最近修改时间：2026-10-18；改动原因：新增，对话链路第二步。
func (c *clawClient) fetchMainSession(clawID string) (string, error) {
	env, err := c.postJSON(clawMainSessionPath, map[string]any{"claw_id": clawID})
	if err != nil {
		return "", err
	}
	if !env.isSuccess() {
		return "", fmt.Errorf("取主会话失败：%s", env.describe())
	}

	var parsed clawSessionResponse
	if errUnmarshal := json.Unmarshal(env.Data, &parsed); errUnmarshal != nil {
		return "", fmt.Errorf("session/get_main 解析失败：%w", errUnmarshal)
	}
	sessionID := strings.TrimSpace(parsed.Session.ID)
	if sessionID == "" {
		return "", fmt.Errorf("session/get_main 未返回会话 ID")
	}
	return sessionID, nil
}

// verifyChatContent 校验待提交内容是否在上游体积上限内。
//
// 为什么必须在发请求前拦：上游对超限内容返回 code=1000003「数据库错误」，
// 宿主会把它当账号级失败把账号打入冷却，一次超长请求会让整个账号池
// 在冷却期内全部返回 auth_unavailable（2026-10-19 生产实证）。
// 提前拦成请求级错误，既不让上游白跑，也不污染账号可用性。
// [参数] content: 待提交的用户文本。
// [返回] 超限时返回带字节数的错误。
// 最近修改时间：2026-10-19；改动原因：新增，64KB 超限的前置拦截。
func verifyChatContent(content string) error {
	size := len(content)
	if size <= clawMaxContentBytes {
		return nil
	}
	return &chatContentTooLongError{size: size, limit: clawMaxContentBytes}
}

// chatContentTooLongError 表示请求正文超出上游体积上限。
//
// 实现 StatusCode 让宿主把它归入请求级故障（413 在
// IsRequestFault 的白名单内），从而跳过账号冷却与换号重试。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，请求级故障语义。
type chatContentTooLongError struct {
	size  int
	limit int
}

// Error 返回可直接展示给下游的错误说明。
// [参数] 无。
// [返回] 错误文案。
// 最近修改时间：2026-10-19；改动原因：新增，超限文案。
func (e *chatContentTooLongError) Error() string {
	return fmt.Sprintf("请求内容过长：%d 字节，扣子上限 %d 字节（约 64KB），请缩短对话历史后重试", e.size, e.limit)
}

// StatusCode 返回 HTTP 413，供宿主判定为请求级故障。
// [参数] 无。
// [返回] 413。
// 最近修改时间：2026-10-19；改动原因：新增，避免超限请求冷却账号。
func (e *chatContentTooLongError) StatusCode() int {
	return http.StatusRequestEntityTooLarge
}

// sendChat 提交一条用户消息，返回其 message_id。
//
// 该调用是异步的：返回时上游只完成了入队，回复内容随后经 WS 推送。
// 体积校验由调用方的同步入口完成（见 executor.go）：只有那里能把
// 413 经 RPC 信封回传宿主，让宿主判为请求级故障而不是账号故障。
// [参数] sessionID: 目标会话；content: 用户文本。
// [返回] 用户消息 ID 与错误。
// 最近修改时间：2026-10-19；改动原因：体积上限收敛到同步入口校验。
func (c *clawClient) sendChat(sessionID, content string) (string, error) {
	payload := map[string]any{
		"session_id": sessionID,
		"content":    content,
		"files":      []any{},
		"skill_ids":  []any{},
		"at_members": []any{},
	}
	env, err := c.postJSON(clawChatPath, payload)
	if err != nil {
		return "", err
	}
	if !env.isSuccess() {
		// 兜底：本地预检放行但上游仍报该码时，按上游服务端故障处理，
		// 保留原始码值。不能在这里猜成「内容过长」——体积正常时该码
		// 也可能出现，误标会让用户反复删内容却毫无改善。
		return "", fmt.Errorf("提交消息失败：%s", env.describe())
	}

	var parsed clawChatResponse
	if errUnmarshal := json.Unmarshal(env.Data, &parsed); errUnmarshal != nil {
		return "", fmt.Errorf("message/chat 解析失败：%w", errUnmarshal)
	}
	messageID := strings.TrimSpace(parsed.MessageID)
	if messageID == "" {
		return "", fmt.Errorf("message/chat 未返回 message_id")
	}
	return messageID, nil
}

// listMessages 拉取会话最近的消息（时间倒序）。
// [参数] sessionID: 目标会话。
// [返回] 消息列表与错误。
// 最近修改时间：2026-10-18；改动原因：新增，兜底轮询入口。
func (c *clawClient) listMessages(sessionID string) ([]clawMessage, error) {
	payload := map[string]any{
		"session_id": sessionID,
		"size":       clawMessageListSize,
		"asc_mode":   false,
	}
	env, err := c.postJSON(clawMessageListPath, payload)
	if err != nil {
		return nil, err
	}
	if !env.isSuccess() {
		return nil, fmt.Errorf("拉取消息失败：%s", env.describe())
	}

	var parsed clawMessageListResponse
	if errUnmarshal := json.Unmarshal(env.Data, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("message/list 解析失败：%w", errUnmarshal)
	}
	return parsed.Messages, nil
}

// fetchFrontierKey 取 frontier WebSocket 的连接凭证。
// [参数] 无。
// [返回] 凭证与错误。
// 最近修改时间：2026-10-18；改动原因：新增，WS 连接前置步骤。
func (c *clawClient) fetchFrontierKey() (*frontierKeyResponse, error) {
	env, err := c.getJSON(frontierKeyPath)
	if err != nil {
		return nil, err
	}
	if !env.isSuccess() {
		return nil, fmt.Errorf("取 frontier 凭证失败：%s", env.describe())
	}

	var parsed frontierKeyResponse
	if errUnmarshal := json.Unmarshal(env.Data, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("get_frontier_key 解析失败：%w", errUnmarshal)
	}
	if strings.TrimSpace(parsed.AccessKey) == "" || strings.TrimSpace(parsed.DeviceID) == "" {
		return nil, fmt.Errorf("get_frontier_key 返回的 access_key 或 device_id 为空")
	}
	return &parsed, nil
}

// findReplyFor 在消息列表里找出针对指定用户消息的 AI 回复。
//
// 配对键是 reply.reply_to_message_id == 用户消息的 message_id；
// 这是唯一可靠的配对方式——同一会话里可能有多轮回复并存。
// [参数] messages: 消息列表；userMessageID: 用户消息 ID。
// [返回] 命中的回复与是否找到。
// 最近修改时间：2026-10-18；改动原因：新增，兜底配对逻辑。
func findReplyFor(messages []clawMessage, userMessageID string) (*clawReplyPayload, bool) {
	for _, msg := range messages {
		if msg.MessageType != "reply" {
			continue
		}
		var payload clawReplyPayload
		if err := json.Unmarshal([]byte(msg.Content), &payload); err != nil {
			continue
		}
		if payload.MessageData.Reply.ReplyToMessageID == userMessageID {
			return &payload, true
		}
	}
	return nil, false
}

// findReplyMessageFor 与 findReplyFor 语义相同，但同时返回整条消息。
//
// 兜底轮询必须拿到 Files 才能补出图片——findReplyFor 只返回内层 payload，
// 附件在消息外层，用它的调用方会永远看不到图。
// [参数] messages: 消息列表；userMessageID: 用户消息 ID。
// [返回] 命中的整条消息与是否找到。
// 最近修改时间：2026-10-18；改动原因：新增，兜底路径需要附件。
func findReplyMessageFor(messages []clawMessage, userMessageID string) (*clawMessage, bool) {
	for index := range messages {
		msg := &messages[index]
		if msg.MessageType != "reply" {
			continue
		}
		var payload clawReplyPayload
		if err := json.Unmarshal([]byte(msg.Content), &payload); err != nil {
			continue
		}
		if payload.MessageData.Reply.ReplyToMessageID == userMessageID {
			return msg, true
		}
	}
	return nil, false
}

// clawFileURLs 提取附件里可下载的地址。
//
// 只要 FileURL：FileURI 是云盘内部标识不能直连，ThumbURL 是压缩过的
// webp 缩略图，原图才是用户要的产物。
// 接收切片而不是消息：WS 帧的顶层 files 与 message/list 的 files
// 同构，调用方不必为了一组文件构造一条假消息。
// [参数] files: 附件列表。
// [返回] 地址列表（顺序与上游一致，已去重）。
// 最近修改时间：2026-10-18；改动原因：新增，附件取址。
func clawFileURLs(files []clawFile) []string {
	if len(files) == 0 {
		return nil
	}
	var urls []string
	for _, file := range files {
		urls = appendUniqueAll(urls, strings.TrimSpace(file.FileURL))
	}
	return urls
}
