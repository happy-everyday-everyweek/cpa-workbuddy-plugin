package main

// 本文件验证宿主 OAuth 登录桥接。
//
// 插件声明了 auth_provider，宿主「+ 添加账号」会调用 auth.login.start；
// 未实现时宿主统一报 failed to generate authorization url。这些用例锁定
// 起点、轮询与桥接页三条链路。

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// startLoginForTest 发起一次登录并解出响应体。
// [参数] t: 测试上下文。
// [返回] 登录起点响应。
// 最近修改时间：2026-10-19；改动原因：新增，登录起点测试helper。
func startLoginForTest(t *testing.T) pluginapi.AuthLoginStartResponse {
	t.Helper()
	raw, errStart := handleStartLogin(nil)
	if errStart != nil {
		t.Fatalf("auth.login.start 失败：%v", errStart)
	}
	var env struct {
		OK     bool                             `json:"ok"`
		Result pluginapi.AuthLoginStartResponse `json:"result"`
		Error  *struct{ Message string }        `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("解析起点响应失败：%v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("起点响应不是成功信封：%s", string(raw))
	}
	return env.Result
}

// pollLoginForTest 轮询一次登录并解出响应体。
// [参数] t: 测试上下文；state: 登录会话标识。
// [返回] 轮询状态与响应。
// 最近修改时间：2026-10-19；改动原因：新增，轮询测试helper。
func pollLoginForTest(t *testing.T, state string) pluginapi.AuthLoginPollResponse {
	t.Helper()
	body, _ := json.Marshal(pluginapi.AuthLoginPollRequest{State: state})
	raw, errPoll := handlePollLogin(body)
	if errPoll != nil {
		t.Fatalf("auth.login.poll 失败：%v", errPoll)
	}
	var env struct {
		OK     bool                            `json:"ok"`
		Result pluginapi.AuthLoginPollResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("解析轮询响应失败：%v", errUnmarshal)
	}
	return env.Result
}

// resetLoginSessions 清空登录会话，避免用例之间互相污染。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，测试隔离。
func resetLoginSessions() {
	loginSessionsMu.Lock()
	defer loginSessionsMu.Unlock()
	loginSessions = make(map[string]*loginSession)
}

// TestHandleMethodRoutesLogin 校验登录方法真的接进了宿主分发入口。
//
// 处理函数写好了、却没在 handleMethod 里注册，宿主调用会落到 default 分支，
// 表现为「failed to generate authorization url」——这正是本轮要修的缺陷本身。
// 因此必须从分发入口断言，而不是直接调用处理函数。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，登录方法分发布线回归。
func TestHandleMethodRoutesLogin(t *testing.T) {
	resetLoginSessions()

	raw, errStart := handleMethod("auth.login.start", nil)
	if errStart != nil {
		t.Fatalf("auth.login.start 分发失败：%v", errStart)
	}
	var startEnv struct {
		OK     bool                             `json:"ok"`
		Result pluginapi.AuthLoginStartResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &startEnv); errUnmarshal != nil {
		t.Fatalf("解析起点响应失败：%v", errUnmarshal)
	}
	if !startEnv.OK || startEnv.Result.URL == "" {
		t.Fatalf("auth.login.start 未返回可用 URL：%s", string(raw))
	}

	pollBody, _ := json.Marshal(pluginapi.AuthLoginPollRequest{State: startEnv.Result.State})
	rawPoll, errPoll := handleMethod("auth.login.poll", pollBody)
	if errPoll != nil {
		t.Fatalf("auth.login.poll 分发失败：%v", errPoll)
	}
	var pollEnv struct {
		OK     bool                            `json:"ok"`
		Result pluginapi.AuthLoginPollResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(rawPoll, &pollEnv); errUnmarshal != nil {
		t.Fatalf("解析轮询响应失败：%v", errUnmarshal)
	}
	if !pollEnv.OK || pollEnv.Result.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("auth.login.poll 未返回 pending：%s", string(rawPoll))
	}
}

// TestManagementRegistrationDeclaresLoginResources 校验登录页与提交入口已注册。
//
// 宿主只把 Resources 里的路径挂成免密钥 resource 路由；漏注册时弹窗打开的
// 是一个 404 地址，用户同样走不下去。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，登录资源注册回归。
func TestManagementRegistrationDeclaresLoginResources(t *testing.T) {
	reg := managementRegistration()
	found := map[string]bool{}
	for _, res := range reg.Resources {
		found[res.Path] = true
	}
	for _, want := range []string{"/login", "/login/callback", "/login/import"} {
		if !found[want] {
			t.Fatalf("登录资源 %s 未注册，弹窗会打开 404 地址", want)
		}
	}
}

// TestStartLoginReturnsBridgeURL 校验登录起点返回可打开的桥接页地址。
//
// 这是「failed to generate authorization url」的直接修复点：宿主拿不到
// URL 就只会弹一句错误，流程根本走不下去。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，登录起点回归。
func TestStartLoginReturnsBridgeURL(t *testing.T) {
	resetLoginSessions()

	resp := startLoginForTest(t)
	if resp.URL == "" {
		t.Fatal("未返回授权 URL，宿主会报 failed to generate authorization url")
	}
	if !strings.Contains(resp.URL, "/login?state=") {
		t.Fatalf("URL 应指向登录桥接页：%s", resp.URL)
	}
	if resp.State == "" || !strings.Contains(resp.URL, resp.State) {
		t.Fatalf("state 未写入 URL：state=%q url=%s", resp.State, resp.URL)
	}
	if resp.Provider != providerName {
		t.Fatalf("provider = %q, 期望 %q", resp.Provider, providerName)
	}
	if time.Until(resp.ExpiresAt) <= 0 {
		t.Fatalf("过期时间必须是未来：%v", resp.ExpiresAt)
	}
	// 桥接页地址必须是相对路径：宿主可能部署在任意域名下。
	if !strings.HasPrefix(resp.URL, "/") {
		t.Fatalf("URL 必须是相对路径：%s", resp.URL)
	}
}

// TestStartLoginGeneratesUniqueState 校验每次登录都生成独立的 state。
//
// state 是桥接页提交凭据的唯一凭证；重复的 state 会让两次登录互相覆盖。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，state 唯一性回归。
func TestStartLoginGeneratesUniqueState(t *testing.T) {
	resetLoginSessions()

	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		resp := startLoginForTest(t)
		if seen[resp.State] {
			t.Fatalf("state 重复：%s", resp.State)
		}
		seen[resp.State] = true
		if len(resp.State) < 16 {
			t.Fatalf("state 太短，容易被猜中：%s", resp.State)
		}
	}
}

// TestPollLoginPendingThenError 校验未导入时保持 pending、会话消失后报错。
//
// pending 是宿主的正常等待态，不能返回 success——否则宿主会拿着空凭据落盘。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，轮询状态机回归。
func TestPollLoginPendingThenError(t *testing.T) {
	resetLoginSessions()
	resp := startLoginForTest(t)

	pending := pollLoginForTest(t, resp.State)
	if pending.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("未导入时应为 pending，实际 %s", pending.Status)
	}

	// 会话不存在（含过期清理）时必须是 error，不能继续空等。
	unknown := pollLoginForTest(t, "not-a-real-state")
	if unknown.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("未知 state 应为 error，实际 %s", unknown.Status)
	}
}

// TestMarkLoginImportedRequiresValidState 校验过期或未知 state 不能登记成功。
//
// 这是桥接页免密钥提交的唯一防线：state 无效时任何凭据都不该被认领。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，state 鉴权回归。
func TestMarkLoginImportedRequiresValidState(t *testing.T) {
	resetLoginSessions()

	if markLoginImported("unknown", []string{"notion-a"}) {
		t.Fatal("未知 state 不应登记成功")
	}
	if markLoginImported("", []string{"notion-a"}) {
		t.Fatal("空 state 不应登记成功")
	}

	resp := startLoginForTest(t)
	if markLoginImported(resp.State, nil) {
		t.Fatal("空账号列表不应登记成功")
	}

	// 人为把会话改成过期，登记必须失败并顺手清理。
	loginSessionsMu.Lock()
	loginSessions[resp.State].startedAt = time.Now().Add(-2 * loginStateTTL)
	loginSessionsMu.Unlock()
	if markLoginImported(resp.State, []string{"notion-a"}) {
		t.Fatal("过期 state 不应登记成功")
	}
}

// TestPollLoginReturnsAllSpaces 校验一次导入拆分出的多个空间账号全部回传。
//
// 导入会把每个工作空间落成一个账号；宿主只领第一份的话，另一个空间会变成
// 孤儿文件——插件侧存在、宿主侧不可路由，额度永远用不上。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，多空间回传回归。
func TestPollLoginReturnsAllSpaces(t *testing.T) {
	resetLoginSessions()
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	// 造两个同 Cookie、不同空间的账号文件，模拟一次全量导入的结果。
	paid := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-paid",
		CookieHeader: "token_v2=t; notion_user_id=u", TokenV2: "t",
		UserID: "u", SpaceID: "space-paid", Label: "主账号 · 订阅",
		Spaces: []NotionSpace{{ID: "space-paid", PremiumLimit: 300, PremiumCredits: 300}},
	}
	free := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-free",
		CookieHeader: "token_v2=t; notion_user_id=u", TokenV2: "t",
		UserID: "u", SpaceID: "space-free", Label: "主账号 · 免费",
		Spaces: []NotionSpace{{ID: "space-free", BasicLimit: 50, BasicUsage: 5}},
	}
	for _, a := range []*StoredAuth{paid, free} {
		if errWrite := writeAuthFileDirect(filepath.Join(dir, a.AuthID+notionAuthFileSuffix), a); errWrite != nil {
			t.Fatalf("落盘失败：%v", errWrite)
		}
	}

	resp := startLoginForTest(t)
	if !markLoginImported(resp.State, []string{"notion-paid", "notion-free"}) {
		t.Fatal("有效 state 应登记成功")
	}

	result := pollLoginForTest(t, resp.State)
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("导入后应为 success，实际 %s（%s）", result.Status, result.Message)
	}
	if len(result.Auths) != 2 {
		t.Fatalf("应回传 2 个空间账号，实际 %d", len(result.Auths))
	}
	gotIDs := map[string]bool{}
	for _, a := range result.Auths {
		// ID 必须带 .json：与宿主按文件名派生的形态一致，否则会重复注册。
		if !strings.HasSuffix(a.ID, notionAuthFileSuffix) {
			t.Fatalf("回传 ID 缺少 %s 后缀：%s", notionAuthFileSuffix, a.ID)
		}
		gotIDs[strings.TrimSuffix(a.ID, notionAuthFileSuffix)] = true
		if len(a.StorageJSON) == 0 {
			t.Fatalf("%s 的 StorageJSON 为空，宿主会覆盖掉账号凭据", a.ID)
		}
	}
	if !gotIDs["notion-paid"] || !gotIDs["notion-free"] {
		t.Fatalf("回传账号不完整：%v", gotIDs)
	}
	if result.Auth.ID == "" {
		t.Fatal("Auth 兼容字段不能为空")
	}
}

// TestLoginStateFromRequestHandlesBothShapes 校验两种 query 下发形态都能取到 state。
//
// 宿主可能把 query 单独放进 req.Query，也可能只把它挂在 req.Path 上。
// 只读其中一处会让桥接页提交稳定报「缺少登录会话标识」——真实 .so 上
// 实测过这个形态差异。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，query 形态兼容回归。
func TestLoginStateFromRequestHandlesBothShapes(t *testing.T) {
	withQuery := pluginapi.ManagementRequest{
		Path:  "/v0/resource/plugins/notion-provider/login/import",
		Query: url.Values{"state": []string{"from-query"}},
	}
	if got := loginStateFromRequest(withQuery); got != "from-query" {
		t.Fatalf("应从 req.Query 取到 state，实际 %q", got)
	}

	inPath := pluginapi.ManagementRequest{
		Path: "/v0/resource/plugins/notion-provider/login/import?state=from-path",
	}
	if got := loginStateFromRequest(inPath); got != "from-path" {
		t.Fatalf("应从 req.Path 取到 state，实际 %q", got)
	}

	if got := loginStateFromRequest(pluginapi.ManagementRequest{Path: "/login"}); got != "" {
		t.Fatalf("无 state 时应返回空串，实际 %q", got)
	}
}

// TestLoginBridgeImportAcceptsPathOnlyState 校验只在 Path 带 state 时也能提交成功。
//
// 这是真实 .so 端到端暴露的形态：宿主把 query 挂在 Path 上，req.Query 为空。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，桥接提交端到端回归。
func TestLoginBridgeImportAcceptsPathOnlyState(t *testing.T) {
	resetLoginSessions()
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	resp := startLoginForTest(t)
	req := pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/resource/plugins/notion-provider/login/import?state=" + resp.State,
		Headers: http.Header{loginBridgeImportHeader: []string{"notion_user_id=u1-not-valid"}},
	}
	result := handleLoginBridgeImport(req)

	// Cookie 无效时应当报「探测工作空间失败」这类上游错误，
	// 而不是「缺少登录会话标识」——后者说明 state 根本没取到。
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(result.Body, &payload); errUnmarshal != nil {
		t.Fatalf("解析提交结果失败：%v", errUnmarshal)
	}
	msg, _ := payload["error"].(string)
	if strings.Contains(msg, "缺少登录会话标识") {
		t.Fatalf("state 未从 Path 取出：%s", msg)
	}
	if msg == "" {
		t.Fatalf("无效 Cookie 应当报错，实际：%s", string(result.Body))
	}
}

// TestPollLoginConsumesStateOnce 校验结果只领取一次。
//
// 同一 state 被重复领取会让宿主把同一份账号登记两次。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，结果一次性消费回归。
func TestPollLoginConsumesStateOnce(t *testing.T) {
	resetLoginSessions()
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	sa := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-paid",
		CookieHeader: "token_v2=t; notion_user_id=u", TokenV2: "t",
		UserID: "u", SpaceID: "space-paid",
	}
	if errWrite := writeAuthFileDirect(filepath.Join(dir, sa.AuthID+notionAuthFileSuffix), sa); errWrite != nil {
		t.Fatalf("落盘失败：%v", errWrite)
	}

	resp := startLoginForTest(t)
	if !markLoginImported(resp.State, []string{"notion-paid"}) {
		t.Fatal("有效 state 应登记成功")
	}
	first := pollLoginForTest(t, resp.State)
	if first.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("首次领取应为 success，实际 %s", first.Status)
	}
	second := pollLoginForTest(t, resp.State)
	if second.Status == pluginapi.AuthLoginStatusSuccess {
		t.Fatal("同一 state 不应被重复领取")
	}
}

// TestLoginBridgeRejectsBadState 校验桥接页在 state 缺失或失效时不渲染表单。
//
// 渲染表单等于给了一个可以提交凭据的入口；state 无效必须直接拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页鉴权回归。
func TestLoginBridgeRejectsBadState(t *testing.T) {
	resetLoginSessions()

	for _, state := range []string{"", "not-a-real-state"} {
		resp := handleLoginBridge(state)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("错误页仍应是 200 以便浏览器渲染，实际 %d", resp.StatusCode)
		}
		body := string(resp.Body)
		if strings.Contains(body, `id="authScript"`) {
			t.Fatalf("state=%q 时不应渲染授权脚本", state)
		}
		if !strings.Contains(body, "无法继续") {
			t.Fatalf("state=%q 时应展示错误说明：%s", state, body)
		}
	}
}

// TestLoginBridgeRendersScriptForValidState 校验有效 state 渲染可复制的脚本与链接。
//
// 这是本轮相对「粘贴 Cookie」形态的核心改动：用户要能直接复制一段脚本，
// 在 Notion 页面 Console 执行后自动跳回回调页。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页脚本形态回归。
func TestLoginBridgeRendersScriptForValidState(t *testing.T) {
	resetLoginSessions()
	resp := startLoginForTest(t)

	page := handleLoginBridge(resp.State)
	body := string(page.Body)
	for _, want := range []string{`id="authScript"`, `id="copyScript"`, `id="notionURL"`, `id="copyURL"`, `id="openNotion"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("桥接页缺少 %s：%s", want, body)
		}
	}
	if !strings.Contains(body, callbackPath) {
		t.Fatal("脚本必须指向回调页，否则采集结果无处提交")
	}
	if !strings.Contains(body, callbackCookieParam) || !strings.Contains(body, callbackErrorParam) {
		t.Fatal("脚本必须同时带回 Cookie 与错误原因")
	}
	if !strings.Contains(body, cpaOriginPlaceholder) {
		t.Fatal("回调地址必须保留 origin 占位符，由桥接页在浏览器里替换")
	}
	if !strings.Contains(body, resp.State) {
		t.Fatal("页面未携带 state，回调时会因缺少会话标识被拒")
	}
	// HttpOnly 是这次形态设计的前提：页面必须如实告知 token_v2 读不到，
	// 并给出 Copy as cURL 这条不需要逐项找字段的替代路径。
	if !strings.Contains(body, "HttpOnly") {
		t.Fatal("页面必须说明 token_v2 是 HttpOnly、脚本读不到")
	}
	if !strings.Contains(body, "Copy as cURL") {
		t.Fatal("页面必须给出 Copy as cURL 这条整段粘贴路径")
	}
}

