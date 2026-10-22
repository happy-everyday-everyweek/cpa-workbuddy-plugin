package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// lowCredits 返回一个必然低于当前红线的正积分值（残血但未耗尽）。
func lowCredits() int64 {
	if redline := loadedMinRoutableCredits(); redline > 0 {
		return redline / 2
	}
	return 0
}

// healthyCredits 返回一个必然高于当前红线的积分值，供路由测试构造健康账号。
// 用例数据统一走这里，阈值调整时语义自动跟随，不会静默变成残血号。
func healthyCredits() int64 {
	return loadedMinRoutableCredits() * 2
}

// TestMinRoutableCredits_DefaultRedlineBoundary 锁定 400 默认红线与边界语义：
// remain == 红线必须可路由，remain == 红线-1 必须被排除（判据是严格小于）。
// 这两个点合起来才能钉住阈值本身与比较方向，任一漂移都会失败。
func TestMinRoutableCredits_DefaultRedlineBoundary(t *testing.T) {
	resetActiveAuth(t)
	restoreMin := setMinRoutableCredits(defaultMinRoutableCredits)
	defer restoreMin()

	const redline = 400
	if got := loadedMinRoutableCredits(); got != redline {
		t.Fatalf("default minRoutableCredits = %d, want %d", got, redline)
	}

	storeCredits(t, "wb-at-redline", redline, 0, redline)
	storeCredits(t, "wb-below-redline", redline-1, 1, redline)

	if accountRoutable("wb-at-redline") != true {
		t.Fatalf("remain == %d must stay routable (predicate is strict <)", redline)
	}
	if accountRoutable("wb-below-redline") != false {
		t.Fatalf("remain == %d must be excluded by the %d redline", redline-1, redline)
	}

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-below-redline", Provider: providerName},
			{ID: "wb-at-redline", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("pick err: %v", err)
	}
	if resp := parsePickResponse(t, raw); !resp.Handled || resp.AuthID != "wb-at-redline" {
		t.Fatalf("want wb-at-redline routed and wb-below-redline excluded, got %+v", resp)
	}
}

// TestMinRoutableCredits_Configurable400Boundary 验证 400 红线可被配置覆盖，
// 且覆盖后判据立即跟随新值（不是把 400 写死在调用点）。
func TestMinRoutableCredits_Configurable400Boundary(t *testing.T) {
	restoreMin := setMinRoutableCredits(defaultMinRoutableCredits)
	defer restoreMin()
	restoreMode := setSchedulerMode(schedulerModeSession)
	defer restoreMode()

	cfgBytes, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("min_routable_credits: 800\nscheduler_mode: credits\n"),
	})
	configure(cfgBytes)
	if got := loadedMinRoutableCredits(); got != 800 {
		t.Fatalf("configured minRoutableCredits = %d, want 800", got)
	}

	storeCredits(t, "wb-700", 700, 0, 700)
	if accountRoutable("wb-700") {
		t.Fatal("remain=700 must be excluded once the redline is raised to 800")
	}

	// 显式 0 关闭红线保护：任何正积分账号都应恢复可路由。
	cfgOff, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("min_routable_credits: 0\nscheduler_mode: credits\n"),
	})
	configure(cfgOff)
	if got := loadedMinRoutableCredits(); got != 0 {
		t.Fatalf("minRoutableCredits = %d after explicit 0, want 0", got)
	}
	if !accountRoutable("wb-700") {
		t.Fatal("explicit min_routable_credits: 0 must disable the low-credit gate")
	}
}

