package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件覆盖 CYCLE-00 的认证链路：令牌兑换、登录态判据、双向续期、
// 以及授权会话的一次性消费。全部走 httptest，不依赖真实网络与账号。

// tokenBeatStub 模拟扣子令牌端点的两种调用形态。
//
// 与真实端点一致的两个关键点：
//   - 无论成败都返回 HTTP 200，失败靠 body 的 error_code 表达；
//   - 带 Cookie 调用返回 x-tt-token 响应头，带 token 调用返回 Set-Cookie。
type tokenBeatStub struct {
	t *testing.T
	// issuedToken 是「带 Cookie 调用」时应返回的令牌。
	issuedToken string
	// cookieNames 是「带 token 调用」时应下发的 Cookie 名。
	cookieNames []string
	// reject 为真时所有调用都返回会话过期。
	reject bool
	// calls 记录每次调用是否带了 token / cookie。
	calls []tokenBeatCall
}

type tokenBeatCall struct {
	HasToken  bool
	HasCookie bool
}

// ServeHTTP 实现令牌端点桩。
// [参数] w: 响应写入器；r: 请求。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，令牌端点桩。
func (s *tokenBeatStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hasToken := r.Header.Get("x-tt-token") != ""
	hasCookie := r.Header.Get("Cookie") != ""
	s.calls = append(s.calls, tokenBeatCall{HasToken: hasToken, HasCookie: hasCookie})

	w.Header().Set("Content-Type", "application/json")
	if s.reject {
		_, _ = w.Write([]byte(`{"message":"error","data":{"error_code":401,"description":"会话过期，请重新登录"}}`))
		return
	}

	// 形态 B：带 Cookie 换 token。
	if hasCookie && !hasToken {
		w.Header().Set("x-tt-token", s.issuedToken)
		_, _ = w.Write([]byte(`{"message":"success","data":{"error_code":0}}`))
		return
	}

	// 形态 A：带 token 换 Cookie。
	if hasToken {
		for _, name := range s.cookieNames {
			http.SetCookie(w, &http.Cookie{Name: name, Value: "v-" + name, Path: "/"})
		}
		_, _ = w.Write([]byte(`{"message":"success","data":{"error_code":0}}`))
		return
	}

	_, _ = w.Write([]byte(`{"message":"error","data":{"error_code":401,"description":"缺少凭据"}}`))
}

// accountInfoStub 模拟登录态判据端点。
type accountInfoStub struct {
	valid bool
	// secUserID 是有效时返回的账号句柄。
	secUserID string
}

// ServeHTTP 实现判据端点桩。
// [参数] w: 响应写入器；r: 请求。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，判据端点桩。
func (s *accountInfoStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.valid {
		_, _ = w.Write([]byte(`{"message":"error","data":{}}`))
		return
	}
	payload := map[string]any{
		"message": "success",
		"data": map[string]any{
			"sec_user_id": s.secUserID,
			"user_id":     float64(810785),
			"screen_name": "测试账号",
			"mobile":      "138****0000",
		},
	}
	raw, _ := json.Marshal(payload)
	_, _ = w.Write(raw)
}

// setupAuthStubs 启动令牌与判据两个桩并接管端点，返回清理函数。
// [参数] t: 测试上下文；token: 令牌端点桩；info: 判据端点桩。
// [返回] 清理函数。
// 最近修改时间：2026-10-18；改动原因：新增，测试环境搭建。
func setupAuthStubs(t *testing.T, token *tokenBeatStub, info *accountInfoStub) func() {
	t.Helper()
	tokenServer := httptest.NewServer(token)
	infoServer := httptest.NewServer(info)
	restore := swapEndpoints(tokenServer.URL, infoServer.URL)
	return func() {
		restore()
		tokenServer.Close()
		infoServer.Close()
	}
}

// defaultCookieNames 是真实端点实测下发的 13 条 Cookie 中我们关心的子集。
var defaultCookieNames = []string{
	"odin_tt", "uid_tt", "sid_guard", "sid_tt", "sessionid", "sessionid_ss",
}

// nowForTest 返回当前时间，供测试构造未过期会话。
// [参数] 无。
// [返回] 当前时间。
// 最近修改时间：2026-10-18；改动原因：新增，避免测试直接依赖 time 包。
func nowForTest() time.Time {
	return time.Now()
}

