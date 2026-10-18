package main

// 本文件负责构造 Notion 请求与统一的 HTTP 调用。
//
// Notion 的鉴权全部走 Cookie：token_v2 是登录态，notion_user_id 与
// space id 还必须同时出现在请求头里，三者缺一会得到 403 或静默空流。
// 因此这里把「账号凭据 + 目标空间」固化成 requestScope，避免调用方
// 各处拼头时漏字段。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// maxNotionResponseBytes 是单次响应体上限，防止异常响应撑爆内存。
	maxNotionResponseBytes = 8 << 20
)

// requestScope 描述一次请求的归属：用哪个账号、打到哪个空间。
type requestScope struct {
	// Cookie 是完整 Cookie header，含 token_v2。
	Cookie string
	// UserID 对应 x-notion-active-user-header。
	UserID string
	// SpaceID 对应 x-notion-space-id，必须是内层 workspace id。
	SpaceID string
	// ClientVersion 对应 notion-client-version，空值时取默认值。
	ClientVersion string
}

// newNotionRequest 构造一个带完整浏览器特征的 POST 请求。
//
// 头集合与网页端一致，其中 Sec-Fetch-* 三项在实测中影响上游是否返回
// 真实流：缺失时偶发 200 空响应。
// [参数] ctx: 请求上下文；scope: 账号与空间；path: /api/v3 之后的路径；body: JSON 请求体。
// [返回] 构造好的请求与错误。
// 最近修改时间：2026-10-18；改动原因：新增，统一请求头构造。
func newNotionRequest(ctx context.Context, scope requestScope, path string, body []byte) (*http.Request, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost, notionAPIBase+path, bytes.NewReader(body))
	if errNew != nil {
		return nil, errNew
	}

	clientVersion := strings.TrimSpace(scope.ClientVersion)
	if clientVersion == "" {
		clientVersion = browserClientVersion
	}

	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/x-ndjson")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	h.Set("User-Agent", browserUserAgent())
	h.Set("sec-ch-ua", browserSecCHUA())
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", browserPlatform)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("Origin", notionOrigin)
	h.Set("Referer", notionReferer)
	h.Set("notion-client-version", clientVersion)
	h.Set("notion-audit-log-platform", notionAuditPlatform)
	h.Set("Cookie", scope.Cookie)
	if scope.UserID != "" {
		h.Set("x-notion-active-user-header", scope.UserID)
	}
	if scope.SpaceID != "" {
		h.Set("x-notion-space-id", scope.SpaceID)
	}
	return req, nil
}

// notionAPIError 表示上游返回了非 200 状态。
//
// 保留响应体前 512 字节：Notion 的错误体是结构化 JSON，
// 其中的 name 字段（UnauthorizedError / ValidationError）足以区分
// 「凭据失效」与「请求构造错误」，不需要落全量响应。
type notionAPIError struct {
	// StatusCode 是上游 HTTP 状态码。
	StatusCode int
	// Body 是响应体截断后的可读片段。
	Body string
}

// Error 实现 error 接口。
// [参数] 无。
// [返回] 错误描述。
// 最近修改时间：2026-10-18；改动原因：新增，标准错误形态。
func (e *notionAPIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("notion 上游返回 HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("notion 上游返回 HTTP %d: %s", e.StatusCode, e.Body)
}

// doNotionJSON 发起一次普通 JSON 请求并解析到 out。
// [参数] ctx: 上下文；cl: HTTP 客户端；scope: 账号与空间；path: API 路径；body: 请求体；out: 解析目标。
// [返回] 错误。
// 最近修改时间：2026-10-18；改动原因：新增，非流式端点统一出口。
func doNotionJSON(ctx context.Context, cl *http.Client, scope requestScope, path string, body []byte, out any) error {
	req, errReq := newNotionRequest(ctx, scope, path, body)
	if errReq != nil {
		return errReq
	}
	// 非流式端点不返回 NDJSON。
	req.Header.Set("Accept", "application/json")

	resp, errDo := cl.Do(req)
	if errDo != nil {
		return errDo
	}
	defer resp.Body.Close()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, maxNotionResponseBytes))
	if errRead != nil {
		return errRead
	}
	if resp.StatusCode != http.StatusOK {
		return &notionAPIError{StatusCode: resp.StatusCode, Body: truncateForError(raw)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// truncateForError 截取响应体用于错误消息。
// [参数] raw: 原始响应体。
// [返回] 最多 512 字节的可读字符串。
// 最近修改时间：2026-10-18；改动原因：新增，避免错误串过长。
func truncateForError(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 512 {
		return s[:512] + "..."
	}
	return s
}

// notionRequestTimeout 返回非流式 API 的默认超时。
// [参数] 无。
// [返回] 超时时长。
// 最近修改时间：2026-10-18；改动原因：新增，模型与额度端点共用。
func notionRequestTimeout() time.Duration {
	return 30 * time.Second
}
