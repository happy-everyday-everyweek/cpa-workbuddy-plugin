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
			"prompt":     "在打开的引导页中粘贴 Notion 网页端的 Cookie，导入成功后此窗口会自动完成。",
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
	if ok {
		importedAuthIDs = append(importedAuthIDs, session.importedAuthIDs...)
	}
	loginSessionsMu.Unlock()

	if !ok {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请回到管理页重新发起",
		})
	}
	if len(importedAuthIDs) == 0 {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待在引导页中粘贴 Cookie",
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

	cookie := strings.TrimSpace(req.Headers.Get(loginBridgeImportHeader))
	if cookie == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "cookie 不能为空"})
	}

	accounts, errSave := saveCredentials(cookie, "", "")
	if errSave != nil {
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
	writeBridgeBody(&b)
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

// writeBridgeBody 写出引导文案与表单控件。
// [参数] b: 输出缓冲。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页正文。
func writeBridgeBody(b *strings.Builder) {
	b.WriteString("<h1>绑定 Notion 账号</h1>")
	b.WriteString("<p>Notion 没有开放给第三方的一键授权：AI 登录态是浏览器里的 HttpOnly Cookie，只能由你从网页端导出。请按下面三步操作。</p>")
	b.WriteString("<ol>")
	b.WriteString("<li>在浏览器打开并登录 <code>https://www.notion.so/ai</code></li>")
	b.WriteString("<li>开发者工具 → Network → 任意 <code>api/v3</code> 请求 → Request Headers → 复制完整 <code>Cookie</code></li>")
	b.WriteString("<li>粘贴到下方文本框并点「导入并完成」</li>")
	b.WriteString("</ol>")
	b.WriteString("<textarea id=\"cookie\" placeholder=\"token_v2=...; notion_user_id=...; notion_browser_id=...\"></textarea>")
	b.WriteString("<button id=\"submit\">导入并完成</button>")
	b.WriteString("<div id=\"result\"></div>")
	b.WriteString("<div class=\"note\">导入会先校验登录态与工作空间，成功才落盘。每个工作空间会拆成独立账号（订阅 / 免费额度分开路由）。</div>")
	b.WriteString("<div class=\"note\">Notion 把 AI 权限绑定到登录 IP，请在本机导出 Cookie；导入成功后本页会自动提示。</div>")
}

// writeBridgeScript 写出桥接页脚本。
//
// 脚本保持纯静态：provider 与 state 从 body 的 data 属性读取，避免把 Go 变量
// 拼进 JS 字面量。提交走 resource 路由（免管理密钥），Cookie 放自定义请求头，
// 因为宿主把 resource 请求统一改写为 GET 且不转发请求体。
// [参数] b: 输出缓冲。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页提交脚本。
func writeBridgeScript(b *strings.Builder) {
	b.WriteString("<script>")
	b.WriteString("var PROVIDER=document.body.getAttribute(\"data-provider\")||\"\";")
	b.WriteString("var STATE=document.body.getAttribute(\"data-state\")||\"\";")
	b.WriteString("var IMPORT=\"/v0/resource/plugins/\"+PROVIDER+\"/login/import?state=\"+encodeURIComponent(STATE);")
	b.WriteString("function show(text,cls){var r=document.getElementById(\"result\");r.className=cls;r.textContent=text;}")
	b.WriteString("document.getElementById(\"submit\").addEventListener(\"click\",function(){")
	b.WriteString("  var btn=this;var cookie=document.getElementById(\"cookie\").value.trim();")
	b.WriteString("  if(!cookie){show(\"请先粘贴 Cookie\",\"err\");return}")
	b.WriteString("  btn.disabled=true;show(\"正在校验登录态与工作空间…\",\"\");")
	b.WriteString("  fetch(IMPORT,{method:\"GET\",cache:\"no-store\",headers:{\"X-Notion-Cookie\":cookie}})")
	b.WriteString("    .then(function(res){return res.json().then(function(d){return {status:res.status,data:d}}).catch(function(){return {status:res.status,data:{error:\"响应解析失败\"}}})})")
	b.WriteString("    .then(function(r){")
	b.WriteString("      if(r.data&&r.data.ok){")
	b.WriteString("        var labels=(r.data.labels||[]).join(\"、\");")
	b.WriteString("        show(\"导入成功\"+(labels?\"：\"+labels:\"\")+\"，正在通知管理页…\",\"ok\");")
	b.WriteString("        document.getElementById(\"submit\").textContent=\"已导入\";return")
	b.WriteString("      }")
	b.WriteString("      show(\"导入失败：\"+((r.data&&r.data.error)||\"未知错误\"),\"err\");")
	b.WriteString("    })")
	b.WriteString("    .catch(function(e){show(\"导入失败：\"+(e&&e.message?e.message:String(e)),\"err\")})")
	b.WriteString("    .then(function(){btn.disabled=false})")
	b.WriteString("});")
	b.WriteString("</script>")
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