// readDirNames 读取目录下的文件名列表。
// [参数] dir: 目录路径。
// [返回] 文件名列表与错误。
// 最近修改时间：2026-10-18；改动原因：新增，断言失败时不留凭据文件。
func readDirNames(dir string) ([]string, error) {
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return nil, errRead
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

// TestExchangeTokenForCookies_Success 验证首次授权：token 换到 Cookie。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，首次授权主链路用例。
func TestExchangeTokenForCookies_Success(t *testing.T) {
	token := &tokenBeatStub{t: t, issuedToken: "unused", cookieNames: defaultCookieNames}
	info := &accountInfoStub{valid: true, secUserID: "sec-user-abc"}
	cleanup := setupAuthStubs(t, token, info)
	defer cleanup()

	res, errExchange := exchangeTokenForCookies("one-time-token")
	if errExchange != nil {
		t.Fatalf("兑换应成功，实际失败：%v", errExchange)
	}
	if res.SetCookieCount != len(defaultCookieNames) {
		t.Fatalf("Set-Cookie 条数应为 %d，实际 %d", len(defaultCookieNames), res.SetCookieCount)
	}
	if !strings.Contains(res.CookieHeader, "sessionid=") {
		t.Fatalf("Cookie 头应包含 sessionid，实际 %q", res.CookieHeader)
	}
	// 首次兑换必须只带 token、不带 Cookie。
	if len(token.calls) != 1 || !token.calls[0].HasToken || token.calls[0].HasCookie {
		t.Fatalf("首次兑换应只带 x-tt-token，实际调用 %+v", token.calls)
	}
}

// TestExchangeTokenForCookies_Rejected 验证上游拒绝时返回错误且不伪造成功。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，失败分支用例。
func TestExchangeTokenForCookies_Rejected(t *testing.T) {
	token := &tokenBeatStub{t: t, reject: true}
	info := &accountInfoStub{valid: true, secUserID: "sec-user-abc"}
	cleanup := setupAuthStubs(t, token, info)
	defer cleanup()

	_, errExchange := exchangeTokenForCookies("expired-token")
	if errExchange == nil {
		t.Fatal("上游拒绝时必须返回错误，不能伪造成功")
	}
	if !strings.Contains(errExchange.Error(), "会话过期") {
		t.Fatalf("错误信息应带上游说明，实际 %q", errExchange.Error())
	}
}

// TestExchangeTokenForCookies_EmptyToken 验证空令牌被本地拦截。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，入参边界用例。
func TestExchangeTokenForCookies_EmptyToken(t *testing.T) {
	if _, err := exchangeTokenForCookies("   "); err == nil {
		t.Fatal("空令牌必须被拒绝")
	}
}

// TestProbeAccountInfo_RequiresSecUserID 验证判据必须同时满足
// message=success 与 sec_user_id 非空。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，判据严格性用例。
func TestProbeAccountInfo_RequiresSecUserID(t *testing.T) {
	// message=success 但 sec_user_id 为空：必须判失败。
	empty := &accountInfoStub{valid: true, secUserID: ""}
	server := httptest.NewServer(empty)
	defer server.Close()
	restore := swapEndpoints("", server.URL)
	defer restore()

	if _, errProbe := probeAccountInfo("sessionid=x"); errProbe == nil {
		t.Fatal("sec_user_id 为空时必须判为无效")
	}
}

// TestRenewSession_DoubleExchange 验证续期走「Cookie → 新 token → 新 Cookie」。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，双向换发核心用例。
func TestRenewSession_DoubleExchange(t *testing.T) {
	token := &tokenBeatStub{t: t, issuedToken: "renewed-token", cookieNames: defaultCookieNames}
	info := &accountInfoStub{valid: true, secUserID: "sec-user-abc"}
	cleanup := setupAuthStubs(t, token, info)
	defer cleanup()

	sa := &StoredAuth{CookieHeader: "sessionid=old; sid_guard=old"}
	res, errRenew := renewSession(sa)
	if errRenew != nil {
		t.Fatalf("续期应成功，实际失败：%v", errRenew)
	}
	if res == nil || !res.Renewed {
		t.Fatalf("续期应标记为已换发，实际 %+v", res)
	}
	if !strings.Contains(res.CookieHeader, "sessionid=v-sessionid") {
		t.Fatalf("续期后应拿到新 Cookie，实际 %q", res.CookieHeader)
	}
	// 两步：先带 Cookie 换 token，再带 token 换 Cookie。
	if len(token.calls) != 2 {
		t.Fatalf("应发生两次换发，实际 %d 次：%+v", len(token.calls), token.calls)
	}
	if !token.calls[0].HasCookie || token.calls[0].HasToken {
		t.Fatalf("第一步应只带 Cookie，实际 %+v", token.calls[0])
	}
	if !token.calls[1].HasToken || token.calls[1].HasCookie {
		t.Fatalf("第二步应只带 token，实际 %+v", token.calls[1])
	}
}

// TestRenewSession_SingleStepWhenCookiesReturned 验证上游一步下发 Cookie 时
// 不再发第二次请求。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，单步续期兼容用例。
func TestRenewSession_SingleStepWhenCookiesReturned(t *testing.T) {
	// 该桩在「带 Cookie」时同时下发 Cookie，模拟一步到位形态。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "direct", Path: "/"})
		_, _ = w.Write([]byte(`{"message":"success","data":{"error_code":0}}`))
	}))
	defer server.Close()
	restore := swapEndpoints(server.URL, "")
	defer restore()

	res, errRenew := renewSession(&StoredAuth{CookieHeader: "sessionid=old"})
	if errRenew != nil {
		t.Fatalf("续期应成功，实际失败：%v", errRenew)
	}
	if res == nil || !res.Renewed {
		t.Fatalf("应标记为已换发，实际 %+v", res)
	}
	if !strings.Contains(res.CookieHeader, "sessionid=direct") {
		t.Fatalf("应直接采用第一步下发的 Cookie，实际 %q", res.CookieHeader)
	}
	if !strings.Contains(res.Note, "第一步直接下发") {
		t.Fatalf("应记录单步走向，实际 %q", res.Note)
	}
}

