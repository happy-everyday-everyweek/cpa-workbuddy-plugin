// checkin.go implements daily check-in for CN accounts: the manual
// handleManualCheckin endpoint, the every-4-hours auto scheduler, and the
// per-account mutex that prevents duplicate check-ins from racing browser
// tabs. CN accounts are excluded — they use one-shot trial claims instead.
package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	schedulerStop chan struct{}
	schedulerMu   sync.Mutex
)

func ensureScheduler() {
	schedulerMu.Lock()
	defer schedulerMu.Unlock()
	if schedulerStop != nil {
		return // already running
	}
	schedulerStop = make(chan struct{})
	go schedulerLoop(schedulerStop)
}

// Note: there is deliberately no stopCheckinScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

func nextCheckinTime(now time.Time) time.Time {
	var earliest time.Time
	// Consider both checkin and keepalive schedules so the timer wakes up for
	// whichever fires first (e.g. 00:00 checkin vs 00:00 keepalive → 00:00 wins,
	// then 04:00 keepalive fires on the next tick).
	hours := append([]int{}, checkinHours...)
	hours = append(hours, keepaliveHours...)
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// schedulerJitterMax 是槽位触发时刻的随机抖动上限。
//
// 各插件的槽位都对齐本地整点（00/04/08/12/16/20），所有插件会在同一秒
// 一起醒来，账号规模上来后形成整点尖峰。给每个插件、每一轮叠加一段
// 不大于该值的随机偏移，把尖峰摊平到数分钟窗口内。
//
// 上界取 5 分钟：远小于 4 小时槽位间隔（不会跨槽串位），也远小于
// shouldRunKeepaliveNow 的 1 小时窗口（抖动后仍落在同一窗口内）。
const schedulerJitterMax = 5 * time.Minute

// schedulerJitterFn 是可替换的抖动源，单测注入确定值；生产用 [0, max) 均匀分布。
var schedulerJitterFn = func() time.Duration {
	return time.Duration(rand.N(int64(schedulerJitterMax)))
}

// nextCheckinFireTime 在纯函数 nextCheckinTime 的结果上叠加抖动。
//
// nextCheckinTime 保持无副作用，供既有调度断言继续验证绝对槽位；抖动只在
// 真正创建定时器时施加，避免把随机性带进可测的排期计算。
//
// [参数] now：当前时间。
// [返回] 实际唤醒时刻，严格晚于 now。
func nextCheckinFireTime(now time.Time) time.Time {
	next := nextCheckinTime(now)
	jitter := schedulerJitterFn()
	if jitter > 0 {
		next = next.Add(jitter)
	}
	// 抖动不得把唤醒时刻推到 now 之前（极小概率出现在槽位前一瞬间抖动越界）。
	if !next.After(now) {
		return now.Add(time.Second)
	}
	return next
}

// schedulerLoop 驱动签到与 token 保活共用的定时唤醒：到达并集时刻后执行签到，
// 命中保活窗口时同 tick 执行一次 token 刷新。
//
// [参数] stop: 关闭信号通道，收到后停止定时器并退出循环。
// [返回] 无（阻塞循环，随 stop 关闭返回）。
// 最近修改时间：2026-10-14 23:26:48；改动原因：签到恢复每 4 小时，移除已冗余的时段门控。
func schedulerLoop(stop chan struct{}) {
	for {
		// 叠加随机抖动：各插件槽位同刻对齐时会形成整点尖峰，抖动把全机
		// 唤醒摊平到数分钟窗口内。
		next := nextCheckinFireTime(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			runAutoCheckin()
			// Fire keepalive if the current tick falls within its scheduled
			// window (e.g. 04:00 keepalive fires on the 04:00 tick even though
			// the previous checkin tick was 00:00).
			if shouldRunKeepaliveNow(time.Now()) {
				runTokenKeepalive()
			}
		}
	}
}

// runAutoCheckin is the scheduled lifecycle tick (every 4 hours: 00:00 / 04:00 / 08:00 / 12:00 / 16:00 / 20:00).
//
// v0.6.31: per-account work runs concurrently (sem=4) — was serial, so N accounts
// meant 3N serial HTTP round-trips on the billing API. Matches the pattern used
// by buildDashboardEx and handleManualCheckin.
func runAutoCheckin() {
	checkinAutoMu.RLock()
	doCheckin := checkinAuto
	checkinAutoMu.RUnlock()
	// Lifecycle may still run when check-in is off (credit gate).
	if !doCheckin && !lifecycleEnabled() {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processAutoCheckinAccount(f, doCheckin)
		}()
	}
	wg.Wait()
}

