package main

// 本文件实现宿主的 OAuth 登录入口。
//
// Notion 没有开放给第三方的一键授权：AI 登录态是浏览器里的 HttpOnly Cookie
// （token_v2），只能由用户从网页端导出。因此这里不实现真实的 OAuth 跳转，
// 而是把宿主「+ 添加账号」打开的弹窗变成桥接页：引导用户导出 Cookie →
// 粘回桥接页 → 插件校验、拆分工作空间并落盘 → 宿主轮询领取。
//
// 为什么必须实现这组方法：插件声明了 auth_provider，宿主会把「+」指向插件的
// auth.login.start。未实现时宿主统一报 failed to generate authorization url。

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginStateTTL 是一次登录桥接会话的有效期。
//
// 比宿主侧 OAuth 会话更短：桥接页只承载一次粘贴动作，忘了关的标签页
// 不该在半小时后还能把凭据塞进来。
const loginStateTTL = 15 * time.Minute

// loginBridgeImportHeader 是桥接页提交 Cookie 时使用的请求头。
//
// resource 路由由宿主强制改写为 GET 且不转发请求体，Cookie 只能走请求头；
// 用自定义头而不是 Cookie 头，避免与浏览器自身的 Cookie 语义混淆。
const loginBridgeImportHeader = "X-Notion-Cookie"

// loginBridgeInputHeader 承载用户粘贴的整段请求原文（base64）。
//
// 不能直接放进请求头：Copy as cURL 是多行文本，而浏览器构造 Headers 时会
// 直接拒绝含换行的值（实测 `Failed to construct 'Headers': Invalid value`）。
// base64 还顺带解决了中文与引号在头部传输中的编码问题。
const loginBridgeInputHeader = "X-Notion-Auth-Input"

// loginInputMaxBytes 是整段原文的上限。
//
// 只需要其中的 Cookie 行，正常 cURL 在数 KB 以内；设上限是为了避免
// 用户误粘大段日志时把请求头撑爆。
const loginInputMaxBytes = 64 << 10

// callbackPath 是脚本采集完成后跳回的 resource 路径。
const callbackPath = "/login/callback"

// 回调页从 URL fragment 读取的键。
//
// 走 fragment 而不是 query：fragment 不会发给服务端，也不进访问日志；
// 脚本采集到的可读 Cookie 只能先落在这里，再由页面经同源请求头回传。
const (
	callbackCookieParam = "cookie"
	callbackErrorParam  = "err"
)

// cpaOriginPlaceholder 是脚本里等待替换成插件所在域 origin 的占位符。
//
// 不能直接在服务端拼绝对地址：插件运行在容器内，看不到用户实际访问的域名
// （反向代理 / 端口映射都会改写）。桥接页在浏览器里加载，location.origin
// 就是用户真实可用的地址，替换后才不会把 Cookie 发到 notion.com。
const cpaOriginPlaceholder = "__CPA_ORIGIN__"

// loginSession 是一次桥接会话。
type loginSession struct {
	// startedAt 是会话创建时间，用于判定会话是否过期。
	startedAt time.Time
	// importedAuthIDs 是桥接页显式导入成功后登记的账号标识。
	//
	// 这是唯一的成功判据：不能靠「账号集合变化」或 UpdatedAt 变新推断——
	// 插件在正常请求后会把滚动 Cookie 写回账号文件，那也会刷新 UpdatedAt，
	// 会让「用户还没粘贴」的会话被误判成登录完成。
	importedAuthIDs []string
	// lastError 记录最近一次失败的说明，供面板与轮询反馈。
	lastError string
}

var (
	loginSessionsMu sync.Mutex
	loginSessions   = make(map[string]*loginSession)
)

// loginBridgeURL 构造桥接页的相对 URL。
//
// 必须是相对路径：宿主 UI 从管理域 origin 用 window.open 打开它，相对路径
// 在本地与远程部署下都能解析到正确的服务地址。
// [参数] state: 登录会话标识。
// [返回] 桥接页相对 URL。
// 最近修改时间：2026-10-19；改动原因：新增，修复 OAuth 登录无法进入授权流程。
func loginBridgeURL(state string) string {
	return "/v0/resource/plugins/" + providerName + "/login?state=" + state
}

// newLoginState 生成一个随机登录会话标识。
//
// 用 crypto/rand 而不是时间戳：state 是桥接页提交凭据的唯一凭证，
// 可预测的 state 等于把「任意人可塞凭据」的入口暴露给同域页面。
// [参数] 无。
// [返回] 32 位十六进制字符串。
// 最近修改时间：2026-10-19；改动原因：新增，登录会话标识生成。
func newLoginState() (string, error) {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", errRead
	}
	return hex.EncodeToString(buf), nil
}