// TestRenewSession_Rejected 验证续期被拒时返回错误。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，续期失败用例。
func TestRenewSession_Rejected(t *testing.T) {
	token := &tokenBeatStub{t: t, reject: true}
	cleanup := setupAuthStubs(t, token, &accountInfoStub{valid: true})
	defer cleanup()

	if _, errRenew := renewSession(&StoredAuth{CookieHeader: "sessionid=old"}); errRenew == nil {
		t.Fatal("上游拒绝时必须返回错误")
	}
}

// TestApplyRenewal_WritesNewCookie 验证续期结果真实写回物理账号文件。
//
// 定时续期循环依赖这条落盘链路：若 applyRenewal 只改内存不写盘，重启后
// 账号会退回旧 Cookie，续期等于没做。这里用真实临时文件验证。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，定时续期落盘验证。
func TestApplyRenewal_WritesNewCookie(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	sa := &StoredAuth{
		AuthID:       "coze-apply",
		CookieHeader: "sessionid=old",
		LastRenewAt:  "2026-10-01T00:00:00+08:00",
		CreatedAt:    "2026-10-01T00:00:00+08:00",
	}
	filePath := dir + "/coze-apply.json"
	if errWrite := writeAuthFileDirect(filePath, sa); errWrite != nil {
		t.Fatalf("准备账号文件失败：%v", errWrite)
	}
	// 按真实形态重新读取：续期循环里的账号都来自 listAllAuthFiles / readAuthFile，
	// 带上读取时刻的内容摘要作为防覆盖基准。
	sa, errRead := readAuthFile(filePath)
	if errRead != nil {
		t.Fatalf("读取账号失败：%v", errRead)
	}

	res := &renewResult{
		Renewed:      true,
		CookieHeader: "sessionid=new",
		Cookies: []savedCookie{{
			Source: "https://www.coze.cn/",
			Cookie: http.Cookie{Name: "sessionid", Value: "new", Domain: "www.coze.cn", Path: "/"},
		}},
		Note: "测试换发",
	}
	if errApply := applyRenewal(sa, res); errApply != nil {
		t.Fatalf("落盘应成功，实际失败：%v", errApply)
	}

	loaded, errRead := readAuthFile(filePath)
	if errRead != nil {
		t.Fatalf("应能读回文件：%v", errRead)
	}
	if loaded.CookieHeader != "sessionid=new" {
		t.Fatalf("落盘 Cookie 头 = %q，期望 sessionid=new", loaded.CookieHeader)
	}
	if loaded.LastRenewAt == sa.CreatedAt {
		t.Fatal("last_renew_at 应推进")
	}
}