// processAutoCheckinAccount handles one account's scheduled tick. Extracted so
// runAutoCheckin can fan out per-account work without duplicating logic.
func processAutoCheckinAccount(f pluginapi.HostAuthFileEntry, doCheckin bool) {
	// A-24: only fetch sa when needed (checkin). For lifecycle-only paths,
	// let reconcileOneAccount do the single hostAuthGetBundle internally.
	if doCheckin {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return
		}
		// CN: daily check-in when enabled.
		ci, err := fetchCheckinStatus(sa)
		if err == nil && ci != nil && ci.Active && !ci.TodayCheckedIn {
			if _, callErr := performCheckinCall(sa, ci.CampaignID); callErr == nil {
				// Refresh once after a successful checkin call so cache reflects
				// the post-call state. If the status call fails keep the pre-call
				// snapshot rather than dropping it (v0.6.31: avoid shadowing ci
				// with a second fetch that could race with concurrent readers).
				if ci2, _ := fetchCheckinStatus(sa); ci2 != nil {
					ci = ci2
				}
				// P1-5: checkin grants new credits — refresh the credits cache
				// immediately so the panel shows the updated balance without
				// waiting for the async reconcile pass.
				if cr2, crErr := fetchUserResource(sa); crErr == nil && cr2 != nil {
					if v, ok := accountCache.Load(f.ID); ok {
						if prev, ok2 := v.(*accountCacheEntry); ok2 {
							fresh := *prev
							fresh.credits = cr2
							fresh.fetched = time.Now()
							accountCache.Store(f.ID, &fresh)
						}
					}
				}
			}
		}
		// Refresh cache with latest checkin status (merge, don't wipe credits/plan).
		if ci != nil {
			var prev *accountCacheEntry
			if v, ok := accountCache.Load(f.ID); ok {
				prev, _ = v.(*accountCacheEntry)
			}
			entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
			if prev != nil {
				entry.credits = prev.credits
				entry.plan = prev.plan
			}
			accountCache.Store(f.ID, entry)
		}
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
		}
		return
	}
	// Lifecycle-only (checkin off): reconcile handles its own get.
	if lifecycleEnabled() {
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

// handleManualCheckin checks in one account (auth_index) or all qoderwork
// accounts. Unlike the workbuddy three-phase classify/execute/summarize flow,
// Qoder AI's checkin is a simple Bearer GET+POST — we run it directly per
// account under an 8s timeout, no classify stage.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	t0 := time.Now()
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	// Run checkins concurrently (one goroutine per account, bounded).
	type result struct {
		idx int
		out map[string]any
	}
	outCh := make(chan result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outCh <- result{idx: i, out: checkinOneAccount(f)}
		}(i, f)
	}
	wg.Wait()
	close(outCh)

	results := make([]map[string]any, len(targets))
	for r := range outCh {
		results[r.idx] = r.out
	}
	successN, alreadyN, failN, noActivityN := 0, 0, 0, 0
	for _, r := range results {
		if r["error"] != nil {
			failN++
			continue
		}
		if r["skipped"] == true {
			alreadyN++
			continue
		}
		if r["reason"] == "global" {
			// 无可领取活动（活动未开放/账号不适用）不是失败，单列。
			noActivityN++
			continue
		}
		if r["success"] == true {
			successN++
		} else {
			failN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":       len(targets),
			"success":     successN,
			"already":     alreadyN,
			"no_activity": noActivityN,
			"fail":        failN,
			"elapsed_ms":  time.Since(t0).Milliseconds(),
		},
	}
}

// checkinOneAccount 对单个账号执行签到状态查询、活动领取与本地缓存同步。
// [参数] f: 宿主认证文件条目。
// [返回] 单账号签到结果响应 map。
// 最近修改时间：2026-10-11 14:58:00 同步刷新 accountCache 签到与积分快照并修复奖励积分类型断言
func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	// 1. 加单账号互斥锁并读取账号凭据
	out := map[string]any{
		"auth_index": f.AuthIndex,
		"name":       f.Name,
	}
	mu := checkinLockFor(f.AuthIndex)
	mu.Lock()
	defer mu.Unlock()

	sa, err := hostAuthGet(f.AuthIndex)
	if err != nil {
		out["error"] = "get auth: " + err.Error()
		return out
	}
	out["nickname"] = sa.Account.Nickname

	// 2. 查询当前签到活动状态并处理已签或无活动分支
	ci, err := fetchCheckinStatus(sa)
	if err != nil {
		out["error"] = "status: " + err.Error()
		return out
	}
	if ci.TodayCheckedIn {
		updateAccountCacheSnapshot(f.ID, accountCacheEntry{checkin: ci})
		out["success"] = true
		out["skipped"] = true
		out["reason"] = "already"
		out["message"] = "今日已签到"
		out["streak_days"] = ci.StreakDays
		out["total_credits"] = ci.TotalCredits
		return out
	}
	if !ci.Active {
		updateAccountCacheSnapshot(f.ID, accountCacheEntry{checkin: ci})
		out["success"] = false
		out["reason"] = "global"
		out["message"] = "当前无可领取的签到活动"
		return out
	}

	// 3. 调用活动领取接口并在成功后同步刷新积分与缓存快照
	res, err := performCheckinCall(sa, ci.CampaignID)
	if err != nil {
		out["error"] = "claim: " + err.Error()
		return out
	}
	if success, _ := res["success"].(bool); success {
		out["success"] = true
		if already, _ := res["already"].(bool); already {
			out["skipped"] = true
			out["reason"] = "already"
			out["message"] = "今日已签到"
		} else {
			out["message"] = "签到成功"
		}
		if rc, ok := res["rewardCredits"].(int64); ok {
			out["reward_credits"] = rc
		} else if rcFloat, ok := res["rewardCredits"].(float64); ok {
			out["reward_credits"] = int64(rcFloat)
		} else if ci.DailyCredit > 0 {
			out["reward_credits"] = ci.DailyCredit
		}
		ci.TodayCheckedIn = true
		ci.TodayCredit = ci.DailyCredit
		if ci.StreakDays == 0 {
			ci.StreakDays = 1
		}
		if ci2, ciErr := fetchCheckinStatus(sa); ciErr == nil && ci2 != nil && ci2.TodayCheckedIn {
			ci = ci2
		}
		var cr *creditsSummary
		if crFetched, crErr := fetchUserResource(sa); crErr == nil && crFetched != nil {
			cr = crFetched
			out["credits"] = cr
		}
		updateAccountCacheSnapshot(f.ID, accountCacheEntry{checkin: ci, credits: cr})
		return out
	}
	out["success"] = false
	out["message"] = res
	return out
}

