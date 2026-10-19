package main

import (
	"testing"
	"time"
)

// TestFailoverCooldownFor_Ladder 锁定阶梯时长契约：count 每增 1 冷却加一个基数，
// 到 failoverCooldownLadderCap 后恒为上限。
// 最近修改时间：2026-10-19；改动原因：基数由 30s 调整为 60s（仍为 base × count 线性阶梯、上限 10 分钟）。
func TestFailoverCooldownFor_Ladder(t *testing.T) {
	cases := []struct {
		count int
		want  time.Duration
	}{
		{0, 0},
		{-1, 0},
		{1, 60 * time.Second},
		{2, 120 * time.Second},
		{3, 180 * time.Second},
		{4, 240 * time.Second},
		{9, 540 * time.Second},
		{10, 10 * time.Minute},
		{11, 10 * time.Minute},
		{99, 10 * time.Minute},
	}
	for _, tc := range cases {
		if got := failoverCooldownFor(tc.count); got != tc.want {
			t.Fatalf("failoverCooldownFor(%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
	if failoverCooldownLadderCap != 10 {
		t.Fatalf("failoverCooldownLadderCap = %d, want 10", failoverCooldownLadderCap)
	}
}

// TestNoteAccountFailure_LadderCooldown 锁定连续失败逐次递增：
// 60s → 120s → 180s，成功后清零回落。
func TestNoteAccountFailure_LadderCooldown(t *testing.T) {
	const authID = "copilot-ladder"
	resetAccountFailover(authID)
	t.Cleanup(func() { resetAccountFailover(authID) })

	wants := []time.Duration{60 * time.Second, 120 * time.Second, 180 * time.Second}
	for i, want := range wants {
		noteAccountFailure(authID, 429, "rate limit")
		failoverMu.RLock()
		entry := failoverRecords[authID]
		remain := time.Until(entry.cooldown)
		count := entry.failCount
		failoverMu.RUnlock()
		if count != i+1 {
			t.Fatalf("iteration %d: failCount = %d, want %d", i, count, i+1)
		}
		if remain <= want-5*time.Second || remain > want+5*time.Second {
			t.Fatalf("iteration %d: cooldown remain = %v, want ~%v", i, remain, want)
		}
	}
	// 成功后清零，下一次失败回到 60s。
	resetAccountFailover(authID)
	noteAccountFailure(authID, 429, "rate limit")
	failoverMu.RLock()
	remain := time.Until(failoverRecords[authID].cooldown)
	failoverMu.RUnlock()
	if remain <= 55*time.Second || remain > 65*time.Second {
		t.Fatalf("after reset: cooldown remain = %v, want ~60s", remain)
	}
}