// TestApplyRenewal_SkipsWhenExternallyReplaced 验证账号文件被外部替换时放弃写入。
//
// 防覆盖语义：用户重新授权或另一实例写入新凭据后，旧快照不能把它冲掉。
// 这里复刻真实调用形态——账号总是先从磁盘读出来（记录内容摘要），
// 之后才可能被外部改写。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，防覆盖闸门验证。
func TestApplyRenewal_SkipsWhenExternallyReplaced(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	seeded := &StoredAuth{AuthID: "coze-replace", CookieHeader: "sessionid=old"}
	filePath := dir + "/coze-replace.json"
	if errWrite := writeAuthFileDirect(filePath, seeded); errWrite != nil {
		t.Fatalf("准备账号文件失败：%v", errWrite)
	}

	// 读取：此刻记录内容摘要，作为后续防覆盖基准。
	sa, errRead := readAuthFile(filePath)
	if errRead != nil {
		t.Fatalf("读取账号失败：%v", errRead)
	}

	// 模拟外部写入新凭据（重新授权 / 另一实例）。
	external := &StoredAuth{AuthID: "coze-replace", CookieHeader: "sessionid=external"}
	if errWrite := writeAuthFileDirect(filePath, external); errWrite != nil {
		t.Fatalf("外部写入失败：%v", errWrite)
	}

	res := &renewResult{Renewed: true, CookieHeader: "sessionid=renewed"}
	if errApply := applyRenewal(sa, res); errApply != nil {
		t.Fatalf("放弃写入不应返回错误，实际：%v", errApply)
	}

	loaded, errRead := readAuthFile(filePath)
	if errRead != nil {
		t.Fatalf("应能读回文件：%v", errRead)
	}
	if loaded.CookieHeader != "sessionid=external" {
		t.Fatalf("外部凭据被覆盖，实际 %q", loaded.CookieHeader)
	}
}

// TestSaveFromToken_WritesAuthFile 验证完整授权链路落盘。// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，落盘端到端用例。
func TestSaveFromToken_WritesAuthFile(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	token := &tokenBeatStub{t: t, cookieNames: defaultCookieNames}
	info := &accountInfoStub{valid: true, secUserID: "sec-user-xyz"}
	cleanup := setupAuthStubs(t, token, info)
	defer cleanup()

	sa, errSave := saveFromToken("one-time-token", "")
	if errSave != nil {
		t.Fatalf("落盘应成功，实际失败：%v", errSave)
	}
	if !strings.HasPrefix(sa.AuthID, cozeAuthFilePrefix) {
		t.Fatalf("账号标识应带 %q 前缀，实际 %q", cozeAuthFilePrefix, sa.AuthID)
	}
	if sa.SecUserID != "sec-user-xyz" {
		t.Fatalf("应回填 sec_user_id，实际 %q", sa.SecUserID)
	}
	if sa.Label != "测试账号" {
		t.Fatalf("未指定备注时应回落到昵称，实际 %q", sa.Label)
	}

	loaded, errRead := readAuthFile(sa.PhysicalPath)
	if errRead != nil {
		t.Fatalf("应能读回落盘文件：%v", errRead)
	}
	if loaded.CookieHeader != sa.CookieHeader {
		t.Fatal("落盘 Cookie 头应与内存一致")
	}
	if loaded.LoginAt == "" || loaded.LastRenewAt == "" {
		t.Fatal("授权时间与续期时间都应落盘")
	}
}

// TestSaveFromToken_NoSecUserID 验证判据不返回账号句柄时不落盘。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，拒绝半成品凭据。
func TestSaveFromToken_NoSecUserID(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	token := &tokenBeatStub{t: t, cookieNames: defaultCookieNames}
	info := &accountInfoStub{valid: true, secUserID: ""}
	cleanup := setupAuthStubs(t, token, info)
	defer cleanup()

	if _, errSave := saveFromToken("one-time-token", ""); errSave == nil {
		t.Fatal("缺少 sec_user_id 时必须拒绝落盘")
	}
	entries, _ := readDirNames(dir)
	if len(entries) != 0 {
		t.Fatalf("失败时不应留下任何凭据文件，实际 %v", entries)
	}
}