// TestLoginCallbackRendersCurlForm 校验回调页只要整段粘贴，不要逐项补字段。
//
// token_v2 是 HttpOnly，脚本采集不到；但让用户去 Application 面板逐个找
// 字段，既容易被 token 轮换打过期，也容易抄错值（真实反馈里的 401 就是这么来的）。
// 正确形态是让用户粘一次 Copy as cURL，由插件解析。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：从「补 token_v2」改为「整段粘贴」。
func TestLoginCallbackRendersCurlForm(t *testing.T) {
	resetLoginSessions()
	resp := startLoginForTest(t)

	page := handleLoginCallback(resp.State)
	body := string(page.Body)
	for _, want := range []string{`id="curl"`, `id="cookie"`, `id="submit"`, callbackCookieParam, "Copy as cURL"} {
		if !strings.Contains(body, want) {
			t.Fatalf("回调页缺少 %s：%s", want, body)
		}
	}
	// 不能再要求用户手工补 token_v2：那正是被反馈否掉的交互。
	if strings.Contains(body, `id="tokenv2"`) {
		t.Fatal("回调页不应再要求手动补填 token_v2")
	}
	if !strings.Contains(body, loginBridgeInputHeader) {
		t.Fatal("提交必须带原文请求头，否则资源路由拿不到凭据")
	}
	// 整段 cURL 含换行，浏览器不允许把它直接放进请求头：必须 base64。
	if !strings.Contains(body, "toBase64(payload)") || !strings.Contains(body, "btoa(bin)") {
		t.Fatal("整段原文必须 base64 编码后提交，否则 Headers 构造会失败")
	}
	// Copy as cURL 会带上请求体，只挑 Cookie 那一行能避开请求头大小上限。
	if !strings.Contains(body, "pickCookie") {
		t.Fatal("提交前应只挑出 Cookie 行，避免整段请求体撑爆请求头")
	}
	// 断言调用点而不是函数定义：只留定义不调用，fragment 依然会留在地址栏。
	if !strings.Contains(body, "clearHash();") {
		t.Fatal("必须调用 clearHash 清掉 fragment，避免凭据留在浏览历史里")
	}
	if !strings.Contains(body, "history.replaceState") {
		t.Fatal("clearHash 必须真的改写地址栏")
	}
	// 提交的必须是整段原文：前端再拼一次 Cookie 就等于把识别逻辑又搬回浏览器。
	if !strings.Contains(body, "curlEl.value") {
		t.Fatal("提交内容必须取自一整段粘贴文本")
	}
}

