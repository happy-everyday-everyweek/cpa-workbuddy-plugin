package main

import (
	"fmt"
	"testing"
	"time"
)

// probeTestNow 是探活单测的统一基准时间，避免依赖真实时钟。
func probeTestNow() time.Time {
	return time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
}

// stubProbePing 替换真实探活请求，并保证用例结束后恢复。
// [参数] fn：测试提供的探活实现。
// [返回] 记录调用次数的指针。
// 最近修改时间：2026-10-19 02:00:00；改动原因：新增。
func stubProbePing(t *testing.T, fn func(*StoredAuth, string) error) *int {
	t.Helper()
	calls := 0
	old := probePingFn
	probePingFn = func(sa *StoredAuth, model string) error {
		calls++
		return fn(sa, model)
	}
	t.Cleanup(func() { probePingFn = old })
	return &calls
}

// TestShouldProbeCandidateWindow 锁定触发窗口：只有「距到期 ≤5s 且尚未到期」
// 的冷却才探测，冷却过期后交给正常路由。
func TestShouldProbeCandidateWindow(t *testing.T) {
	now := probeTestNow()
	const authID = "zcode-window"
	const model = "GLM-5.3"

	cases := []struct {
		name          string
		cooldownUntil time.Time
		model         string
		probedAt      time.Time
		want          bool
	}{
		{name: "距到期 10s 太早", cooldownUntil: now.Add(10 * time.Second), model: model, want: false},
		{name: "距到期 6s 仍未进窗口", cooldownUntil: now.Add(6 * time.Second), model: model, want: false},
		{name: "距到期正好 5s 进窗口", cooldownUntil: now.Add(5 * time.Second), model: model, want: true},
		{name: "距到期 1s 仍可探测", cooldownUntil: now.Add(1 * time.Second), model: model, want: true},
		{name: "冷却已过期不探测", cooldownUntil: now.Add(-time.Second), model: model, want: false},
		{name: "无模型不探测", cooldownUntil: now.Add(2 * time.Second), model: "", want: false},
		{name: "无冷却不探测", cooldownUntil: time.Time{}, model: model, want: false},
		{
			name:          "本周期已探测不再重复",
			cooldownUntil: now.Add(3 * time.Second),
			model:         model,
			probedAt:      now.Add(3 * time.Second),
			want:          false,
		},
		{
			name:          "上一周期探测过、冷却已延长则应再探",
			cooldownUntil: now.Add(3 * time.Second),
			model:         model,
			probedAt:      now.Add(-60 * time.Second),
			want:          true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldProbeCandidate(authID, tc.cooldownUntil, tc.model, tc.probedAt, now); got != tc.want {
				t.Fatalf("shouldProbeCandidate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCooldownProbeSuccessClearsCooldown 锁定成功路径：探活成功立即清除冷却。
func TestCooldownProbeSuccessClearsCooldown(t *testing.T) {
	const authID = "zcode-probe-ok"
	resetAccountFailover(authID)
	clearCooldownProbeState()
	t.Cleanup(func() {
		resetAccountFailover(authID)
		clearCooldownProbeState()
	})

	noteAccountFailureForModel(authID, "GLM-5.3", 503, "service unavailable")
	if !isAuthModelCoolingDown(authID, "GLM-5.3") {
		t.Fatal("前置条件不成立：应处于冷却")
	}

	stubProbePing(t, func(*StoredAuth, string) error { return nil })

	candidate := cooldownProbeCandidate{authID: authID, cooldownUntil: time.Now().Add(2 * time.Second), model: "GLM-5.3"}
	if !probeCoolingAccount(candidate, &StoredAuth{}) {
		t.Fatal("探活成功应返回 true")
	}
	if isAuthModelCoolingDown(authID, "GLM-5.3") {
		t.Fatal("探活成功后应立即解除冷却")
	}
}

// TestCooldownProbeFailureEscalatesCooldown 锁定失败路径：探活失败继续沿阶梯递增。
func TestCooldownProbeFailureEscalatesCooldown(t *testing.T) {
	const authID = "zcode-probe-fail"
	resetAccountFailover(authID)
	clearCooldownProbeState()
	t.Cleanup(func() {
		resetAccountFailover(authID)
		clearCooldownProbeState()
	})

	noteAccountFailureForModel(authID, "GLM-5.3", 503, "service unavailable")
	failoverMu.RLock()
	before := failoverRecords[authID].cooldown
	beforeCount := failoverRecords[authID].failCount
	failoverMu.RUnlock()

	stubProbePing(t, func(*StoredAuth, string) error { return fmt.Errorf("upstream HTTP 503: unavailable") })

	candidate := cooldownProbeCandidate{authID: authID, cooldownUntil: before, model: "GLM-5.3"}
	if probeCoolingAccount(candidate, &StoredAuth{}) {
		t.Fatal("探活失败应返回 false")
	}

	failoverMu.RLock()
	afterCount := failoverRecords[authID].failCount
	after := failoverRecords[authID].cooldown
	failoverMu.RUnlock()
	if afterCount != beforeCount+1 {
		t.Fatalf("失败后计数 = %d, want %d（应沿阶梯递增）", afterCount, beforeCount+1)
	}
	if !after.After(before) {
		t.Fatalf("失败后到期时间应延后: before=%v after=%v", before, after)
	}
	if !isAuthModelCoolingDown(authID, "GLM-5.3") {
		t.Fatal("探活失败后账号应继续处于冷却")
	}
}

// TestCooldownProbeUsesFailedModel 锁定「必须用当初失败的模型回放」。
func TestCooldownProbeUsesFailedModel(t *testing.T) {
	const authID = "zcode-probe-model"
	const failedModel = "GLM-5.3"
	resetAccountFailover(authID)
	clearCooldownProbeState()
	t.Cleanup(func() {
		resetAccountFailover(authID)
		clearCooldownProbeState()
	})

	noteAccountFailureForModel(authID, failedModel, 503, "unavailable")

	var gotModel string
	stubProbePing(t, func(_ *StoredAuth, model string) error {
		gotModel = model
		return nil
	})

	// 把冷却改到探测窗口内（2s 后到期），再取目标。
	failoverMu.Lock()
	if entry := failoverRecords[authID]; entry != nil {
		entry.cooldown = time.Now().Add(2 * time.Second)
	}
	failoverMu.Unlock()

	targets := cooldownProbeTargets(time.Now())
	if len(targets) != 1 {
		t.Fatalf("应恰好命中 1 个探测目标，got %d", len(targets))
	}
	if targets[0].model != failedModel {
		t.Fatalf("探测目标模型 = %q, want %q", targets[0].model, failedModel)
	}

	probeCoolingAccount(targets[0], &StoredAuth{})
	if gotModel != failedModel {
		t.Fatalf("探活实际使用的模型 = %q, want %q", gotModel, failedModel)
	}
}

// TestCooldownProbeSkipsModelScopeOnly 锁定「只探测账号级冷却」。
func TestCooldownProbeSkipsModelScopeOnly(t *testing.T) {
	const authID = "zcode-probe-model-scope"
	resetAccountFailover(authID)
	clearCooldownProbeState()
	t.Cleanup(func() {
		resetAccountFailover(authID)
		clearCooldownProbeState()
	})

	noteAccountFailureForModel(authID, "GLM-5.3", 429, "rate limit")

	if isAccountCoolingDown(authID) {
		t.Fatal("前置条件不成立：模型维度失败不应让账号整体冷却")
	}
	if targets := cooldownProbeTargets(time.Now()); len(targets) != 0 {
		t.Fatalf("模型维度冷却不应进入探活目标，got %d", len(targets))
	}
}

// TestCooldownProbeRunsOncePerCooldown 锁定每冷却周期最多探测一次。
func TestCooldownProbeRunsOncePerCooldown(t *testing.T) {
	const authID = "zcode-probe-once"
	resetAccountFailover(authID)
	clearCooldownProbeState()
	t.Cleanup(func() {
		resetAccountFailover(authID)
		clearCooldownProbeState()
	})

	noteAccountFailureForModel(authID, "", 503, "unavailable")
	until := time.Now().Add(3 * time.Second)

	failoverMu.Lock()
	if entry := failoverRecords[authID]; entry != nil {
		entry.cooldown = until
		entry.lastModel = "GLM-5.3"
	}
	failoverMu.Unlock()

	calls := stubProbePing(t, func(*StoredAuth, string) error { return nil })

	resolve := func(string) *StoredAuth { return &StoredAuth{} }

	now := time.Now()
	if probed := runCooldownProbeTick(now, resolve); probed != 1 {
		t.Fatalf("首轮应探测 1 个账号，got %d", probed)
	}
	if probed := runCooldownProbeTick(now, resolve); probed != 0 {
		t.Fatalf("同一冷却周期不应重复探测，got %d", probed)
	}
	if *calls != 1 {
		t.Fatalf("探活请求次数 = %d, want 1", *calls)
	}
}

// TestCooldownProbeSkipsUnresolvableAuth 锁定解析失败不标记已探测。
func TestCooldownProbeSkipsUnresolvableAuth(t *testing.T) {
	const authID = "zcode-probe-unresolvable"
	resetAccountFailover(authID)
	clearCooldownProbeState()
	t.Cleanup(func() {
		resetAccountFailover(authID)
		clearCooldownProbeState()
	})

	noteAccountFailureForModel(authID, "", 503, "unavailable")
	failoverMu.Lock()
	if entry := failoverRecords[authID]; entry != nil {
		entry.cooldown = time.Now().Add(2 * time.Second)
		entry.lastModel = "GLM-5.3"
	}
	failoverMu.Unlock()

	stubProbePing(t, func(*StoredAuth, string) error { return nil })

	if probed := runCooldownProbeTick(time.Now(), func(string) *StoredAuth { return nil }); probed != 0 {
		t.Fatalf("解析失败时不应探测，got %d", probed)
	}
	if probed := runCooldownProbeTick(time.Now(), func(string) *StoredAuth { return &StoredAuth{} }); probed != 1 {
		t.Fatalf("解析恢复后应探测 1 个账号，got %d", probed)
	}
}