// TestLoginSession_OneTimeConsumption 验证授权会话只能被领取一次。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，state 一次性用例。
func TestLoginSession_OneTimeConsumption(t *testing.T) {
	state := randomState()
	loginSessionsMu.Lock()
	loginSessions[state] = &loginSession{startedAt: nowForTest()}
	loginSessionsMu.Unlock()
	defer func() {
		loginSessionsMu.Lock()
		delete(loginSessions, state)
		loginSessionsMu.Unlock()
	}()

	if !markLoginImported(state, "coze-abc") {
		t.Fatal("首次登记应成功")
	}
	// 登记成功后由轮询消费；模拟消费。
	loginSessionsMu.Lock()
	delete(loginSessions, state)
	loginSessionsMu.Unlock()

	if markLoginImported(state, "coze-abc") {
		t.Fatal("已消费的 state 不应再次被登记")
	}
}

// TestLoginSession_RejectsUnknownState 验证未知 state 被拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，伪造 state 用例。
func TestLoginSession_RejectsUnknownState(t *testing.T) {
	if validLoginState("never-issued-state") {
		t.Fatal("未签发过的 state 必须判为无效")
	}
	if markLoginImported("never-issued-state", "coze-abc") {
		t.Fatal("未签发过的 state 不应能登记导入结果")
	}
}

// TestBuildAuthScript_PointsToPlaceholder 验证脚本使用占位符而不是
// coze.cn 的 location.origin。
//
// 这是本轮发现的真实缺陷：脚本在扣子页面执行，若用 location.origin，
// 令牌会被发到 www.coze.cn 而不是插件。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，回调域正确性用例。
func TestBuildAuthScript_PointsToPlaceholder(t *testing.T) {
	script := buildAuthScript("state-123")
	if !strings.Contains(script, cpaOriginPlaceholder) {
		t.Fatalf("脚本应包含 origin 占位符，实际 %q", script)
	}
	if strings.Contains(script, "location.origin") {
		t.Fatal("脚本不得使用 location.origin：那会把令牌发到 coze.cn")
	}
	if !strings.Contains(script, "state-123") {
		t.Fatal("脚本应内嵌 state")
	}
	if !strings.Contains(script, "x-tt-token") {
		t.Fatal("脚本应读取 x-tt-token 响应头")
	}
	if !strings.Contains(script, "credentials:'include'") {
		t.Fatal("脚本必须带 credentials:include 才会携带登录 Cookie")
	}
}

// TestBuildAuthScript_TokenNeverInQuery 验证令牌不会出现在 URL query 中。
//
// resource 路由的访问日志会记录完整 URL；令牌若走 query 会落进宿主日志，
// 因此必须放在 fragment（浏览器不发给服务端）+ 同源请求头。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，凭据卫生用例。
func TestBuildAuthScript_TokenNeverInQuery(t *testing.T) {
	script := buildAuthScript("state-123")
	if strings.Contains(script, "&token=") || strings.Contains(script, "?token=") {
		t.Fatalf("令牌不得走 query（会落进访问日志），实际脚本 %q", script)
	}
	if !strings.Contains(script, "'#token='") {
		t.Fatalf("令牌应经 fragment 回传，实际脚本 %q", script)
	}
	if !strings.Contains(script, "'#err='") {
		t.Fatalf("失败原因应经 fragment 回传，实际脚本 %q", script)
	}
}

// TestLoginCallbackShell_HidesTokenFromServer 验证接收页用请求头回传令牌。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，回传通道用例。
func TestLoginCallbackShell_HidesTokenFromServer(t *testing.T) {
	page := string(loginCallbackShell("state-123", "").Body)
	if !strings.Contains(page, "location.hash") {
		t.Fatal("接收页应从 location.hash 读取令牌")
	}
	if !strings.Contains(page, callbackTokenHeader) {
		t.Fatalf("接收页应经 %s 请求头回传令牌", callbackTokenHeader)
	}
	if !strings.Contains(page, "history.replaceState") {
		t.Fatal("回传后应清掉地址栏 fragment，避免令牌留在浏览历史")
	}
}

