package main

import (
	"fmt"
	"testing"
	"time"
)

// TestCooldownProbeRetriesAfterEscalation 验证「探活失败 → 冷却延长 → 新周期仍会探测」。
//
// 这是防自激与防漏探的交界：探活失败后冷却沿阶梯延长，下一次探测必须跟随新的
// 到期时间重新排期，而不是被上一周期的探测记录永久挡住。若此逻辑写错，
// 探活失败一次就会让该账号在整个后续冷却中再也不会被探测。
func TestCooldownProbeRetriesAfterEscalation(t *testing.T) {
	resetFailover(t)
	clearCooldownProbeState()
	t.Cleanup(clearCooldownProbeState)

	const authID = "auth-probe-escalate"
	const model = "glm-4.6"

	// 首次失败：构造一个 3s 后到期的冷却（落在探测窗口内）。
	recordAccountFailure(authID, 503, "unavailable")
	failoverMu.Lock()
	for _, k := range failoverKeysLocked(authID) {
		if st := failoverStates[k]; st != nil {
			st.cooldownUntil = time.Now().Add(3 * time.Second)
			st.lastModel = model
		}
	}
	failoverMu.Unlock()

	calls := 0
	old := probePingFn
	probePingFn = func(*storedAuth, string) error {
		calls++
		return fmt.Errorf("upstream HTTP 500: still broken")
	}
	t.Cleanup(func() { probePingFn = old })

	resolve := func(string) *storedAuth { return &storedAuth{} }

	// 1. 首次探测：命中并失败，冷却被延长。
	if probed := runCooldownProbeTick(time.Now(), resolve); probed != 1 {
		t.Fatalf("首轮应探测 1 个账号，got %d", probed)
	}
	_, escalatedUntil, ok := failoverStateSnapshot(authID)
	if !ok || !escalatedUntil.After(time.Now().Add(3*time.Second)) {
		t.Fatalf("失败后冷却应被延长，got %v", escalatedUntil)
	}
	// 精确锁定「先探测、后标记」：记录的周期标识必须是延长后的新截止时间。
	// 若改回「先标记」，这里记录的是延长前的旧值，下一轮会被误判为已探测。
	probedUntilMu.Lock()
	recorded := probedUntil[authID]
	probedUntilMu.Unlock()
	if !recorded.Equal(escalatedUntil) {
		t.Fatalf("探测周期标识 = %v, want 延长后的 %v（先标记后探测会记录旧值）", recorded, escalatedUntil)
	}

	// 2. 冷却延长后立刻再跑：新到期时间尚未进入提前量窗口，不应探测。
	if probed := runCooldownProbeTick(time.Now(), resolve); probed != 0 {
		t.Fatalf("延长后的冷却尚未进入探测窗口，不应探测，got %d", probed)
	}

	// 3. 把冷却改到新周期的探测窗口内：必须能再次探测，不能被旧记录挡住。
	now := time.Now()
	failoverMu.Lock()
	for _, k := range failoverKeysLocked(authID) {
		if st := failoverStates[k]; st != nil {
			st.cooldownUntil = now.Add(3 * time.Second)
		}
	}
	failoverMu.Unlock()

	if probed := runCooldownProbeTick(now, resolve); probed != 1 {
		t.Fatalf("冷却延长后的新周期应重新探测，got %d", probed)
	}
	if calls != 2 {
		t.Fatalf("探活请求次数 = %d, want 2", calls)
	}
}