// TestLoginImportDecodesBase64Input 校验服务端能解出整段原文并识别 Cookie。
//
// 不解码会让整段 base64 当成 Cookie 发去上游，表现为一条莫名的 401。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，base64 入参解析回归。
func TestLoginImportDecodesBase64Input(t *testing.T) {
	curlText := "curl 'https://www.notion.so/api/v3/getSpaces' \\\n" +
		"  -H 'cookie: notion_user_id=u-1; token_v2=v03%3Aabc'"
	req := pluginapi.ManagementRequest{
		Headers: http.Header{loginBridgeInputHeader: []string{base64.StdEncoding.EncodeToString([]byte(curlText))}},
	}
	got, err := loginInputFromRequest(req)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if got != curlText {
		t.Fatalf("解出的原文不一致：%q", got)
	}

	identity, errParse := parseCookieIdentity(got)
	if errParse != nil {
		t.Fatalf("整段原文应能解析出凭据：%v", errParse)
	}
	if identity.TokenV2 != "v03%3Aabc" || identity.UserID != "u-1" {
		t.Fatalf("识别结果不对：%+v", identity)
	}

	// 旧形态（单行 Cookie 头）必须继续可用，避免打断已按旧方式提交的调用方。
	legacy := pluginapi.ManagementRequest{
		Headers: http.Header{loginBridgeImportHeader: []string{"token_v2=t; notion_user_id=u"}},
	}
	if got, _ := loginInputFromRequest(legacy); got == "" {
		t.Fatal("旧 Cookie 头兜底失效")
	}

	// 损坏的 base64 要给明确提示，而不是把乱码发给上游。
	broken := pluginapi.ManagementRequest{
		Headers: http.Header{loginBridgeInputHeader: []string{"not-base64!!"}},
	}
	if _, errBroken := loginInputFromRequest(broken); errBroken == nil {
		t.Fatal("损坏的 base64 应当报错")
	}
}

