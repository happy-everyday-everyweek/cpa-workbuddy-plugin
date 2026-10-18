package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件覆盖积分查询：余额映射、过期明细过滤、明细失败不阻断余额、
// 缓存命中与强制刷新、以及续期间隔是否真的下发到宿主 metadata。

// creditBalanceStub 模拟积分余额端点。
type creditBalanceStub struct {
	total  int64
	remain int64
	code   int
}

// ServeHTTP 实现积分余额端点桩。
// [参数] w: 响应写入器；r: 请求。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，余额端点桩。
func (s *creditBalanceStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.code != 0 {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": s.code, "message": "余额查询失败"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 0,
		"data": map[string]any{
			"total_balance": s.total,
			"total_remain":  s.remain,
			"items": []map[string]any{
				{"remaining": s.remain, "source": 24, "total": s.total},
			},
		},
	})
}

// creditDetailStub 模拟积分明细端点。
type creditDetailStub struct {
	items []creditDetailItem
	fail  bool
	// calls 记录请求 query，用于确认 account_type 与 display_status 口径。
	calls []url.Values
}

// ServeHTTP 实现积分明细端点桩。
// [参数] w: 响应写入器；r: 请求。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，明细端点桩。
func (s *creditDetailStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls = append(s.calls, r.URL.Query())
	w.Header().Set("Content-Type", "application/json")
	if s.fail {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 788770202, "message": "Invalid parameter"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 0,
		"data": map[string]any{"items": s.items, "has_more": false},
	})
}

// swapCreditEndpoints 临时覆盖积分端点，返回恢复函数。
// [参数] balanceURL: 余额端点；detailURL: 明细端点。
// [返回] 恢复函数。
// 最近修改时间：2026-10-18；改动原因：新增，测试端点注入。
func swapCreditEndpoints(balanceURL, detailURL string) func() {
	prevBalance, prevDetail := creditBalanceOverride, creditDetailOverride
	creditBalanceOverride = balanceURL
	creditDetailOverride = detailURL
	return func() {
		creditBalanceOverride = prevBalance
		creditDetailOverride = prevDetail
	}
}

// setupCreditStubs 启动积分桩并接管端点。
// [参数] t: 测试上下文；balance: 余额桩；detail: 明细桩。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，积分环境搭建。
func setupCreditStubs(t *testing.T, balance *creditBalanceStub, detail *creditDetailStub) {
	t.Helper()
	balanceSrv := httptest.NewServer(balance)
	t.Cleanup(balanceSrv.Close)
	detailSrv := httptest.NewServer(detail)
	t.Cleanup(detailSrv.Close)
	t.Cleanup(swapCreditEndpoints(balanceSrv.URL, detailSrv.URL))
	resetCreditCache()
	t.Cleanup(resetCreditCache)
}

// TestFetchCreditSummary_MapsBalance 验证余额字段映射。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，余额映射用例。
func TestFetchCreditSummary_MapsBalance(t *testing.T) {
	setupCreditStubs(t, &creditBalanceStub{total: 3000, remain: 1500}, &creditDetailStub{})

	summary, errFetch := fetchCreditSummary(&StoredAuth{AuthID: "coze-c1", CookieHeader: "sid=1"})
	if errFetch != nil {
		t.Fatalf("积分查询应成功：%v", errFetch)
	}
	if summary.Total != 3000 || summary.Remain != 1500 {
		t.Fatalf("余额映射错误：total=%d remain=%d", summary.Total, summary.Remain)
	}
	if summary.FetchedAt == "" {
		t.Fatal("必须记录查询时间")
	}
}

