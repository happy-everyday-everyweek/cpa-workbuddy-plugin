package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetTestFailed wipes the in-memory 测试 mirror so each test starts clean.
func resetTestFailed(t *testing.T) {
	t.Helper()
	testFailedSetMu.Lock()
	testFailedSet = make(map[string]struct{})
	testFailedSetMu.Unlock()
	t.Cleanup(func() {
		testFailedSetMu.Lock()
		testFailedSet = make(map[string]struct{})
		testFailedSetMu.Unlock()
	})
}

// storeCredits seeds accountCache with a credits snapshot for one auth ID and
// auto-clears it on test end. The routing tests below are its only callers.
func storeCredits(t *testing.T, id string, remain, used, total int64) {
	t.Helper()
	accountCache.Store(id, &accountCacheEntry{credits: &creditsSummary{
		TotalRemain: remain,
		TotalUsed:   used,
		TotalSize:   total,
	}})
	t.Cleanup(func() { accountCache.Delete(id) })
}

// TestSchedulerPick_TestFailedFiltered: a 「测试」account must never carry
// traffic — not even when it is the panel-selected card.
func TestSchedulerPick_TestFailedFiltered(t *testing.T) {
	resetActiveAuth(t)
	resetTestFailed(t)
	resetFailover(t)
	storeCredits(t, "wbai-tagged", 30, 0, 30)
	storeCredits(t, "wbai-clean", 40, 0, 40)
	testFailedSetPut("wbai-tagged")
	setActiveAuthID("wbai-tagged")
	raw, err := handleSchedulerPick(mustMarshalBytes(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wbai-tagged", Provider: providerName},
			{ID: "wbai-clean", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wbai-clean" {
		t.Fatalf("want 测试 account skipped → wbai-clean, got %+v", resp)
	}
}

// TestSchedulerPick_AllTestFailed_Defers: 测试 is a hard exclusion, so an
// all-tagged fleet must defer (cross-provider failover) rather than re-admit a
// known-broken account.
func TestSchedulerPick_AllTestFailed_Defers(t *testing.T) {
	resetActiveAuth(t)
	resetTestFailed(t)
	resetFailover(t)
	storeCredits(t, "wbai-a", 30, 0, 30)
	storeCredits(t, "wbai-b", 40, 0, 40)
	testFailedSetPut("wbai-a")
	testFailedSetPut("wbai-b")
	setActiveAuthID("wbai-b")
	raw, err := handleSchedulerPick(mustMarshalBytes(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wbai-a", Provider: providerName},
			{ID: "wbai-b", Provider: providerName},
			{ID: "tr-a", Provider: "traework-provider"},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if resp.Handled {
		t.Fatalf("all-测试 fleet must defer (cross-provider failover), got %+v", resp)
	}
}

// TestSchedulerPick_LowCreditFirstAndUnknownLast pins the ordering contract:
// a below-floor account is hard-excluded, and an account with no cached
// snapshot (-1) never jumps ahead of a measured one. (The test name predates
// the low-credit-first reversal; current routing is HIGH-CREDIT-FIRST.)
func TestSchedulerPick_LowCreditFirstAndUnknownLast(t *testing.T) {
	resetActiveAuth(t)
	resetTestFailed(t)
	resetFailover(t)
	storeCredits(t, "wbai-mid", 300, 0, 300)
	storeCredits(t, "wbai-low", 12, 288, 300)
	accountCache.Store("wbai-unknown", &accountCacheEntry{})
	defer accountCache.Delete("wbai-unknown")
	raw, err := handleSchedulerPick(mustMarshalBytes(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wbai-unknown", Provider: providerName},
			{ID: "wbai-mid", Provider: providerName},
			{ID: "wbai-low", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	// wbai-low (12 < 20) 低于安全红线被硬排除，wbai-mid (300) 排在 wbai-unknown (-1) 之前被选中
	if !resp.Handled || resp.AuthID != "wbai-mid" {
		t.Fatalf("want healthy wbai-mid routed (wbai-low < 20 excluded), got %+v", resp)
	}
}

// TestSchedulerPick_UnknownCreditsRankLast: with only a measured account and an
// unmeasured one left, the measured account must win.
func TestSchedulerPick_UnknownCreditsRankLast(t *testing.T) {
	resetActiveAuth(t)
	resetTestFailed(t)
	resetFailover(t)
	storeCredits(t, "wbai-known", 900, 0, 900)
	accountCache.Store("wbai-unknown", &accountCacheEntry{})
	defer accountCache.Delete("wbai-unknown")
	raw, err := handleSchedulerPick(mustMarshalBytes(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wbai-unknown", Provider: providerName},
			{ID: "wbai-known", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wbai-known" {
		t.Fatalf("want measured wbai-known ahead of unknown-credits account, got %+v", resp)
	}
}

// TestSchedulerPick_RecoversAfterTagCleared: clearing the 测试 flag (which the
// scheduled ping does on success) must put the account back into rotation.
func TestSchedulerPick_RecoversAfterTagCleared(t *testing.T) {
	resetActiveAuth(t)
	resetTestFailed(t)
	resetFailover(t)
	storeCredits(t, "wbai-recovered", 20, 0, 20)
	storeCredits(t, "wbai-other", 90, 0, 90)
	testFailedSetPut("wbai-recovered")
	setActiveAuthID("wbai-recovered")
	raw, err := handleSchedulerPick(mustMarshalBytes(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wbai-recovered", Provider: providerName},
			{ID: "wbai-other", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp := parsePickResponse(t, raw); !resp.Handled || resp.AuthID != "wbai-other" {
		t.Fatalf("tagged account must be skipped, got %+v", resp)
	}

	testFailedSetClear("wbai-recovered")
	setActiveAuthID("wbai-recovered")
	raw2, err := handleSchedulerPick(mustMarshalBytes(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wbai-recovered", Provider: providerName},
			{ID: "wbai-other", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp := parsePickResponse(t, raw2); !resp.Handled || resp.AuthID != "wbai-recovered" {
		t.Fatalf("cleared account must be routable again, got %+v", resp)
	}
}

// TestTestFailedSetBasic covers the mirror helpers the routing filter reads.
func TestTestFailedSetBasic(t *testing.T) {
	resetTestFailed(t)
	if isTestFailed("wbai-1") {
		t.Fatal("unmarked account must not be test-failed")
	}
	if isTestFailed("") {
		t.Fatal("empty authID must never be test-failed")
	}
	testFailedSetPut("wbai-1")
	if !isTestFailed("wbai-1") {
		t.Fatal("testFailedSetPut should mark the account")
	}
	testFailedSetPut("wbai-1")
	if len(testFailedSnapshot()) != 1 {
		t.Fatalf("snapshot size = %d, want 1", len(testFailedSnapshot()))
	}
	testFailedSetClear("wbai-1")
	if isTestFailed("wbai-1") {
		t.Fatal("testFailedSetClear should unmark the account")
	}
	testFailedSetClear("wbai-missing")
}

func TestEnsureDefaultActiveAuth_SkipsTestFailed(t *testing.T) {
	resetActiveAuth(t)
	resetTestFailed(t)
	resetFailover(t)
	storeCredits(t, "a1", 10, 0, 300)
	storeCredits(t, "a2", 500, 0, 500)
	storeCredits(t, "a3", 300, 0, 300)
	testFailedSetPut("a1")
	testFailedSetPut("a2")
	setActiveAuthID("a1")
	id := ensureDefaultActiveAuth([]wbAccount{
		{AuthIndex: "a1", AuthID: "a1"},
		{AuthIndex: "a2", AuthID: "a2"},
		{AuthIndex: "a3", AuthID: "a3"},
	})
	if id != "a3" {
		t.Fatalf("want only available a3, got %q", id)
	}
}