// TestLoginCallbackRejectsBadState 校验回调页在 state 失效时不渲染补填表单。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，回调页鉴权回归。
func TestLoginCallbackRejectsBadState(t *testing.T) {
	resetLoginSessions()

	for _, state := range []string{"", "not-a-real-state"} {
		resp := handleLoginCallback(state)
		body := string(resp.Body)
		if strings.Contains(body, `id="tokenv2"`) {
			t.Fatalf("state=%q 时不应渲染补填表单", state)
		}
		if !strings.Contains(body, "无法继续") {
			t.Fatalf("state=%q 时应展示错误说明：%s", state, body)
		}
	}
}

// TestLoginCallbackExpiredState 校验过期会话的回调页同样拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，会话过期回归。
func TestLoginCallbackExpiredState(t *testing.T) {
	resetLoginSessions()
	resp := startLoginForTest(t)

	loginSessionsMu.Lock()
	loginSessions[resp.State].startedAt = time.Now().Add(-2 * loginStateTTL)
	loginSessionsMu.Unlock()

	body := string(handleLoginCallback(resp.State).Body)
	if strings.Contains(body, `id="tokenv2"`) {
		t.Fatal("过期会话不应渲染补填表单")
	}
	if !strings.Contains(body, "已过期") {
		t.Fatalf("过期会话应提示重新发起：%s", body)
	}
}

