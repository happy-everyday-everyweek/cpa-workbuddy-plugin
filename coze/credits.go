package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// 本文件实现扣子积分查询。
//
// 两个上游端点分工：
//   - /api/marketplace/trade/credit/balance：一次拿到总额与可用余量，无参数；
//   - /api/marketplace/trade/credit/balance/detail?account_type=2：逐笔明细，
//     带 expired_at，用于算「即将过期」。
//
// 余额端点单独可用，明细端点失败不阻断余额展示——用户最关心的是还有多少分。

const (
	// creditCacheTTL 是积分缓存有效期。
	//
	// 积分变化只在推理消耗与每日登录奖励时发生，频率低；5 分钟足以让面板
	// 反复刷新不打爆上游，又不会让用户看到过期太久的数字。
	creditCacheTTL = 5 * time.Minute

	// creditExpiringWindow 是「即将过期」的判定窗口。
	//
	// 与上游 display_status=2 口径一致；这里再用时间窗口兜底，
	// 避免上游把已过期条目也算进「即将过期」。
	creditExpiringWindow = 30 * 24 * time.Hour

	// creditRequestTimeout 是单次积分请求超时。
	creditRequestTimeout = 10 * time.Second
)

// creditSummary 是账号积分摘要。
type creditSummary struct {
	// Total 是额度池总量，Remain 是当前可用余量。
	Total  int64 `json:"total"`
	Remain int64 `json:"remain"`
	// ExpiringRemain 是即将过期（30 天内）的积分总量。
	ExpiringRemain int64 `json:"expiring_remain"`
	// ExpiringAt 是最早一批即将过期积分的到期时间（RFC3339，空表示没有）。
	ExpiringAt string `json:"expiring_at,omitempty"`
	// Items 是即将过期的逐笔明细，按到期时间升序。
	Items []creditItem `json:"items,omitempty"`
	// FetchedAt 是本次查询时间（RFC3339）。
	FetchedAt string `json:"fetched_at"`
	// Error 非空表示余额查询失败；明细失败只记在 DetailError。
	Error       string `json:"error,omitempty"`
	DetailError string `json:"detail_error,omitempty"`
}

// creditItem 是一笔即将过期的积分。
type creditItem struct {
	Title     string `json:"title"`
	Remaining int64  `json:"remaining"`
	ExpiredAt string `json:"expired_at"`
}

// creditCache 缓存各账号的积分摘要。
//
// 按 AuthID 分桶：多账号面板会逐个展示，单条缓存会互相覆盖。
type creditCache struct {
	mu      sync.Mutex
	entries map[string]creditCacheEntry
}

type creditCacheEntry struct {
	summary creditSummary
	fetched time.Time
}

var creditsCache = creditCache{entries: map[string]creditCacheEntry{}}

// creditBalanceOverride / creditDetailOverride 是端点的测试覆盖点；非空时优先使用。
//
// 生产路径下始终为空，端点由 protocol.go 的常量拼装。
var (
	creditBalanceOverride string
	creditDetailOverride  string
)

// resolveCreditBalanceURL 返回实际使用的积分余额端点。
// [参数] 无。
// [返回] 端点 URL。
// 最近修改时间：2026-10-18；改动原因：新增，测试覆盖入口。
func resolveCreditBalanceURL() string {
	if creditBalanceOverride != "" {
		return creditBalanceOverride
	}
	return creditBalanceURL()
}

// resolveCreditDetailURL 返回实际使用的积分明细端点。
// [参数] 无。
// [返回] 端点 URL。
// 最近修改时间：2026-10-18；改动原因：新增，测试覆盖入口。
func resolveCreditDetailURL() string {
	if creditDetailOverride != "" {
		return creditDetailOverride
	}
	return creditDetailURL()
}

// fetchCreditBalance 拉取积分余额。
// [参数] client: HTTP 客户端；cookieHeader: 会话 Cookie。
// [返回] 余额响应与错误。
// 最近修改时间：2026-10-18；改动原因：新增，积分余额查询。
func fetchCreditBalance(client httpDoer, cookieHeader string) (*creditBalanceResponse, error) {
	req, errReq := http.NewRequest(http.MethodGet, resolveCreditBalanceURL(), nil)
	if errReq != nil {
		return nil, fmt.Errorf("构造积分请求失败：%w", errReq)
	}
	applyWebHeaders(req, cookieHeader)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("积分请求失败：%w", errDo)
	}
	defer resp.Body.Close()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("读取积分响应失败：%w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("积分端点返回 HTTP %d", resp.StatusCode)
	}

	var parsed creditBalanceResponse
	if errUnmarshal := json.Unmarshal(body, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("积分响应解析失败：%w", errUnmarshal)
	}
	if parsed.Code != 0 {
		return nil, fmt.Errorf("积分端点返回 code=%d msg=%s", parsed.Code, parsed.Message)
	}
	return &parsed, nil
}