// TestHandleLoginCallback_ReplayIsIdempotent 验证同一 state 重放不会二次兑换令牌。
//
// 回调页可能被刷新或从浏览历史重新打开，而 x-tt-token 只能兑换一次；
// 已导入成功的会话必须直接返回成功，不再打上游。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，重放幂等用例。
func TestHandleLoginCallback_ReplayIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	token := &tokenBeatStub{t: t, cookieNames: defaultCookieNames}
	info := &accountInfoStub{valid: true, secUserID: "sec-replay"}
	cleanup := setupAuthStubs(t, token, info)
	defer cleanup()

	state := randomState()
	loginSessionsMu.Lock()
	loginSessions[state] = &loginSession{startedAt: nowForTest()}
	loginSessionsMu.Unlock()
	defer func() {
		loginSessionsMu.Lock()
		delete(loginSessions, state)
		loginSessionsMu.Unlock()
	}()

	req := pluginapi.ManagementRequest{Headers: http.Header{}}
	req.Headers.Set(callbackTokenHeader, "one-time-token")
	req.Query = url.Values{"state": []string{state}}

	first := handleLoginCallback(req)
	if first.StatusCode != http.StatusOK || !strings.Contains(string(first.Body), `"ok":true`) {
		t.Fatalf("首次回调应成功，实际 %d %s", first.StatusCode, first.Body)
	}
	callsAfterFirst := len(token.calls)

	second := handleLoginCallback(req)
	if second.StatusCode != http.StatusOK || !strings.Contains(string(second.Body), `"ok":true`) {
		t.Fatalf("重放应幂等返回成功，实际 %d %s", second.StatusCode, second.Body)
	}
	if len(token.calls) != callsAfterFirst {
		t.Fatalf("重放不应再次调用上游：首次后 %d 次，重放后 %d 次", callsAfterFirst, len(token.calls))
	}
}

