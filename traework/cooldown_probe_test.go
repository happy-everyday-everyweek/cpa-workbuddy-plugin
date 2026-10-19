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
// 最近修改时间：2026-10-19 01:20:00；改动原因：新增。
func stubProbePing(t *testing.T, fn func(*traeAuth, string) error) *int {
	t.Helper()
	calls := 0
	old := probePingFn
	probePingFn = func(sa *traeAuth, model string) error {
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
	const authID = "auth-window"
	const model = "Doubao-Seed-2.1-Pro"

	cases := []struct {
		name          string
		cooldownUntil time.Time
		model         string
		probedAt      time.Time
		want          bool
	}{
		{
			name:          "距到期 10s 太早",
			cooldownUntil: now.Add(10 * time.Second),
			model:         model,
			want:          false,
		},
		{
			name:          "距到期 6s 仍未进窗口",
			cooldownUntil: now.Add(6 * time.Second),
			model:         model,
			want:          false,
		},
		{
			name:          "距到期正好 5s 进窗口",
			cooldownUntil: now.Add(5 * time.Second),
			model:         model,
			want:          true,
		},
		{
			name:          "距到期 1s 仍可探测",
			cooldownUntil: now.Add(1 * time.Second),
			model:         model,
			want:          true,
		},
		{
			name:          "冷却已过期不探测",
			cooldownUntil: now.Add(-time.Second),
			model:         model,
			want:          false,
		},
		{
			name:          "无模型不探测",
			cooldownUntil: now.Add(2 * time.Second),
			model:         "",
			want:          false,
		},
		{
			name:          "无冷却不探测",
			cooldownUntil: time.Time{},
			model:         model,
			want:          false,
		},
		{
			name:          "本周期已探测不再重复",
			cooldownUntil: now.Add(3 * time.Second),
			model:         model,
			// probedAt 记录的是已探测的冷却截止时间，与当前相同即同一周期。
			probedAt: now.Add(3 * time.Second),
			want:     false,
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

// TestCooldownProbeSuccessClearsCooldown 锁定成功路径：探活成功立即清除冷却，
// 账号可以马上重新承载流量。
func TestCooldownProbeSuccessClearsCooldown(t *testing.T) {
	resetFailover(t)
	clearCooldownProbeState()
	t.Cleanup(clearCooldownProbeState)

	const authID = "auth-probe-ok"
	recordAccountFailure(authID, 503, "service unavailable")
	if !isAccountCoolingDown(authID) {
		t.Fatal("前置条件不成立：应处于冷却")
	}

	stubProbePing(t, func(*traeAuth, string) error { return nil })

	candidate := cooldownProbeCandidate{authID: authID, cooldownUntil: time.Now().Add(2 * time.Second), model: "GLM-4.6"}
	if !probeCoolingAccount(candidate, &traeAuth{}) {
		t.Fatal("探活成功应返回 true")
	}
	if isAccountCoolingDown(authID) {
		t.Fatal("探活成功后应立即解除冷却")
	}
}

// TestCooldownProbeFailureEscalatesCooldown 锁定失败路径：探活失败继续沿阶梯递增，
// 而不是清除冷却或原地不动。
func TestCooldownProbeFailureEscalatesCooldown(t *testing.T) {
	resetFailover(t)
	clearCooldownProbeState()
	t.Cleanup(clearCooldownProbeState)

	const authID = "auth-probe-fail"
	recordAccountFailure(authID, 503, "service unavailable")
	_, before, _ := failoverStateSnapshot(authID)
	beforeCount, _, _ := failoverStateSnapshot(authID)

	stubProbePing(t, func(*traeAuth, string) error { return fmt.Errorf("upstream HTTP 503: unavailable") })

	candidate := cooldownProbeCandidate{authID: authID, cooldownUntil: before, model: "GLM-4.6"}
	if probeCoolingAccount(candidate, &traeAuth{}) {
		t.Fatal("探活失败应返回 false")
	}

	count, after, ok := failoverStateSnapshot(authID)
	if !ok {
		t.Fatal("探活失败后冷却状态应存在")
	}
	if count != beforeCount+1 {
		t.Fatalf("失败后计数 = %d, want %d（应沿阶梯递增）", count, beforeCount+1)
	}
	if !after.After(before) {
		t.Fatalf("失败后到期时间应延后: before=%v after=%v", before, after)
	}
	if !isAccountCoolingDown(authID) {
		t.Fatal("探活失败后账号应继续处于冷却")
	}
}

// TestCooldownProbeNeverWritesTestFailed 是本模块最重要的用例：
// 无论探活成功还是失败，都不得写 test_failed 标签——该标签是硬排除、
// 永不自动过期，写进去比冷却严重得多。
func TestCooldownProbeNeverWritesTestFailed(t *testing.T) {
	resetFailover(t)
	clearCooldownProbeState()
	resetTestFailed(t)
	t.Cleanup(clearCooldownProbeState)

	const okAuth = "auth-probe-tag-ok"
	const failAuth = "auth-probe-tag-fail"

	recordAccountFailure(okAuth, 503, "unavailable")
	recordAccountFailure(failAuth, 503, "unavailable")

	stubProbePing(t, func(sa *traeAuth, model string) error {
		if model == "fail-model" {
			return fmt.Errorf("upstream HTTP 500: boom")
		}
		return nil
	})

	probeCoolingAccount(cooldownProbeCandidate{authID: okAuth, cooldownUntil: time.Now().Add(time.Second), model: "ok-model"}, &traeAuth{})
	probeCoolingAccount(cooldownProbeCandidate{authID: failAuth, cooldownUntil: time.Now().Add(time.Second), model: "fail-model"}, &traeAuth{})

	if isTestFailed(okAuth) {
		t.Fatal("探活成功不得写 test_failed")
	}
	if isTestFailed(failAuth) {
		t.Fatal("探活失败不得写 test_failed——该标签永不自动过期，比冷却严重得多")
	}
}

// TestCooldownProbeUsesFailedModel 锁定「必须用当初失败的模型回放」：
// 探活请求收到的模型名必须等于 lastModel，不能用任意模型代替。
func TestCooldownProbeUsesFailedModel(t *testing.T) {
	resetFailover(t)
	clearCooldownProbeState()
	t.Cleanup(clearCooldownProbeState)

	const authID = "auth-probe-model"
	const failedModel = "Deepseek-V4.1-Flash"

	recordAccountFailureForModel(authID, failedModel, 503, "unavailable")

	var gotModel string
	stubProbePing(t, func(_ *traeAuth, model string) error {
		gotModel = model
		return nil
	})

	// 把冷却改到探测窗口内（2s 后到期），再取目标。
	failoverMu.Lock()
	for _, k := range failoverKeysLocked(authID) {
		if st := failoverStates[k]; st != nil {
			st.cooldownUntil = time.Now().Add(2 * time.Second)
		}
	}
	failoverMu.Unlock()

	targets := cooldownProbeTargets(time.Now())
	if len(targets) != 1 {
		t.Fatalf("应恰好命中 1 个探测目标，got %d", len(targets))
	}
	if targets[0].model != failedModel {
		t.Fatalf("探测目标模型 = %q, want %q", targets[0].model, failedModel)
	}

	probeCoolingAccount(targets[0], &traeAuth{})
	if gotModel != failedModel {
		t.Fatalf("探活实际使用的模型 = %q, want %q", gotModel, failedModel)
	}
}

// TestCooldownProbeSkipsModelScopeOnly 锁定「只探测账号级冷却」：
// 仅模型维度冷却的账号（账号层不在冷却）不应被探活——模型级失败重试无法自愈。
func TestCooldownProbeSkipsModelScopeOnly(t *testing.T) {
	resetFailover(t)
	clearCooldownProbeState()
	t.Cleanup(clearCooldownProbeState)

	const authID = "auth-probe-model-scope"
	recordAccountFailureForModel(authID, "GLM-4.6", 429, "rate limit")

	if isAccountCoolingDown(authID) {
		t.Fatal("前置条件不成立：模型维度失败不应让账号整体冷却")
	}
	if targets := cooldownProbeTargets(time.Now()); len(targets) != 0 {
		t.Fatalf("模型维度冷却不应进入探活目标，got %d", len(targets))
	}
}

// TestCooldownProbeRunsOncePerCooldown 锁定每冷却周期最多探测一次，
// 防止探测失败 → 冷却递增 → 再探测 的高频自激。
func TestCooldownProbeRunsOncePerCooldown(t *testing.T) {
	resetFailover(t)
	clearCooldownProbeState()
	t.Cleanup(clearCooldownProbeState)

	const authID = "auth-probe-once"
	recordAccountFailure(authID, 503, "unavailable")
	until := time.Now().Add(3 * time.Second)

	failoverMu.Lock()
	for _, k := range failoverKeysLocked(authID) {
		if st := failoverStates[k]; st != nil {
			st.cooldownUntil = until
			st.lastModel = "GLM-4.6"
		}
	}
	failoverMu.Unlock()

	calls := stubProbePing(t, func(*traeAuth, string) error { return nil })

	resolve := func(string) *traeAuth { return &traeAuth{} }

	now := time.Now()
	if probed := runCooldownProbeTick(now, resolve); probed != 1 {
		t.Fatalf("首轮应探测 1 个账号，got %d", probed)
	}
	// 紧接着再跑一轮：同一冷却周期内不应重复探测。
	if probed := runCooldownProbeTick(now, resolve); probed != 0 {
		t.Fatalf("同一冷却周期不应重复探测，got %d", probed)
	}
	if *calls != 1 {
		t.Fatalf("探活请求次数 = %d, want 1", *calls)
	}
}

// TestCooldownProbeSkipsUnresolvableAuth 锁定解析失败不标记已探测：
// 宿主 RPC 抖动后下一轮仍会重试，而不是整轮错过。
func TestCooldownProbeSkipsUnresolvableAuth(t *testing.T) {
	resetFailover(t)
	clearCooldownProbeState()
	t.Cleanup(clearCooldownProbeState)

	const authID = "auth-probe-unresolvable"
	recordAccountFailure(authID, 503, "unavailable")
	failoverMu.Lock()
	for _, k := range failoverKeysLocked(authID) {
		if st := failoverStates[k]; st != nil {
			st.cooldownUntil = time.Now().Add(2 * time.Second)
			st.lastModel = "GLM-4.6"
		}
	}
	failoverMu.Unlock()

	stubProbePing(t, func(*traeAuth, string) error { return nil })

	// 第一轮解析失败：不探测，也不消耗本周期的探测配额。
	if probed := runCooldownProbeTick(time.Now(), func(string) *traeAuth { return nil }); probed != 0 {
		t.Fatalf("解析失败时不应探测，got %d", probed)
	}
	// 第二轮解析恢复：应能正常探测。
	if probed := runCooldownProbeTick(time.Now(), func(string) *traeAuth { return &traeAuth{} }); probed != 1 {
		t.Fatalf("解析恢复后应探测 1 个账号，got %d", probed)
	}
}