// TestLoginImportFailureSurfacesInPoll 校验导入失败的原因会回显给宿主轮询。
//
// 只把错误回给浏览器不够：宿主的弹窗在轮询，用户往往只盯着弹窗。
// 失败不写进会话时，弹窗会一直显示「等待中」，看起来像卡死。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，失败原因回显回归。
func TestLoginImportFailureSurfacesInPoll(t *testing.T) {
	resetLoginSessions()
	resp := startLoginForTest(t)

	req := pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/resource/plugins/notion-provider/login/import?state=" + resp.State,
		Headers: http.Header{loginBridgeImportHeader: []string{"notion_user_id=u1-not-valid"}},
	}
	result := handleLoginBridgeImport(req)
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(result.Body, &payload); errUnmarshal != nil {
		t.Fatalf("解析提交结果失败：%v", errUnmarshal)
	}
	if msg, _ := payload["error"].(string); msg == "" {
		t.Fatalf("无效 Cookie 应当报错，实际：%s", string(result.Body))
	}

	poll := pollLoginForTest(t, resp.State)
	if poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("失败后仍应是 pending（等待重试），实际 %s", poll.Status)
	}
	if !strings.Contains(poll.Message, "上次尝试失败") {
		t.Fatalf("轮询应回显失败原因，实际：%s", poll.Message)
	}
}