// TestLoginStartFromPanel_ScriptBase64 验证面板授权入口以 base64 下发脚本。
//
// 生产实测：宿主回写 ManagementResponse.Body 时会再做一次带 HTML 转义的
// JSON 序列化（错误文案里的 `<`、`'` 实测变成 `&lt;`、`&#39;`），插件侧
// 关掉自己的转义也无济于事。因此脚本必须以 base64 下发，面板端解码。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，base64 下发契约用例。
func TestLoginStartFromPanel_ScriptBase64(t *testing.T) {
	raw, errStart := handleLoginStartFromPanel()
	if errStart != nil {
		t.Fatalf("生成授权会话失败：%v", errStart)
	}

	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Body string `json:"Body"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("信封解析失败：%v", errUnmarshal)
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(env.Result.Body)
	if errDecode != nil {
		t.Fatalf("响应体 base64 解码失败：%v", errDecode)
	}
	body := string(decoded)

	if !strings.Contains(body, `"encoding":"base64"`) {
		t.Fatalf("响应应声明 encoding=base64，实际 %s", body)
	}
	// 原始 body 里不得出现任何 JS 片段——出现即说明未编码，会被宿主转义。
	for _, fragment := range []string{"=>", "'include'", "async()"} {
		if strings.Contains(body, fragment) {
			t.Fatalf("响应体不应包含明文脚本片段 %q，实际 %s", fragment, body)
		}
	}
}

// TestEnvelope_NoHTMLEscapeInScript 验证授权脚本在响应里不被实体化。
//
// 生产实测缺陷：json.Marshal 默认 HTML 转义会把脚本里的 `=>`、`'` 变成
// `=&gt;`、`&#39;`，面板复制到的脚本直接语法错误、无法执行。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，响应转义回归用例。
func TestEnvelope_NoHTMLEscapeInScript(t *testing.T) {
	script := buildAuthScript("state-abc")
	raw, errEnvelope := okEnvelope(map[string]any{"script": script})
	if errEnvelope != nil {
		t.Fatalf("信封构造失败：%v", errEnvelope)
	}
	body := string(raw)
	if strings.Contains(body, "&gt;") || strings.Contains(body, "&#39;") || strings.Contains(body, "&amp;") {
		t.Fatalf("响应不得包含 HTML 实体，实际 %s", body)
	}
	if !strings.Contains(body, "=>") {
		t.Fatalf("脚本中的 => 应原样保留，实际 %s", body)
	}
}

// TestHandleLoginCallback_ShellWhenNoHeaders 验证首次打开返回接收页而非报错。
//
// 令牌在 URL fragment 里，服务端首次请求看不到；此时必须返回接收页，
// 由页面脚本读 fragment 后再用请求头回传。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，fragment 两段式回传用例。
func TestHandleLoginCallback_ShellWhenNoHeaders(t *testing.T) {
	state := randomState()
	loginSessionsMu.Lock()
	loginSessions[state] = &loginSession{startedAt: nowForTest()}
	loginSessionsMu.Unlock()
	defer func() {
		loginSessionsMu.Lock()
		delete(loginSessions, state)
		loginSessionsMu.Unlock()
	}()

	req := pluginapi.ManagementRequest{Headers: http.Header{}}
	req.Query = url.Values{"state": []string{state}}
	resp := handleLoginCallback(req)
	body := string(resp.Body)
	if !strings.Contains(body, "location.hash") || !strings.Contains(body, callbackTokenHeader) {
		t.Fatalf("应返回接收页，实际 %s", body)
	}
}

// TestBuildAuthScript_EscapesState 验证 state 中的特殊字符不会破坏脚本。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，注入防护用例。
func TestBuildAuthScript_EscapesState(t *testing.T) {
	script := buildAuthScript("a'b\\c")
	if strings.Contains(script, "state=a'b") {
		t.Fatal("单引号必须被转义，否则会截断 JS 字符串")
	}
}

// TestCookieStore_KeepsDuplicateNames 验证同名不同路径的 Cookie 不会被覆盖。
//
// 真实端点下发的 13 条 Cookie 中 uid_tt / uid_tt_ss / session_tlb_tag
// 各出现两次，按名覆盖会直接丢凭据。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，同名分域存储用例。
func TestCookieStore_KeepsDuplicateNames(t *testing.T) {
	store := newCookieStore()
	u := mustParseURL(cozeSiteHost + "/")
	store.SetCookies(u, []*http.Cookie{
		{Name: "uid_tt", Value: "root", Path: "/", Domain: ".coze.cn"},
		{Name: "uid_tt", Value: "deep", Path: "/passport", Domain: ".coze.cn"},
	})

	if got := len(store.snapshot()); got != 2 {
		t.Fatalf("同名不同路径应存两条，实际 %d 条", got)
	}
	if !strings.Contains(store.header(), "uid_tt=root") || !strings.Contains(store.header(), "uid_tt=deep") {
		t.Fatalf("Cookie 头应同时包含两条同名值，实际 %q", store.header())
	}
}

// TestCookieStore_HasLiveSession 验证会话有效性判定。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，登录态判定用例。
func TestCookieStore_HasLiveSession(t *testing.T) {
	store := newCookieStore()
	if store.hasLiveSession() {
		t.Fatal("空容器不应判为有效会话")
	}
	store.SetCookies(mustParseURL(cozeSiteHost+"/"), []*http.Cookie{
		{Name: "ttwid", Value: "x", Path: "/"},
	})
	if store.hasLiveSession() {
		t.Fatal("只有非会话 Cookie 时不应判为有效会话")
	}
	store.SetCookies(mustParseURL(cozeSiteHost+"/"), []*http.Cookie{
		{Name: "sessionid", Value: "x", Path: "/"},
	})
	if !store.hasLiveSession() {
		t.Fatal("含未过期 sessionid 时应判为有效会话")
	}
}

// TestAuthIDForSecUserID_Stable 验证账号标识是确定性派生。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，标识稳定性用例。
func TestAuthIDForSecUserID_Stable(t *testing.T) {
	a := authIDForSecUserID("sec-user-abc")
	b := authIDForSecUserID("sec-user-abc")
	if a != b {
		t.Fatalf("同一账号必须派生同一标识：%q vs %q", a, b)
	}
	if a == authIDForSecUserID("sec-user-other") {
		t.Fatal("不同账号必须派生不同标识")
	}
	if authIDForSecUserID("") != "" {
		t.Fatal("空账号句柄应返回空标识")
	}
}

// TestNormalizeMgmtSubPath 验证三种前缀形态都能归一化。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，路由归一化用例。
func TestNormalizeMgmtSubPath(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"/v0/management/plugins/coze-provider/accounts", "/accounts"},
		{"/plugins/coze-provider/renew", "/renew"},
		{"/v0/resource/plugins/coze-provider/login/callback?state=x", "/login/callback"},
		{"/v0/resource/plugins/coze-provider", "/panel"},
	}
	for _, c := range cases {
		got, _ := normalizeMgmtSubPath(c.raw)
		if got != c.want {
			t.Fatalf("%q 应归一化为 %q，实际 %q", c.raw, c.want, got)
		}
	}
}