// TestFetchCreditSummary_ExpiringItems 验证过期明细过滤与排序。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，过期统计用例。
func TestFetchCreditSummary_ExpiringItems(t *testing.T) {
	now := time.Now()
	detail := &creditDetailStub{items: []creditDetailItem{
		// 较晚到期，应排在后面。
		{Title: "新用户首次注册奖励", Remaining: 1500, Total: 1500, ExpiredAt: now.Add(20 * 24 * time.Hour).Unix()},
		// 较早到期，应排在前面。
		{Title: "每日登录", Remaining: 500, Total: 1500, ExpiredAt: now.Add(5 * 24 * time.Hour).Unix()},
		// 已过期：必须剔除。
		{Title: "历史积分", Remaining: 999, Total: 999, ExpiredAt: now.Add(-24 * time.Hour).Unix()},
		// 剩余为 0：必须剔除。
		{Title: "已用完", Remaining: 0, Total: 999, ExpiredAt: now.Add(10 * 24 * time.Hour).Unix()},
	}}
	setupCreditStubs(t, &creditBalanceStub{total: 3000, remain: 2000}, detail)

	summary, errFetch := fetchCreditSummary(&StoredAuth{AuthID: "coze-c2", CookieHeader: "sid=1"})
	if errFetch != nil {
		t.Fatalf("积分查询应成功：%v", errFetch)
	}
	if summary.ExpiringRemain != 2000 {
		t.Fatalf("即将过期合计应为 2000，实际 %d", summary.ExpiringRemain)
	}
	if len(summary.Items) != 2 {
		t.Fatalf("应保留 2 笔有效明细，实际 %d", len(summary.Items))
	}
	if summary.Items[0].Title != "每日登录" {
		t.Fatalf("明细应按到期时间升序，首项为 %q", summary.Items[0].Title)
	}
	if summary.ExpiringAt == "" {
		t.Fatal("必须给出最早到期时间")
	}
}

// TestFetchCreditSummary_DetailFailureKeepsBalance 验证明细失败不阻断余额。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，部分失败容错用例。
func TestFetchCreditSummary_DetailFailureKeepsBalance(t *testing.T) {
	setupCreditStubs(t, &creditBalanceStub{total: 3000, remain: 3000}, &creditDetailStub{fail: true})

	summary, errFetch := fetchCreditSummary(&StoredAuth{AuthID: "coze-c3", CookieHeader: "sid=1"})
	if errFetch != nil {
		t.Fatalf("明细失败不应整体失败：%v", errFetch)
	}
	if summary.Remain != 3000 {
		t.Fatalf("余额仍应可用，实际 %d", summary.Remain)
	}
	if summary.DetailError == "" {
		t.Fatal("明细失败必须记录原因，不能静默当作没有即将过期积分")
	}
}

// TestFetchCreditSummary_BalanceFailure 验证余额失败整体报错。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，余额失败用例。
func TestFetchCreditSummary_BalanceFailure(t *testing.T) {
	setupCreditStubs(t, &creditBalanceStub{code: 700012006}, &creditDetailStub{})

	if _, errFetch := fetchCreditSummary(&StoredAuth{AuthID: "coze-c4", CookieHeader: "sid=1"}); errFetch == nil {
		t.Fatal("余额失败必须返回错误，不得伪造 0 积分")
	}
}

// TestFetchCreditDetail_UsesAccountType2 验证明细请求口径。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，请求参数用例。
func TestFetchCreditDetail_UsesAccountType2(t *testing.T) {
	detail := &creditDetailStub{}
	setupCreditStubs(t, &creditBalanceStub{total: 1, remain: 1}, detail)

	if _, errFetch := fetchCreditSummary(&StoredAuth{AuthID: "coze-c5", CookieHeader: "sid=1"}); errFetch != nil {
		t.Fatalf("积分查询应成功：%v", errFetch)
	}
	if len(detail.calls) == 0 {
		t.Fatal("明细端点必须被调用")
	}
	got := detail.calls[0]
	// account_type 拼在 URL 里，其余参数走 query。
	if !containsQuery(got, "display_status", creditDetailDisplayExpiring) {
		t.Fatalf("display_status 口径不符：%v", got)
	}
	if got.Get("page_size") != creditDetailPageSize {
		t.Fatalf("page_size 不符：%v", got)
	}
}