// TestCurlInputReachesUpstreamAsCleanCookie 校验整段 cURL 最终变成干净的 Cookie 头。
//
// 这是整条链路唯一真正发往上游的地方：base64 入参 → 识别 Cookie →
// requestScope → 请求头。任何一步漏掉，上游收到的都是 curl 原文，表现为 401。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，链路级回归。
func TestCurlInputReachesUpstreamAsCleanCookie(t *testing.T) {
	curlText := "curl 'https://www.notion.so/api/v3/getSpaces' \\\n" +
		"  -H 'accept: */*' \\\n" +
		"  -H 'cookie: notion_user_id=u-1; token_v2=v03%3Aabc; notion_browser_id=b-1' \\\n" +
		"  --data-raw '{}'"

	req := pluginapi.ManagementRequest{
		Headers: http.Header{loginBridgeInputHeader: []string{base64.StdEncoding.EncodeToString([]byte(curlText))}},
	}
	raw, errInput := loginInputFromRequest(req)
	if errInput != nil {
		t.Fatalf("取入参失败：%v", errInput)
	}
	identity, errParse := parseCookieIdentity(raw)
	if errParse != nil {
		t.Fatalf("解析失败：%v", errParse)
	}

	upstream, errRequest := newNotionRequest(t.Context(), requestScope{
		Cookie:  identity.CookieHeader,
		UserID:  identity.UserID,
		SpaceID: "space-1",
	}, "/getSpaces", []byte(`{}`))
	if errRequest != nil {
		t.Fatalf("构造上游请求失败：%v", errRequest)
	}

	got := upstream.Header.Get("Cookie")
	if got != "notion_user_id=u-1; token_v2=v03%3Aabc; notion_browser_id=b-1" {
		t.Fatalf("上游收到的 Cookie 头不对：%q", got)
	}
	for _, leaked := range []string{"curl", "accept:", "--data-raw"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("上游 Cookie 头混入了 %q：%s", leaked, got)
		}
	}
}