// TestMinRoutableCredits_ConfigAndExclusion 验证安全缓冲额度红线默认 400、可配置、
// 调度排除 remain < 红线、平摊轮询以及 14018 换号判据。
func TestMinRoutableCredits_ConfigAndExclusion(t *testing.T) {
	resetActiveAuth(t)
	resetTestFailed(t)
	resetFailover(t)
	restoreMin := setMinRoutableCredits(defaultMinRoutableCredits)
	defer restoreMin()

	// 1. 默认阈值为 400
	if got := loadedMinRoutableCredits(); got != 400 {
		t.Fatalf("default minRoutableCredits = %d, want 400", got)
	}

	// 2. 通过 configure() 动态修改阈值为 50
	cfgBytes, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("min_routable_credits: 50\nscheduler_mode: credits\n"),
	})
	configure(cfgBytes)
	if got := loadedMinRoutableCredits(); got != 50 {
		t.Fatalf("configured minRoutableCredits = %d, want 50", got)
	}
	setMinRoutableCredits(defaultMinRoutableCredits)

	// 3. 调度器硬排除 remain < 红线账号，并在健康账号间平摊轮询。
	// 用例数据全部相对默认红线构造，阈值再次调整时语义不被静默改写。
	lowRemain, okRemain, okHighRemain := lowCredits(), healthyCredits(), healthyCredits()*2
	accountCache.Store("acc-low", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: lowRemain, TotalUsed: 100, TotalSize: lowRemain + 100},
	})
	accountCache.Store("acc-ok1", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: okRemain, TotalUsed: 0, TotalSize: okRemain},
	})
	accountCache.Store("acc-ok2", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: okHighRemain, TotalUsed: 0, TotalSize: okHighRemain},
	})
	defer func() {
		accountCache.Delete("acc-low")
		accountCache.Delete("acc-ok1")
		accountCache.Delete("acc-ok2")
	}()

	// 即使面板曾经选中了 acc-low（低于红线），调度器也必须拒绝它并切换到健康账号
	setActiveAuthID("acc-low")
	raw1, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "acc-low", Provider: providerName},
			{ID: "acc-ok1", Provider: providerName},
			{ID: "acc-ok2", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("pick1 err: %v", err)
	}
	resp1 := parsePickResponse(t, raw1)
	if !resp1.Handled || resp1.AuthID != "acc-ok2" {
		t.Fatalf("expected acc-low (<%d) excluded and highest healthy acc-ok2 picked first, got %+v", defaultMinRoutableCredits, resp1)
	}

	// 再次切换（清空当前选中模拟轮询）：应轮询到下一个健康候选 acc-ok1
	activeAuthMu.Lock()
	activeAuthID = ""
	activeAuthMu.Unlock()
	raw2, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "acc-low", Provider: providerName},
			{ID: "acc-ok1", Provider: providerName},
			{ID: "acc-ok2", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("pick2 err: %v", err)
	}
	resp2 := parsePickResponse(t, raw2)
	if !resp2.Handled || resp2.AuthID != "acc-ok1" {
		t.Fatalf("expected round-robin to pick acc-ok1 on second turn, got %+v", resp2)
	}

	// 4. 所有候选均低于红线时，必须全灭 defer (Handled: false)
	rawAllLow, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "acc-low", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("pickAllLow err: %v", err)
	}
	respAllLow := parsePickResponse(t, rawAllLow)
	if respAllLow.Handled {
		t.Fatalf("expected all low-credit candidates to defer (Handled: false), got %+v", respAllLow)
	}

	// 5. accountRoutable 对 remain < 红线判定为 false
	if accountRoutable("acc-low") {
		t.Fatalf("expected accountRoutable(acc-low) == false for remain=%d < %d", lowRemain, defaultMinRoutableCredits)
	}
	if !accountRoutable("acc-ok1") {
		t.Fatalf("expected accountRoutable(acc-ok1) == true for remain=%d >= %d", okRemain, defaultMinRoutableCredits)
	}

	// 6. 14018 与 exceeded the quota 必须触发换号
	if !shouldRotateOnUpstreamErr(200, `upstream 200: 14018: Your requests have exceeded the quota.`) {
		t.Fatal("expected 14018 / exceeded the quota to trigger account rotation")
	}
}
