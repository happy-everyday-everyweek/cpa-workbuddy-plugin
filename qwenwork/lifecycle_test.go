package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAuthMarkerReaders pins the three top-level flag readers used by the
// exhausted-disable lifecycle: disabled, manual_disable (user intent — never
// auto-touched) and exhausted_disable (历史自动禁用标记，校验时据此恢复启用).
func TestAuthMarkerReaders(t *testing.T) {
	if !parseDisabledFromAuthJSON([]byte(`{"disabled":true}`)) {
		t.Fatal("disabled:true must read true")
	}
	if parseDisabledFromAuthJSON(nil) {
		t.Fatal("nil input must read false")
	}

	if !manualDisableFromAuthJSON([]byte(`{"disabled":true,"manual_disable":true}`)) {
		t.Fatal("manual_disable:true must read true")
	}
	if manualDisableFromAuthJSON([]byte(`{"manual_disable":"true"}`)) {
		t.Fatal("non-bool manual_disable must read false")
	}
	if manualDisableFromAuthJSON(nil) {
		t.Fatal("nil input must read false")
	}

	if !exhaustedDisableFromAuthJSON([]byte(`{"disabled":true,"exhausted_disable":true}`)) {
		t.Fatal("exhausted_disable:true must read true")
	}
	if exhaustedDisableFromAuthJSON([]byte(`{"disabled":true}`)) {
		t.Fatal("missing exhausted_disable must read false")
	}
	if exhaustedDisableFromAuthJSON(nil) {
		t.Fatal("nil input must read false")
	}
}

// TestBuildAuthFileJSONExhaustedMarker verifies the exhausted_disable marker
// round-trips through buildAuthFileJSON extra (auto-disable path) and is
// dropped by the reenable rebuild (extra=nil) — the marker set/clear pair is
// what arms and disarms auto-recovery.
func TestBuildAuthFileJSONExhaustedMarker(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "uid-ex-1"}}

	raw, err := buildAuthFileJSON(sa, true, "CN · 已禁用 · 耗尽 · 余0 已用2300", map[string]any{"exhausted_disable": true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["exhausted_disable"] != true {
		t.Fatalf("exhausted_disable missing: %v", m["exhausted_disable"])
	}
	if m["disabled"] != true {
		t.Fatalf("disabled must be true: %v", m["disabled"])
	}

	// Re-enable rebuild (extra=nil) must clear both intent markers.
	raw2, err := buildAuthFileJSON(sa, false, "CN · 恢复启用", nil)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(raw2, &m2); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if _, ok := m2["exhausted_disable"]; ok {
		t.Fatalf("reenable must clear exhausted_disable: %v", m2["exhausted_disable"])
	}
	if _, ok := m2["manual_disable"]; ok {
		t.Fatalf("reenable must clear manual_disable: %v", m2["manual_disable"])
	}
}

// TestExhaustedCreditsAreNotRoutable 验证耗尽账号被排除路由并显示「耗尽」：
// QwenWork 无免费模型（2026-10-18 生产实测 0 积分账号对全部模型返回
// 429 balance_insufficient），因此 0 积分不得继续参与路由。
func TestExhaustedCreditsAreNotRoutable(t *testing.T) {
	zero := &creditsSummary{TotalRemain: 0, TotalUsed: 500, TotalSize: 500}

	// 1. 耗尽账号不可路由
	if !isCreditsLowForRouting(zero) {
		t.Fatal("exhausted account must be excluded from routing (no free models)")
	}
	if !isCreditsLowOrExhausted(zero) {
		t.Fatal("isCreditsLowOrExhausted must flag exhausted credits")
	}

	// 2. 只有正积分残血才触发低额保护
	low := &creditsSummary{TotalRemain: 5, TotalUsed: 495, TotalSize: 500}
	if !isCreditsLowForRouting(low) {
		t.Fatal("positive residual below the floor must be excluded")
	}

	// 3. 未知额度不得误判为耗尽
	if isCreditsExhausted(&creditsSummary{}) {
		t.Fatal("empty snapshot with no usage signal must stay unknown")
	}

	// 4. 面板备注明确显示「耗尽」
	if note := displayNote(&storedAuth{}, zero, false); !strings.Contains(note, "耗尽") {
		t.Fatalf("exhausted note must say 耗尽, got %q", note)
	}
}

// TestExceededFlagMarksExhausted 验证上游 quota.exceeded 是权威耗尽判据。
// 免费版账号没有额度包，total_used/total_size 恒为 0，仅靠数值会被误判成未知。
func TestExceededFlagMarksExhausted(t *testing.T) {
	// 三项数值全 0 + exceeded=true：必须判为耗尽
	upstreamExceeded := &creditsSummary{Exceeded: true}
	if !isCreditsExhausted(upstreamExceeded) {
		t.Fatal("quota.exceeded=true must be treated as exhausted")
	}
	if !isCreditsLowForRouting(upstreamExceeded) {
		t.Fatal("quota.exceeded=true must be excluded from routing")
	}

	// 三项数值全 0 且无 exceeded：未知，不得误判
	if isCreditsExhausted(&creditsSummary{}) {
		t.Fatal("zero snapshot without exceeded must stay unknown")
	}

	// 有余额时即使 exceeded 残留也不得判为耗尽
	withRemain := &creditsSummary{TotalRemain: 100, Exceeded: true}
	if isCreditsExhausted(withRemain) {
		t.Fatal("positive remain must win over a stale exceeded flag")
	}
}