// handleStartLogin 响应宿主 auth.login.start。
//
// 不做任何上游请求：只登记会话并返回桥接页地址。宿主弹窗打开的就是本页，
// 用户在页内完成 Cookie 粘贴后，宿主通过 auth.login.poll 拿到结果。
// [参数] raw: 宿主登录起点请求载荷。
// [返回] 登录起点响应信封与错误。
// 最近修改时间：2026-10-19；改动原因：新增，修复「failed to generate authorization url」。
func handleStartLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}

	state, errState := newLoginState()
	if errState != nil {
		return nil, fmt.Errorf("生成登录会话标识失败：%w", errState)
	}

	loginSessionsMu.Lock()
	pruneExpiredLoginSessionsLocked()
	loginSessions[state] = &loginSession{startedAt: time.Now()}
	loginSessionsMu.Unlock()

	bridgeURL := loginBridgeURL(state)
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       bridgeURL,
		State:     state,
		ExpiresAt: time.Now().Add(loginStateTTL).UTC(),
		Metadata: map[string]any{
			"logo":       pluginLogoURL,
			"bridge_url": bridgeURL,
			"prompt":     "在打开的引导页执行脚本并粘贴一次 Copy as cURL，插件自行识别凭据，导入成功后此窗口会自动完成。",
		},
	})
}

// handlePollLogin 响应宿主 auth.login.poll。
//
// 成功判据是桥接页显式登记了导入结果（见 markLoginImported）。不能用账号
// 集合变化或 UpdatedAt 推断：Cookie 滚动写回同样会刷新 UpdatedAt，
// 会让「用户还没粘贴」的会话被误判成登录完成。
// [参数] raw: 宿主登录轮询请求载荷。
// [返回] 轮询结果信封与错误。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页结果回传。
func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("解析轮询请求失败：%w", errUnmarshal)
		}
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("缺少登录会话标识")
	}

	loginSessionsMu.Lock()
	session, ok := loginSessions[state]
	if ok && time.Since(session.startedAt) > loginStateTTL {
		delete(loginSessions, state)
		ok = false
	}
	var importedAuthIDs []string
	var lastError string
	if ok {
		importedAuthIDs = append(importedAuthIDs, session.importedAuthIDs...)
		lastError = session.lastError
	}
	loginSessionsMu.Unlock()

	if !ok {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请回到管理页重新发起",
		})
	}
	if len(importedAuthIDs) == 0 {
		msg := "等待在引导页执行授权脚本"
		if lastError != "" {
			msg = "上次尝试失败：" + lastError
		}
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: msg,
		})
	}

	accounts, errList := listAllAuthFiles()
	if errList != nil {
		return nil, fmt.Errorf("读取账号失败：%w", errList)
	}

	// 一次导入会按工作空间拆成多个账号，必须全部回传：宿主只领第一份的话，
	// 另一个空间会变成孤儿文件（插件侧存在、宿主侧不可路由）。
	auths := make([]pluginapi.AuthData, 0, len(importedAuthIDs))
	for _, authID := range importedAuthIDs {
		sa := findAccountByID(accounts, authID)
		if sa == nil {
			// 导入成功后被删除：跳过该项而不是整体失败，其余空间仍可交付。
			continue
		}
		auths = append(auths, authDataForLogin(sa))
	}
	if len(auths) == 0 {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "账号已被删除，请重新导入",
		})
	}

	// 结果只消费一次：成功后立即删除会话，避免同一 state 被重复领取。
	loginSessionsMu.Lock()
	delete(loginSessions, state)
	loginSessionsMu.Unlock()

	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: fmt.Sprintf("Notion 登录态导入成功，共 %d 个工作空间", len(auths)),
		Auth:    auths[0],
		Auths:   auths,
	})
}

// markLoginImported 记录桥接页已成功导入的账号，供宿主轮询领取。
// [参数] state: 登录会话标识；authIDs: 导入成功的账号标识列表。
// [返回] 会话不存在或已过期时返回 false。
// 最近修改时间：2026-10-19；改动原因：新增，显式成功信号（替代账号集合推断）。
func markLoginImported(state string, authIDs []string) bool {
	state = strings.TrimSpace(state)
	if state == "" || len(authIDs) == 0 {
		return false
	}
	loginSessionsMu.Lock()
	defer loginSessionsMu.Unlock()
	session, ok := loginSessions[state]
	if !ok || time.Since(session.startedAt) > loginStateTTL {
		delete(loginSessions, state)
		return false
	}
	session.importedAuthIDs = append([]string(nil), authIDs...)
	return true
}

// findAccountByID 按账号标识查找账号。
// [参数] accounts: 账号列表；authID: 账号标识。
// [返回] 命中的账号；没有时返回 nil。
// 最近修改时间：2026-10-19；改动原因：新增，轮询结果定位。
func findAccountByID(accounts []*StoredAuth, authID string) *StoredAuth {
	for _, sa := range accounts {
		if sa != nil && sa.AuthID == authID {
			return sa
		}
	}
	return nil
}