// TestLoginProbeErrorAddsActionableHint 校验上游 401 会补上可执行的下一步。
//
// 上游原文只有一句 `Token is invalid.`，用户看不出该重来还是换账号。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，鉴权失败提示回归。
func TestLoginProbeErrorAddsActionableHint(t *testing.T) {
	unauthorized := &notionAPIError{StatusCode: http.StatusUnauthorized, Body: `{"name":"UnauthorizedError"}`}
	hint := loginHintForProbeError(unauthorized)
	if hint == "" || !strings.Contains(hint, "凭据已失效") {
		t.Fatalf("401 应补可操作提示，实际 %q", hint)
	}

	serverErr := &notionAPIError{StatusCode: http.StatusInternalServerError}
	if hint := loginHintForProbeError(serverErr); hint != "" {
		t.Fatalf("非鉴权错误不应误加提示，实际 %q", hint)
	}
}

// TestBuildAuthScriptEscapesState 校验脚本里的 state 不会破坏 JS 字面量。
//
// state 由服务端生成（十六进制），但这个转义是防止将来换熵源后
// 引号或换行把脚本截断的最后一道防线。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，脚本转义回归。
func TestBuildAuthScriptEscapesState(t *testing.T) {
	script := buildAuthScript(`a'b\c`)
	if strings.Contains(script, `a'b\c`) {
		t.Fatalf("state 未转义：%s", script)
	}
	if !strings.Contains(script, `a\'b\\c`) {
		t.Fatalf("state 转义结果不符合预期：%s", script)
	}
	if !strings.Contains(script, cpaOriginPlaceholder+"/v0/resource/plugins/"+providerName+callbackPath) {
		t.Fatalf("脚本回调地址拼接错误：%s", script)
	}
	// 脚本必须在 Notion 页面内先校验登录态，否则带着空 Cookie 跳回去只会得到一条难懂的报错。
	if !strings.Contains(script, "getSpaces") || !strings.Contains(script, "credentials:'include'") {
		t.Fatalf("脚本应带 Cookie 校验登录态：%s", script)
	}
	if !strings.Contains(script, "document.cookie") {
		t.Fatalf("脚本应采集可读 Cookie：%s", script)
	}
}