// updateAccountCacheSnapshot 将最新的签到或积分快照合并写入 accountCache。
// [参数] authID: 宿主账号 ID；patch: 待合并的签到与积分缓存字段。
// [返回] 无。
// 最近修改时间：2026-10-11 15:05:00 收敛参数为双参数结构体形态
func updateAccountCacheSnapshot(authID string, patch accountCacheEntry) {
	// 1. 读取既有缓存条目并合并非空字段后写回
	if authID == "" {
		return
	}
	var prev *accountCacheEntry
	if v, ok := accountCache.Load(authID); ok {
		prev, _ = v.(*accountCacheEntry)
	}
	now := time.Now()
	entry := &accountCacheEntry{fetched: now}
	if prev != nil {
		entry.checkin = prev.checkin
		entry.credits = prev.credits
		entry.plan = prev.plan
	}
	if patch.checkin != nil {
		entry.checkin = patch.checkin
	}
	if patch.credits != nil {
		patch.credits.FetchedAt = now.UTC().Format(time.RFC3339)
		entry.credits = patch.credits
	}
	accountCache.Store(authID, entry)
}

func checkinLockFor(authIndex string) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// checkProUpgradeEligibility returns whether the account can still claim the
// one-time Pro Upgrade pack (+1800).
func checkProUpgradeEligibility(sa *storedAuth) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, upstreamBase+"/sash/api/v1/me/pro-upgrade/eligibility", nil)
	if err != nil {
		return false, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("http %d", resp.StatusCode)
	}
	var m struct {
		Eligible bool `json:"eligible"`
	}
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return false, err
	}
	return m.Eligible, nil
}

// claimProUpgrade claims the one-time Pro Upgrade pack (+1800 credits).
func claimProUpgrade(sa *storedAuth) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, endpointProUpgrade, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	if resp.StatusCode >= 400 {
		return map[string]any{"success": false, "message": fmt.Sprintf("http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return nil, err
	}
	if _, ok := m["success"]; !ok {
		m["success"] = true
	}
	return m, nil
}

// pruneCheckinLocks removes lock entries for auth indices that no longer
// exist in hostAuthList. Call after dashboard prune.
// Lock keys are auth_index (used for host RPC), so live map needs auth_index too.
func pruneCheckinLocks() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{} // checkinLockFor uses auth_index as key
	}
	checkinLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			checkinLocks.Delete(key)
		}
		return true
	})
}

// handleClaimPro checks pro-upgrade eligibility for one account and claims
// the one-time Pro pack (+ credits) when eligible. Surfaced as a panel
// per-card button — NOT called automatically during login (login writes the
// auth file first; this is a user-triggered post-login action).
func handleClaimPro(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	sa, err := hostAuthGet(authIndex)
	if err != nil {
		return map[string]any{"error": "get auth: " + err.Error()}
	}
	out := map[string]any{
		"auth_index": authIndex,
		"nickname":   sa.Account.Nickname,
	}
	eligible, err := checkProUpgradeEligibility(sa)
	if err != nil {
		out["error"] = "eligibility: " + err.Error()
		return out
	}
	if !eligible {
		out["success"] = false
		out["message"] = "不可领取（已领或活动未开放）"
		return out
	}
	res, err := claimProUpgrade(sa)
	if err != nil {
		out["error"] = "claim: " + err.Error()
		return out
	}
	for k, v := range res {
		out[k] = v
	}
	if _, ok := out["success"]; !ok {
		out["success"] = true
	}
	// Refresh credits snapshot so panel shows updated balance immediately.
	if cr, crErr := fetchUserResource(sa); crErr == nil && cr != nil {
		out["credits"] = cr
	}
	return out
}