// pruneExpiredLoginSessionsLocked 清理过期会话，避免 map 无限增长。
// 调用方必须已持有 loginSessionsMu。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，会话回收。
func pruneExpiredLoginSessionsLocked() {
	for state, session := range loginSessions {
		if session == nil || time.Since(session.startedAt) > loginStateTTL {
			delete(loginSessions, state)
		}
	}
}

// handleLoginBridge 渲染桥接页。
//
// 页面只做两件事：说明为什么没有一键授权，以及把用户粘贴的 Cookie 交给插件。
// 提交走 resource 路由（/login/import）而不是 management 路由：resource 路由
// 由宿主以 GET 免密钥放行，桥接页是浏览器顶层打开的普通页面，拿不到、也不该
// 要求用户填管理密钥。
// [参数] state: 登录会话标识。
// [返回] 管理响应（HTML）。
// 最近修改时间：2026-10-19；改动原因：新增，登录引导页。
func handleLoginBridge(state string) pluginapi.ManagementResponse {
	state = strings.TrimSpace(state)
	if state == "" {
		return loginBridgePage("", "缺少 state 参数：请从管理页的「添加账号」重新发起。")
	}

	loginSessionsMu.Lock()
	session, ok := loginSessions[state]
	if ok && time.Since(session.startedAt) > loginStateTTL {
		delete(loginSessions, state)
		ok = false
	}
	loginSessionsMu.Unlock()
	if !ok || session == nil {
		return loginBridgePage("", "登录会话不存在或已过期（15 分钟有效）。请回到管理页重新点击「添加账号」。")
	}

	return loginBridgePage(state, "")
}

// handleLoginBridgeImport 处理桥接页的登录态提交（resource 路由，免管理密钥）。
//
// 只有持有有效 state 的桥接页才能调用：state 在 handleStartLogin 时随机生成、
// 15 分钟过期，等于一次性的短时凭证。
// [参数] req: 宿主透传的请求（Cookie 在 X-Notion-Cookie 头，state 在 query）。
// [返回] 管理响应（JSON）。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页免密钥提交入口。
func handleLoginBridgeImport(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	// 宿主有两种下发形态：Query 单独给出，或只把 query 挂在 Path 上。
	// 只读其中一个会在另一种形态下判成「缺少登录会话标识」。
	state := loginStateFromRequest(req)
	if state == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "缺少登录会话标识，请回到管理页重新发起"})
	}

	loginSessionsMu.Lock()
	session, ok := loginSessions[state]
	if ok && time.Since(session.startedAt) > loginStateTTL {
		delete(loginSessions, state)
		ok = false
	}
	loginSessionsMu.Unlock()
	if !ok || session == nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "登录会话不存在或已过期，请回到管理页重新发起"})
	}

	cookie, errInput := loginInputFromRequest(req)
	if errInput != nil {
		markLoginFailed(state, errInput.Error())
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errInput.Error()})
	}
	if cookie == "" {
		markLoginFailed(state, "未收到登录凭据")
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "没有收到登录凭据，请粘贴 Copy as cURL 的内容"})
	}

	accounts, errSave := saveCredentials(cookie, "", "")
	if errSave != nil {
		// 失败原因同时记进会话：宿主轮询能显示具体原因，而不是永远「等待中」。
		markLoginFailed(state, errSave.Error())
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": errSave.Error()})
	}
	authIDs := make([]string, 0, len(accounts))
	labels := make([]string, 0, len(accounts))
	for _, sa := range accounts {
		authIDs = append(authIDs, sa.AuthID)
		labels = append(labels, sa.Label)
	}

	if !markLoginImported(state, authIDs) {
		// 账号已落盘但会话刚好过期：不丢账号，只提示宿主重来。
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"ok":       true,
			"auth_ids": authIDs,
			"labels":   labels,
			"message":  "登录态导入成功，但登录会话已过期；账号已保存，可直接在面板使用",
		})
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":       true,
		"auth_ids": authIDs,
		"labels":   labels,
		"message":  fmt.Sprintf("登录态导入成功，共 %d 个工作空间", len(authIDs)),
	})
}

// loginInputFromRequest 取出用户提交的整段请求原文。
//
// 优先读 base64 头：Copy as cURL 是多行文本，浏览器不允许把它直接放进
// 请求头（实测 Headers 构造即报 Invalid value）。旧的纯 Cookie 头保留为
// 兜底，供面板等已按单行形态提交的调用方继续使用。
// [参数] req: 宿主透传的管理请求。
// [返回] 用户原文与错误。
// 最近修改时间：2026-10-19；改动原因：新增，整段原文 base64 传输。
func loginInputFromRequest(req pluginapi.ManagementRequest) (string, error) {
	encoded := strings.TrimSpace(req.Headers.Get(loginBridgeInputHeader))
	if encoded != "" {
		decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
		if errDecode != nil {
			return "", fmt.Errorf("粘贴内容传输格式错误，请重新复制后提交")
		}
		if len(decoded) > loginInputMaxBytes {
			return "", fmt.Errorf("粘贴内容过长（超过 %d 字节），请只复制一条 api/v3 请求", loginInputMaxBytes)
		}
		return strings.TrimSpace(string(decoded)), nil
	}
	return strings.TrimSpace(req.Headers.Get(loginBridgeImportHeader)), nil
}

