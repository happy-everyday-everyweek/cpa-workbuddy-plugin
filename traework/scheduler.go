// scheduler.go implements the CPA scheduler.pick capability for traework.
//
// Routing uses the panel-selected active account; when the selection is
// exhausted/disabled/missing/cooling-down, it switches to another
// healthy candidate. Non-traework candidates are always deferred so the
// built-in scheduler handles them, and when EVERY traework candidate is
// exhausted/cooling-down the pick is deferred as well (Handled: false) so
// the host can fail over to other providers' accounts (e.g. workbuddy).
// Only active when scheduler_mode: credits or session is configured
// (default off).
//
// Only "available" accounts participate: 「测试」标签 (test_failed) and 冷却
// (failover cooldown) are hard-excluded — they are unusable by product
// definition, so there is no fallback that re-admits them. Survivors are
// ordered HIGH-CREDIT-FIRST: the healthiest accounts (most remaining credits)
// lead and unmeasured (-1) accounts rank last, so load spreads across healthy
// accounts instead of burning the weakest down first.
package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	schedulerMode   = schedulerModeSession
	schedulerModeMu sync.RWMutex
)

// setSchedulerMode is a test helper that returns a restore func.
func setSchedulerMode(mode string) func() {
	schedulerModeMu.Lock()
	old := schedulerMode
	schedulerMode = mode
	schedulerModeMu.Unlock()
	return func() {
		schedulerModeMu.Lock()
		schedulerMode = old
		schedulerModeMu.Unlock()
	}
}

func loadedSchedulerMode() string {
	schedulerModeMu.RLock()
	defer schedulerModeMu.RUnlock()
	return schedulerMode
}

// handleSchedulerPick selects a traework auth candidate based on the
// panel-selected active account. Non-traework candidates are always deferred
// (Handled: false) so the built-in scheduler handles them. When every
// traework candidate is exhausted or cooling down, the pick is deferred too
// (Handled: false) so the built-in scheduler can fail over to other
// providers' accounts — the plugin never answers with a dead candidate.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	if loadedSchedulerMode() != schedulerModeCredits && loadedSchedulerMode() != schedulerModeSession {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Collect traework candidates only. Accounts in failover cooldown are
	// skipped so new requests route to a healthy account instead. If EVERY
	// traework account is cooling down, defer (Handled: false) so the host's
	// built-in scheduler can fail over to OTHER providers' accounts (e.g.
	// workbuddy) instead of pinning the request to a dead account.
	var wbCandidates []pluginapi.SchedulerAuthCandidate
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		if candidateDisabled(c) {
			continue
		}
		wbCandidates = append(wbCandidates, c)
	}
	if len(wbCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	// Hard exclusion: 「测试」标签 (scheduled active ping failed while credits
	// remained) / 冷却 (failover cooldown) accounts never carry traffic. Both
	// are "not available" by product definition, so they are filtered
	// unconditionally — there is NO "keep the full list" fallback: when every
	// candidate is excluded we defer (Handled: false) so the host's built-in
	// scheduler can fail over to OTHER providers' accounts (e.g. workbuddy)
	// instead of pinning the request to an account we just declared unusable.
	// 冷却判定带模型维度：账号×模型级冷却只应挡住同一模型的请求，
	// 否则一次限流就会把该账号从其它模型上一并摘掉。调度请求的 Model 是
	// 客户端模型名，需与记账侧一样归一化后再比对。
	upstreamModel := circuitModel(stripProviderPrefix(req.Model))
	available := make([]pluginapi.SchedulerAuthCandidate, 0, len(wbCandidates))
	for _, c := range wbCandidates {
		if isTestFailed(c.ID) || isAuthModelCoolingDown(c.ID, upstreamModel) {
			continue
		}
		remain, lowOrExhausted := cachedCreditsScore(c.ID)
		if lowOrExhausted || (remain >= 0 && remain < loadedMinRoutableCredits()) {
			continue
		}
		available = append(available, c)
	}
	if len(available) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	wbCandidates = available

	// 健康平摊轮询：废除残血优先（Low-Credit-First）反模式。
	// 健康可用账号按高积分优先排序（未测 -1 排最后），并在同一健康阶梯内平摊负载。
	sort.SliceStable(wbCandidates, func(i, j int) bool {
		left, _ := cachedCreditsScore(wbCandidates[i].ID)
		right, _ := cachedCreditsScore(wbCandidates[j].ID)
		if (left < 0) != (right < 0) {
			return left >= 0
		}
		return left > right
	})

	// Build thin view for active-auth picker. Exhausted accounts are passed
	// through with Exhausted=true (rather than pre-filtered) so the picker can
	// skip them and still report "no healthy account" when they are all spent.
	cands := make([]activeAuthCandidate, 0, len(wbCandidates))
	for _, c := range wbCandidates {
		_, exhausted := cachedCreditsScore(c.ID)
		cands = append(cands, activeAuthCandidate{
			ID:        c.ID,
			Disabled:  false, // already filtered
			Exhausted: exhausted,
		})
	}
	var picked string
	if loadedSchedulerMode() == schedulerModeSession {
		picked = pickSessionAuth(extractSessionKey(req), cands)
	} else {
		picked = pickActiveAuth(cands)
	}
	if picked == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  picked,
		Handled: true,
	})
}

// candidateDisabled reports host-disabled auth from Status/metadata.
func candidateDisabled(c pluginapi.SchedulerAuthCandidate) bool {
	st := strings.ToLower(strings.TrimSpace(c.Status))
	if st == "disabled" {
		return true
	}
	if c.Metadata != nil {
		if v, ok := c.Metadata["disabled"]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return strings.EqualFold(strings.TrimSpace(t), "true")
			}
		}
	}
	return false
}