// TestLoginAuthDataIDMatchesFileName 校验登录回传的 ID 与宿主文件名口径一致。
//
// 宿主有两条注册路径：host.auth.save 按相对路径派生 ID（含 .json），
// 登录结果走 AuthData.ID（原样使用）。两者不一致时同一个物理文件会被
// 注册成两个 auth 记录，面板上出现两条一模一样的账号。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，登录 ID 口径回归。
func TestLoginAuthDataIDMatchesFileName(t *testing.T) {
	sa := &StoredAuth{
		AuthID: "notion-ba00ef0b191a", UserID: "u", SpaceID: "space-paid",
		CookieHeader: "token_v2=t",
	}

	data := authDataForLogin(sa)
	if data.ID != "notion-ba00ef0b191a.json" {
		t.Fatalf("登录 ID 必须带 .json，实际 %q", data.ID)
	}
	if data.FileName != data.ID {
		t.Fatalf("FileName 与 ID 必须一致：%q vs %q", data.FileName, data.ID)
	}
	// 传入已带后缀的标识不能重复追加。
	if got := authFileNameOf("notion-x.json"); got != "notion-x.json" {
		t.Fatalf("后缀被重复追加：%s", got)
	}
	if got := authFileNameOf("notion-x"); got != "notion-x.json" {
		t.Fatalf("裸标识未补后缀：%s", got)
	}
}

// TestLoginBridgeEscapesDetail 校验错误说明做了 HTML 转义。
//
// 错误文本可能来自上游返回，未转义就是一处反射型 XSS。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，XSS 防护回归。
func TestLoginBridgeEscapesDetail(t *testing.T) {
	page := loginBridgePage("", `<script>alert("x")</script>`)
	body := string(page.Body)
	if strings.Contains(body, "<script>alert") {
		t.Fatalf("错误说明未转义，存在 XSS：%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("应输出转义后的文本：%s", body)
	}
}