// loginStateFromRequest 从管理请求中取出桥接会话标识。
//
// 宿主可能把 query 单独放在 req.Query，也可能只挂在 req.Path 上；
// 两处都要看，否则桥接页提交会稳定被判成「缺少会话标识」。
// [参数] req: 宿主透传的管理请求。
// [返回] 会话标识；两处都没有时返回空串。
// 最近修改时间：2026-10-19；改动原因：新增，兼容宿主两种 query 下发形态。
func loginStateFromRequest(req pluginapi.ManagementRequest) string {
	if vals := req.Query["state"]; len(vals) > 0 {
		if state := strings.TrimSpace(vals[0]); state != "" {
			return state
		}
	}
	if parsed, errParse := url.Parse(req.Path); errParse == nil {
		return strings.TrimSpace(parsed.Query().Get("state"))
	}
	return ""
}

// loginBridgePage 渲染桥接页 HTML。detail 非空时为错误页。
//
// 逐段 WriteString 拼接而非原生字符串：页面里需要写 JS，反引号会截断
// Go 的原始字符串字面量。
// [参数] state: 登录会话标识；detail: 错误说明。
// [返回] 管理响应（HTML）。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页渲染。
func loginBridgePage(state, detail string) pluginapi.ManagementResponse {
	var b strings.Builder
	writeBridgeHead(&b, state)
	if detail != "" {
		b.WriteString("<h1>无法继续</h1><div class=\"err\">" + htmlEscape(detail) + "</div>")
		b.WriteString("</div></body></html>")
		return htmlManagementResponse(b.String())
	}
	writeBridgeBody(&b, state)
	writeBridgeScript(&b)
	b.WriteString("</div></body></html>")
	return htmlManagementResponse(b.String())
}