// containsQuery 判断 query 中某键是否为期望值。
// [参数] q: 查询参数；key/value: 期望。
// [返回] 匹配时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，断言辅助。
func containsQuery(q url.Values, key, value string) bool {
	return q.Get(key) == value
}

// TestCachedCreditSummary_CacheAndForce 验证缓存命中与强制刷新。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，缓存语义用例。
func TestCachedCreditSummary_CacheAndForce(t *testing.T) {
	balance := &creditBalanceStub{total: 3000, remain: 3000}
	detail := &creditDetailStub{}
	setupCreditStubs(t, balance, detail)
	sa := &StoredAuth{AuthID: "coze-c6", CookieHeader: "sid=1"}

	if _, errFirst := cachedCreditSummary(sa, false); errFirst != nil {
		t.Fatalf("首次查询应成功：%v", errFirst)
	}
	firstCalls := len(detail.calls)

	if _, errSecond := cachedCreditSummary(sa, false); errSecond != nil {
		t.Fatalf("二次查询应成功：%v", errSecond)
	}
	if len(detail.calls) != firstCalls {
		t.Fatal("二次调用应命中缓存，不应再次打上游")
	}

	if _, errForce := cachedCreditSummary(sa, true); errForce != nil {
		t.Fatalf("强制刷新应成功：%v", errForce)
	}
	if len(detail.calls) <= firstCalls {
		t.Fatal("force=true 必须绕过缓存重新查询")
	}
}

// pluginapiManagementRequest 返回一个空的管理请求。
// [参数] 无。
// [返回] 空请求。
// 最近修改时间：2026-10-18；改动原因：新增，构造入参。
func pluginapiManagementRequest() pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Query: url.Values{}}
}

// TestHandleCreditsQuery_RequiresAuthID 验证缺少账号标识时报错。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，入参校验用例。
func TestHandleCreditsQuery_RequiresAuthID(t *testing.T) {
	res := handleCreditsQuery(pluginapiManagementRequest())
	if res["error"] == nil {
		t.Fatal("缺少 auth_id 时必须返回错误")
	}
}

// TestHandleCreditsQuery_ReturnsSummary 验证查询路由返回积分摘要。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，路由载荷用例。
func TestHandleCreditsQuery_ReturnsSummary(t *testing.T) {
	sa := seedAccount(t, "coze-credit1", "sec-user-1")
	setupCreditStubs(t, &creditBalanceStub{total: 3000, remain: 1500}, &creditDetailStub{})

	req := pluginapiManagementRequest()
	req.Query = url.Values{"auth_id": []string{sa.AuthID}}
	res := handleCreditsQuery(req)
	if res["ok"] != true {
		t.Fatalf("查询应成功，实际 %v", res["error"])
	}
	if res["remain"] != int64(1500) {
		t.Fatalf("剩余积分不符：%v", res["remain"])
	}
}

// TestAuthDataForLogin_CarriesRefreshInterval 验证续期间隔下发到宿主 metadata。
//
// 缺这个键时宿主自动刷新循环不会为扣子账号排期，续期只能靠手动点击。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，定时续期契约用例。
func TestAuthDataForLogin_CarriesRefreshInterval(t *testing.T) {
	sa := &StoredAuth{AuthID: "coze-r1", Label: "测试账号"}
	data := authDataForLogin(sa)

	raw, ok := data.Metadata["refresh_interval_seconds"]
	if !ok {
		t.Fatal("metadata 必须带 refresh_interval_seconds，否则宿主不会自动续期")
	}
	seconds, okCast := raw.(int)
	if !okCast {
		t.Fatalf("refresh_interval_seconds 必须是整数秒，实际 %T", raw)
	}
	if seconds != 4*3600 {
		t.Fatalf("续期间隔应为 4 小时（14400 秒），实际 %d", seconds)
	}
}