// fetchCreditDetail 拉取积分逐笔明细。
//
// 只取「即将过期」页：display_status=2 与面板的过期提示同口径。
// [参数] client: HTTP 客户端；cookieHeader: 会话 Cookie。
// [返回] 明细响应与错误。
// 最近修改时间：2026-10-18；改动原因：新增，过期明细来源。
func fetchCreditDetail(client httpDoer, cookieHeader string) (*creditDetailResponse, error) {
	u, errParse := url.Parse(resolveCreditDetailURL())
	if errParse != nil {
		return nil, fmt.Errorf("构造积分明细地址失败：%w", errParse)
	}
	q := u.Query()
	q.Set("display_status", creditDetailDisplayExpiring)
	q.Set("page_num", "1")
	q.Set("page_size", creditDetailPageSize)
	u.RawQuery = q.Encode()

	req, errReq := http.NewRequest(http.MethodGet, u.String(), nil)
	if errReq != nil {
		return nil, fmt.Errorf("构造积分明细请求失败：%w", errReq)
	}
	applyWebHeaders(req, cookieHeader)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("积分明细请求失败：%w", errDo)
	}
	defer resp.Body.Close()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("读取积分明细响应失败：%w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("积分明细端点返回 HTTP %d", resp.StatusCode)
	}

	var parsed creditDetailResponse
	if errUnmarshal := json.Unmarshal(body, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("积分明细响应解析失败：%w", errUnmarshal)
	}
	if parsed.Code != 0 {
		return nil, fmt.Errorf("积分明细端点返回 code=%d msg=%s", parsed.Code, parsed.Message)
	}
	return &parsed, nil
}

// applyWebHeaders 给请求补上网页端必需的请求头。
// [参数] req: 待补头的请求；cookieHeader: 会话 Cookie。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，积分与模型端点共用。
func applyWebHeaders(req *http.Request, cookieHeader string) {
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Referer", cozeSiteHost+"/")
}

// fetchCreditSummary 查询账号积分（余额 + 即将过期明细）。
//
// 余额失败即整体失败；明细失败只记 DetailError——用户最关心的是还有多少分，
// 不能因为明细接口改版就整块积分不显示。
// [参数] sa: 账号。
// [返回] 积分摘要与错误。
// 最近修改时间：2026-10-18；改动原因：新增，积分查询主链路。
func fetchCreditSummary(sa *StoredAuth) (*creditSummary, error) {
	if sa == nil || strings.TrimSpace(sa.CookieHeader) == "" {
		return nil, fmt.Errorf("账号凭据为空")
	}
	client := newUpstreamClient(creditRequestTimeout)

	balance, errBalance := fetchCreditBalance(client, sa.CookieHeader)
	if errBalance != nil {
		return nil, errBalance
	}
	out := &creditSummary{
		Total:     balance.Data.TotalBalance,
		Remain:    balance.Data.TotalRemain,
		FetchedAt: time.Now().Format(time.RFC3339),
	}

	detail, errDetail := fetchCreditDetail(client, sa.CookieHeader)
	if errDetail != nil {
		out.DetailError = errDetail.Error()
		return out, nil
	}
	now := time.Now()
	for _, item := range detail.Data.Items {
		if item.Remaining <= 0 || item.ExpiredAt <= 0 {
			continue
		}
		expiredAt := time.Unix(item.ExpiredAt, 0)
		// 已过期的不计入：上游偶有把过期条目留在列表里的情况。
		if !expiredAt.After(now) {
			continue
		}
		out.ExpiringRemain += item.Remaining
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = strings.TrimSpace(item.ShowName)
		}
		out.Items = append(out.Items, creditItem{
			Title:     title,
			Remaining: item.Remaining,
			ExpiredAt: expiredAt.Format(time.RFC3339),
		})
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		return out.Items[i].ExpiredAt < out.Items[j].ExpiredAt
	})
	if len(out.Items) > 0 {
		out.ExpiringAt = out.Items[0].ExpiredAt
	}
	return out, nil
}

// cachedCreditSummary 返回账号积分摘要，命中缓存时不打上游。
// [参数] sa: 账号；force: 为真时跳过缓存。
// [返回] 积分摘要与错误。
// 最近修改时间：2026-10-18；改动原因：新增，带缓存的积分入口。
func cachedCreditSummary(sa *StoredAuth, force bool) (*creditSummary, error) {
	if sa == nil {
		return nil, fmt.Errorf("账号为空")
	}
	key := sa.AuthID
	if key == "" {
		key = sa.SecUserID
	}
	creditsCache.mu.Lock()
	if entry, ok := creditsCache.entries[key]; ok && !force &&
		entry.summary.Error == "" && time.Since(entry.fetched) < creditCacheTTL {
		cached := entry.summary
		creditsCache.mu.Unlock()
		return &cached, nil
	}
	creditsCache.mu.Unlock()

	summary, errFetch := fetchCreditSummary(sa)
	if errFetch != nil {
		// 失败不写缓存：一次网络抖动不该让面板在 TTL 内一直显示「查询失败」。
		return nil, errFetch
	}
	creditsCache.mu.Lock()
	creditsCache.entries[key] = creditCacheEntry{summary: *summary, fetched: time.Now()}
	creditsCache.mu.Unlock()
	return summary, nil
}

// resetCreditCache 清空积分缓存（测试与凭据变更时使用）。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，缓存失效入口。
func resetCreditCache() {
	creditsCache.mu.Lock()
	creditsCache.entries = map[string]creditCacheEntry{}
	creditsCache.mu.Unlock()
}