// writeBridgeHead 写出页面头部与样式表。
// [参数] b: 输出缓冲；state: 登录会话标识（写入 body 的 data 属性）。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，从桥接页渲染中拆出。
func writeBridgeHead(b *strings.Builder, state string) {
	b.WriteString("<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	b.WriteString("<title>Notion 账号登录</title>")
	b.WriteString("<style>")
	b.WriteString("body{font-family:-apple-system,'Segoe UI','Microsoft YaHei',sans-serif;background:#faf9f5;color:#2d2a26;margin:0;padding:32px 16px;display:flex;justify-content:center}")
	b.WriteString(".card{background:#fff;border:1px solid #e3e1db;border-radius:14px;max-width:640px;width:100%;padding:26px 28px}")
	b.WriteString("h1{font-size:19px;margin:0 0 10px}p{line-height:1.7;margin:8px 0;font-size:14px;color:#6d6760}")
	b.WriteString("ol{padding-left:20px;color:#6d6760}ol li{margin:8px 0;font-size:14px;line-height:1.7}")
	b.WriteString("code{background:#f0eee8;border-radius:4px;padding:2px 6px;font-size:13px}")
	b.WriteString("textarea{width:100%;box-sizing:border-box;min-height:110px;border:1px solid #e3e1db;border-radius:8px;padding:9px 11px;font:12px/1.5 ui-monospace,Consolas,monospace;outline:none;resize:vertical}")
	b.WriteString("textarea:focus{border-color:#4c8dff;box-shadow:0 0 0 3px rgba(76,141,255,.18)}")
	b.WriteString("button{margin-top:12px;background:#4c8dff;color:#fff;border:0;border-radius:8px;padding:9px 18px;font-size:14px;cursor:pointer}")
	b.WriteString("button:disabled{opacity:.55;cursor:default}")
	b.WriteString(".err{background:#fdf1ef;border:1px solid #e8b9b1;border-radius:8px;padding:10px 14px;margin-top:14px;font-size:14px;color:#a8452f}")
	b.WriteString(".ok{background:#eaf8f2;border:1px solid #a8dcc7;border-radius:8px;padding:10px 14px;margin-top:14px;font-size:14px;color:#0d7a55}")
	b.WriteString(".note{background:#fdf7e6;border:1px solid #edd9a3;border-radius:8px;padding:10px 14px;margin-top:14px;font-size:13px;color:#8a6a10}")
	b.WriteString("</style></head><body data-provider=\"" + htmlEscape(providerName) + "\" data-state=\"" + htmlEscape(state) + "\"><div class=\"card\">")
}

// writeBridgeBody 写出引导文案、可复制链接与一次性授权脚本。
//
// 与 coze 桥接页同一形态：页面给出「可复制的链接」和「可在 Notion 页面
// Console 直接粘贴执行的脚本」，脚本采集完成后自动跳回插件回调页。
//
// 与 coze 的差异（Notion 特有约束）：token_v2 是 HttpOnly，页面 JS 读不到。
// 因此不能只靠脚本采集——脚本负责校验登录态并带回可读 Cookie，回调页再让
// 用户一次粘贴「Copy as cURL」整段请求，由插件自己把 token_v2 抽出来。
// 用户不需要去 Application 面板逐个找字段。
// [参数] b: 输出缓冲；state: 登录会话标识。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：改为可复制链接 + 控制台脚本形态。
func writeBridgeBody(b *strings.Builder, state string) {
	notionURL := "https://www.notion.so/ai"
	script := buildAuthScript(state)
	b.WriteString("<h1>授权 Notion 账号</h1>")
	b.WriteString("<p>Notion 没有开放给第三方的一键授权：AI 登录态是浏览器里的 HttpOnly Cookie（<code>token_v2</code>），只发给 <code>notion.com</code>，插件无法作为 OAuth 回调接收方。请按下面三步操作，全程约 30 秒。</p>")
	b.WriteString("<ol>")
	b.WriteString("<li>点击下方按钮打开 <code>" + notionURL + "</code>，确认已登录目标账号。</li>")
	b.WriteString("<li>在 Notion 页面按 <code>F12</code> 打开开发者工具 → 切到 <code>Console</code> 标签。</li>")
	b.WriteString("<li>复制下方脚本粘贴到 Console 回车执行；页面会跳回本插件并自动完成授权。</li>")
	b.WriteString("</ol>")
	b.WriteString("<button id=\"openNotion\" type=\"button\">打开 Notion</button>")
	b.WriteString("<p style=\"margin-top:18px\">Notion 页面地址（可复制）：</p>")
	b.WriteString("<pre id=\"notionURL\">" + htmlEscape(notionURL) + "</pre>")
	b.WriteString("<button id=\"copyURL\" type=\"button\">复制链接</button>")
	b.WriteString("<p style=\"margin-top:18px\">一次性授权脚本（15 分钟内有效）：</p>")
	b.WriteString("<pre id=\"authScript\">" + htmlEscape(script) + "</pre>")
	b.WriteString("<button id=\"copyScript\" type=\"button\">复制脚本</button>")
	b.WriteString("<div id=\"copyHint\"></div>")
	b.WriteString("<div class=\"note\">脚本只做一件事：在 Notion 页面内校验登录态、采集浏览器可读的 Cookie，然后交给本插件。它不读取、也不上传你的 <code>token_v2</code>（该值被浏览器设为 HttpOnly，任何页面脚本都读不到）。</div>")
	b.WriteString("<div class=\"note\">回调页会用「Copy as cURL」补齐最后一步：在 Notion 页面按 <code>F12 → Network</code>，任选一个 <code>api/v3</code> 请求右键 → <code>Copy</code> → <code>Copy as cURL</code>，把整段粘进回调页即可。插件会自己解析出 <code>token_v2</code>，不需要你逐项找。</div>")
}

// writeBridgeScript 写出桥接页自身的交互脚本（复制按钮与打开 Notion）。
//
// 脚本保持纯静态：provider 与 state 从 body 的 data 属性读取，避免把 Go
// 变量拼进 JS 字面量。
// [参数] b: 输出缓冲。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：改为复制链接 / 复制脚本交互。
func writeBridgeScript(b *strings.Builder) {
	b.WriteString("<script>")
	// 脚本在 Notion 页面执行，那里的 location.origin 是 notion.com；回调地址
	// 必须指向本插件所在域，因此占位符在桥接页（即 CPA 域）加载时替换。
	b.WriteString("var scriptEl=document.getElementById(\"authScript\");")
	b.WriteString("if(scriptEl){scriptEl.textContent=scriptEl.textContent.split(\"" + cpaOriginPlaceholder + "\").join(location.origin)}")
	b.WriteString("function bindCopy(btnId,preId,okText){")
	b.WriteString("  var btn=document.getElementById(btnId);if(!btn){return}")
	b.WriteString("  btn.addEventListener(\"click\",function(){")
	b.WriteString("    var text=(document.getElementById(preId).textContent||\"\").trim();")
	b.WriteString("    var hint=document.getElementById(\"copyHint\");")
	b.WriteString("    var done=function(){hint.className=\"ok\";hint.textContent=okText};")
	b.WriteString("    var fail=function(){hint.className=\"err\";hint.textContent=\"自动复制失败，请手动选中文本复制。\"};")
	b.WriteString("    if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(text).then(done).catch(fail)}else{fail()}")
	b.WriteString("  })")
	b.WriteString("}")
	b.WriteString("bindCopy(\"copyScript\",\"authScript\",\"已复制，请到 Notion 页面的 Console 粘贴执行。\");")
	b.WriteString("bindCopy(\"copyURL\",\"notionURL\",\"已复制，请在已登录的浏览器标签页打开。\");")
	b.WriteString("var openBtn=document.getElementById(\"openNotion\");")
	b.WriteString("if(openBtn){openBtn.addEventListener(\"click\",function(){window.open(\"https://www.notion.so/ai\",\"_blank\",\"noopener\")})}")
	b.WriteString("</script>")
}

// buildAuthScript 生成一次性授权脚本。
//
// 脚本在 Notion 页面上下文执行，因此：同源 fetch 天然带上登录 Cookie，
// 用它校验登录态有效；同时把浏览器可读的 Cookie 拼成 header 交给回调页。
// token_v2 读不到（HttpOnly），回调页会提示补填。
// [参数] state: 登录会话标识。
// [返回] 可粘贴执行的 JS 源码（含待替换的 origin 占位符）。
// 最近修改时间：2026-10-19；改动原因：新增，从粘贴 Cookie 改为脚本采集。
func buildAuthScript(state string) string {
	callback := "/v0/resource/plugins/" + providerName + callbackPath
	var b strings.Builder
	b.WriteString("(async()=>{\n")
	b.WriteString("  const cb='")
	b.WriteString(cpaOriginPlaceholder)
	b.WriteString(callback)
	b.WriteString("?state=")
	b.WriteString(jsStringEscape(state))
	b.WriteString("';\n")
	b.WriteString("  const fail=(m)=>{location.href=cb+'#" + callbackErrorParam + "='+encodeURIComponent(m)};\n")
	// 先确认登录态：未登录时上游返回 401，脚本直接报错而不是带着空 Cookie 跳回去。
	b.WriteString("  try{\n")
	b.WriteString("    const r=await fetch('/api/v3/getSpaces',{method:'POST',credentials:'include',headers:{'Content-Type':'application/json'},body:'{}'});\n")
	b.WriteString("    if(!r.ok){fail('当前页面未登录 Notion（HTTP '+r.status+'），请先登录再执行脚本');return;}\n")
	b.WriteString("  }catch(e){fail('校验登录态失败：'+(e&&e.message?e.message:String(e)));return;}\n")
	// 只采集可读 Cookie；token_v2 由回调页补填。
	b.WriteString("  const kv=[];\n")
	b.WriteString("  document.cookie.split(';').forEach(p=>{const s=p.trim();if(s.indexOf('=')>0){kv.push(s)}});\n")
	b.WriteString("  if(!kv.some(s=>s.indexOf('notion_user_id=')===0)){fail('未取到 notion_user_id，请确认已在 Notion 登录');return;}\n")
	b.WriteString("  location.href=cb+'#" + callbackCookieParam + "='+encodeURIComponent(kv.join('; '));\n")
	b.WriteString("})()")
	return b.String()
}

// jsStringEscape 转义要嵌入单引号 JS 字符串的文本。
// [参数] s: 原始文本。
// [返回] 转义后的文本。
// 最近修改时间：2026-10-19；改动原因：新增，避免 state 破坏脚本字面量。
func jsStringEscape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "'", "\\'", "\n", "\\n", "\r", "\\r", "<", "\\x3c")
	return r.Replace(s)
}

