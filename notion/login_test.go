package main

// 本文件验证宿主 OAuth 登录桥接。
//
// 插件声明了 auth_provider，宿主「+ 添加账号」会调用 auth.login.start；
// 未实现时宿主统一报 failed to generate authorization url。这些用例锁定
// 起点、轮询与桥接页三条链路。

import (
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
	for _, want := range []string{"/login", "/login/import"} {
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
		if strings.Contains(body, `id="cookie"`) {
			t.Fatalf("state=%q 时不应渲染 Cookie 表单", state)
		}
		if !strings.Contains(body, "无法继续") {
			t.Fatalf("state=%q 时应展示错误说明：%s", state, body)
		}
	}
}

// TestLoginBridgeRendersFormForValidState 校验有效 state 渲染完整导入表单。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，桥接页正常路径回归。
func TestLoginBridgeRendersFormForValidState(t *testing.T) {
	resetLoginSessions()
	resp := startLoginForTest(t)

	page := handleLoginBridge(resp.State)
	body := string(page.Body)
	if !strings.Contains(body, `id="cookie"`) || !strings.Contains(body, `id="submit"`) {
		t.Fatalf("有效 state 应渲染导入表单：%s", body)
	}
	if !strings.Contains(body, loginBridgeImportHeader) {
		t.Fatal("提交脚本必须带自定义 Cookie 头，否则资源路由拿不到凭据")
	}
	if !strings.Contains(body, resp.State) {
		t.Fatal("页面未携带 state，导入时会因缺少会话标识被拒")
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