// TestAuthStateOf_RespectsUpstreamInvalid 验证上游失效优先于本地 Cookie。
//
// 生产实测（2026-10-19）：三账号中两个已被上游判定失效（续期返回
// 「会话过期，请重新登录」、积分端点返回 700012006），但面板仍全部显示
// 「可用」——扣子的会话 Cookie 不带 Expires，本地判活永远通过。
// 这里锁定契约：上游明确判定失效后，状态必须报 expired。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，失效状态误报回归。
func TestAuthStateOf_RespectsUpstreamInvalid(t *testing.T) {
	sa := &StoredAuth{
		AuthID:       "coze-invalid",
		CookieHeader: "sessionid=still-looks-alive; sid_tt=x",
	}
	if got := authStateOf(sa); got != "ok" {
		t.Fatalf("本地 Cookie 存在时应判 ok，实际 %q", got)
	}

	sa.AuthInvalid = true
	if got := authStateOf(sa); got != "expired" {
		t.Fatalf("上游已判失效时必须报 expired，实际 %q", got)
	}
}

// TestIsSessionExpiredError 验证失效判定只认上游会话类错误。
//
// 误判的代价是让用户去重新授权一个本来可用的账号，
// 所以超时、限流、5xx 这类瞬态错误必须排除在外。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，失效判定边界。
func TestIsSessionExpiredError(t *testing.T) {
	expiredCases := []string{
		"续期失败（第一步）：会话过期，请重新登录",
		"积分端点返回 code=700012006 msg=",
		"登录认证失败",
		"Login verification is invalid",
	}
	for _, reason := range expiredCases {
		if !isSessionExpiredError(reason) {
			t.Errorf("%q 应判为会话失效", reason)
		}
	}

	transientCases := []string{
		"",
		"积分请求失败：context deadline exceeded",
		"令牌端点返回 HTTP 502：bad gateway",
		"续期失败（第一步）：请求过于频繁",
	}
	for _, reason := range transientCases {
		if isSessionExpiredError(reason) {
			t.Errorf("%q 属瞬态错误，不得判为会话失效", reason)
		}
	}
}

// TestMarkSessionInvalidPersists 验证失效标记真实落盘。
//
// 只在内存里置位会在插件热重载后丢失，面板又会回到「可用」。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，失效状态持久化。
func TestMarkSessionInvalidPersists(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	filePath := dir + "/coze-mark.json"
	seeded := &StoredAuth{AuthID: "coze-mark", CookieHeader: "sessionid=x"}
	if errWrite := writeAuthFileDirect(filePath, seeded); errWrite != nil {
		t.Fatalf("准备账号文件失败：%v", errWrite)
	}
	sa, errRead := readAuthFile(filePath)
	if errRead != nil {
		t.Fatalf("读取账号失败：%v", errRead)
	}

	markSessionInvalid(sa, "续期失败（第一步）：会话过期，请重新登录")

	reloaded, errReload := readAuthFile(filePath)
	if errReload != nil {
		t.Fatalf("重读账号失败：%v", errReload)
	}
	if !reloaded.AuthInvalid {
		t.Fatal("失效标记必须持久化")
	}
	if got := authStateOf(reloaded); got != "expired" {
		t.Fatalf("落盘后状态应为 expired，实际 %q", got)
	}

	// 瞬态错误不得写标记。
	otherPath := dir + "/coze-transient.json"
	other := &StoredAuth{AuthID: "coze-transient", CookieHeader: "sessionid=y"}
	if errWrite := writeAuthFileDirect(otherPath, other); errWrite != nil {
		t.Fatalf("准备第二个账号失败：%v", errWrite)
	}
	otherLoaded, _ := readAuthFile(otherPath)
	markSessionInvalid(otherLoaded, "积分请求失败：context deadline exceeded")
	if otherLoaded.AuthInvalid {
		t.Fatal("瞬态错误不得标记账号失效")
	}

	// 续期成功应清除标记。
	invalid := &StoredAuth{AuthID: "coze-mark", AuthInvalid: true, CookieHeader: "sessionid=x"}
	invalid.PhysicalPath = filePath
	invalid.LoadedDigest = fileDigest(filePath)
	if errApply := applyRenewal(invalid, &renewResult{Renewed: true, CookieHeader: "sessionid=new"}); errApply != nil {
		t.Fatalf("续期落盘失败：%v", errApply)
	}
	if invalid.AuthInvalid {
		t.Fatal("续期成功后必须清除失效标记")
	}
}