// handleLoginCallback 渲染脚本跳回的接收页。
//
// 脚本把采集到的可读 Cookie 放在 URL fragment 里（#cookie=...），服务端
// 看不到 fragment，因此首次请求只返回接收页；页面脚本把 fragment 解析出来、
// 提示用户补填 token_v2，再经同源请求头提交到 /login/import。
//
// 为什么不直接由脚本提交：token_v2 是 HttpOnly，脚本读不到；必须给用户一个
// 可见的补填入口，否则导入必然因「缺少 token_v2」失败。
// [参数] state: 登录会话标识。
// [返回] 管理响应（HTML）。
// 最近修改时间：2026-10-19；改动原因：新增，脚本回调落点。
func handleLoginCallback(state string) pluginapi.ManagementResponse {
	state = strings.TrimSpace(state)
	if state == "" {
		return loginCallbackShell("", "缺少 state 参数，请回到管理页重新发起授权。")
	}
	if !validLoginState(state) {
		return loginCallbackShell("", "登录会话不存在或已过期（15 分钟有效）。请回到管理页重新发起。")
	}
	return loginCallbackShell(state, "")
}

// loginCallbackShell 渲染脚本回调页。
//
// detail 非空时只展示错误；否则读取 fragment 中的 Cookie 并给出补填表单。
// [参数] state: 登录会话标识；detail: 错误说明。
// [返回] 管理响应（HTML）。
// 最近修改时间：2026-10-19；改动原因：新增，fragment 接收与补填页。
func loginCallbackShell(state, detail string) pluginapi.ManagementResponse {
	var b strings.Builder
	writeBridgeHead(&b, state)
	if detail != "" {
		b.WriteString("<h1>无法继续</h1><div class=\"err\">" + htmlEscape(detail) + "</div>")
		b.WriteString("<p class=\"note\">请回到管理页重新发起授权。</p></div></body></html>")
		return htmlManagementResponse(b.String())
	}
	writeCallbackBody(&b)
	writeCallbackScript(&b)
	b.WriteString("</div></body></html>")
	return htmlManagementResponse(b.String())
}

// writeCallbackBody 写出凭据补全表单。
//
// 页面不再要求用户逐个字段抄写：主入口是「Copy as cURL」整段粘贴，
// 插件在服务端自己解析 Cookie（含 HttpOnly 的 token_v2）。脚本已采集的
// 可读 Cookie 单独保留为只读展示，仅用于出错时对照。
// [参数] b: 输出缓冲。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：改为整段粘贴 + 插件自行识别。
func writeCallbackBody(b *strings.Builder) {
	b.WriteString("<h1>完成 Notion 授权</h1>")
	b.WriteString("<p>脚本已采集到可读 Cookie。最后一步只需要把 Notion 的请求整段交给插件，<code>token_v2</code> 由插件自己解析——不用去 Application 面板逐项找。</p>")
	b.WriteString("<ol>")
	b.WriteString("<li>回到已登录的 Notion 页面，按 <code>F12</code> → <code>Network</code>（网络）标签。</li>")
	b.WriteString("<li>点任意一个 <code>api/v3</code> 请求（例如 <code>getSpaces</code>），右键 → <code>Copy</code> → <code>Copy as cURL</code>。</li>")
	b.WriteString("<li>把复制到的整段内容粘贴到下面的框里，点「完成授权」。</li>")
	b.WriteString("</ol>")
	b.WriteString("<p style=\"margin-top:18px\">粘贴 Copy as cURL / 请求头（插件自动识别 Cookie）：</p>")
	b.WriteString("<textarea id=\"curl\" style=\"min-height:120px\" placeholder=\"curl 'https://www.notion.so/api/v3/getSpaces' -H 'cookie: token_v2=...; notion_user_id=...' ...\"></textarea>")
	b.WriteString("<details style=\"margin-top:12px\"><summary style=\"cursor:pointer;font-size:13px;color:#6d6760\">脚本已采集的可读 Cookie（只读，供排错对照）</summary>")
	b.WriteString("<textarea id=\"cookie\" readonly style=\"margin-top:8px;min-height:64px;background:#f7f6f2\"></textarea></details>")
	b.WriteString("<button id=\"submit\" type=\"button\">完成授权</button>")
	b.WriteString("<div id=\"result\"></div>")
	b.WriteString("<div class=\"note\">导入会先校验登录态与工作空间，成功才落盘。每个工作空间会拆成独立账号（订阅 / 免费额度分开路由）。</div>")
}

// writeCallbackScript 写出回调页脚本：解析 fragment、提交整段请求。
//
// 提交的内容是用户粘贴的原文（Copy as cURL / 请求头整段），Cookie 的抽取
// 全部交给服务端：token_v2 是 HttpOnly，前端既读不到、也不该在这里做
// 「猜哪个字段是凭据」的判断——那正是之前让用户逐项找字段的根源。
// 成功后用 history.replaceState 清掉 fragment，避免凭据留在浏览历史里。
// [参数] b: 输出缓冲。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：改为整段提交，插件自行识别凭据。
func writeCallbackScript(b *strings.Builder) {
	b.WriteString("<script>")
	b.WriteString("var PROVIDER=document.body.getAttribute(\"data-provider\")||\"\";")
	b.WriteString("var STATE=document.body.getAttribute(\"data-state\")||\"\";")
	b.WriteString("var IMPORT=\"/v0/resource/plugins/\"+PROVIDER+\"/login/import?state=\"+encodeURIComponent(STATE);")
	b.WriteString("var p=new URLSearchParams(location.hash.replace(/^#/,\"\"));")
	b.WriteString("var collected=p.get(\"" + callbackCookieParam + "\")||\"\";")
	b.WriteString("var upstreamErr=p.get(\"" + callbackErrorParam + "\")||\"\";")
	b.WriteString("var box=document.getElementById(\"result\");")
	b.WriteString("function show(text,cls){box.className=cls;box.textContent=text;}")
	b.WriteString("function clearHash(){try{history.replaceState(null,\"\",location.pathname+location.search)}catch(e){}}")
	// TextEncoder 保证中文与 emoji 也按 UTF-8 编码，btoa 只接受单字节字符。
	b.WriteString("function toBase64(text){try{")
	b.WriteString("  var bytes=new TextEncoder().encode(text);var bin=\"\";")
	b.WriteString("  for(var i=0;i<bytes.length;i++){bin+=String.fromCharCode(bytes[i])}")
	b.WriteString("  return btoa(bin)}catch(e){return \"\"}}")
	// Copy as cURL 会把请求体一起带上，整段可能几十 KB；请求头有大小上限，
	// 因此只挑出 Cookie 那一行提交。字段级的识别仍然全部交给服务端。
	b.WriteString("function pickCookie(text){")
	b.WriteString("  var m=String(text||\"\").match(/cookie\\s*:\\s*([^\\r\\n]*)/i);")
	b.WriteString("  if(!m){return \"\"}")
	b.WriteString("  return m[1].replace(/^[\\s\\\"']+/,\"\").replace(/[\\s\\\\^\\\"',;]+$/,\"\")")
	b.WriteString("}")
	b.WriteString("clearHash();")
	b.WriteString("var curlEl=document.getElementById(\"curl\");")
	b.WriteString("var cookieEl=document.getElementById(\"cookie\");")
	b.WriteString("if(upstreamErr){show(\"Notion 侧未完成授权：\"+upstreamErr,\"err\");}")
	b.WriteString("cookieEl.value=collected;")
	b.WriteString("document.getElementById(\"submit\").addEventListener(\"click\",function(){")
	b.WriteString("  var btn=this;var raw=(curlEl.value||\"\").trim();")
	b.WriteString("  if(!raw){show(\"请先粘贴 Copy as cURL 的内容\",\"err\");return}")
	b.WriteString("  btn.disabled=true;show(\"正在识别凭据、校验登录态与工作空间…\",\"\");")
	// 整段原文经 base64 提交：请求头不允许含换行，直传会被浏览器判为 Invalid value。
	// 抽取 token_v2 / notion_user_id 仍然是服务端的职责。
	// 优先只提交 Cookie 那一行：请求体可能很大，而头部有大小上限。
	// 认不出 Cookie 行时退回整段原文，由服务端继续识别（含纯 Cookie 粘贴）。
	b.WriteString("  var picked=pickCookie(raw);var payload=picked||raw;")
	b.WriteString("  var encoded=toBase64(payload);")
	b.WriteString("  if(!encoded){show(\"这段内容无法编码，请确认粘贴的是文本\",\"err\");btn.disabled=false;return}")
	b.WriteString("  fetch(IMPORT,{method:\"GET\",cache:\"no-store\",headers:{\"" + loginBridgeInputHeader + "\":encoded}})")
	b.WriteString("    .then(function(res){return res.json().then(function(d){return d}).catch(function(){return {error:\"响应解析失败\"}})}).then(function(d){")
	b.WriteString("      if(d&&d.ok){var labels=(d.labels||[]).join(\"、\");btn.textContent=\"已授权\";show(\"授权成功\"+(labels?\"：\"+labels:\"\")+\"，正在通知管理页…\",\"ok\");return}")
	b.WriteString("      show(\"授权失败：\"+((d&&d.error)||\"未知错误\"),\"err\");")
	b.WriteString("    }).catch(function(e){show(\"授权失败：\"+(e&&e.message?e.message:String(e)),\"err\")})")
	b.WriteString("    .then(function(){btn.disabled=false});")
	b.WriteString("});")
	b.WriteString("</script>")
}

// markLoginFailed 记录一次失败说明，供轮询回显。
// [参数] state: 登录会话标识；reason: 失败原因。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，失败原因可见化。
func markLoginFailed(state, reason string) {
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	loginSessionsMu.Lock()
	defer loginSessionsMu.Unlock()
	if session, ok := loginSessions[state]; ok {
		session.lastError = strings.TrimSpace(reason)
	}
}

// validLoginState 判断登录会话是否仍然有效。
// [参数] state: 登录会话标识。
// [返回] 有效时返回 true。
// 最近修改时间：2026-10-19；改动原因：新增，回调页与会话登记的共用校验。
func validLoginState(state string) bool {
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	loginSessionsMu.Lock()
	defer loginSessionsMu.Unlock()
	session, ok := loginSessions[state]
	if !ok || session == nil {
		return false
	}
	if time.Since(session.startedAt) > loginStateTTL {
		delete(loginSessions, state)
		return false
	}
	return true
}

// htmlManagementResponse 把 HTML 文本包装成管理响应。
// [参数] page: 完整 HTML。
// [返回] 管理响应。
// 最近修改时间：2026-10-19；改动原因：新增，HTML 响应构造。
func htmlManagementResponse(page string) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: []byte(page)}
}

// jsonManagementResponse 把结果编码成 JSON 管理响应。
// [参数] status: HTTP 状态码；payload: 结果对象。
// [返回] 管理响应。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页提交结果回写。
func jsonManagementResponse(status int, payload map[string]any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(payload)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

// htmlEscape 转义 HTML 关键字符。
//
// 不能用 html.EscapeString 之外的简化版本：错误说明直接来自上游错误文本，
// 未转义就是一处反射型 XSS。
// [参数] s: 原始字符串。
// [返回] 转义后的字符串。
// 最近修改时间：2026-10-19；改动原因：新增，错误说明渲染。
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return r.Replace(s)
}
