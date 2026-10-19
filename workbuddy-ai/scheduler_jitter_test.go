// scheduler_jitter_test.go 覆盖槽位抖动：各插件槽位同刻对齐会形成整点尖峰，
// 抖动把全机唤醒摊平到数分钟窗口内。
//
// 变异对照：
//   - 把 nextCheckinFireTime 改成直接返回 nextCheckinTime(now)（去掉抖动）
//     → TestSchedulerJitter_Applied 的偏移断言 FAIL；
//   - 把抖动上界调成 >= 1 小时 → TestSchedulerJitter_NeverSkipsSlot FAIL；
//   - 让 jitter 无限大 → TestSchedulerJitter_StaysWithinMax FAIL。
package main

import (
	"testing"
	"time"
)

// withFixedJitter 把抖动源临时替换成固定值，用例结束后恢复。
// [参数] d：注入的抖动偏移
func withFixedJitter(t *testing.T, d time.Duration) {
	t.Helper()
	old := schedulerJitterFn
	schedulerJitterFn = func() time.Duration { return d }
	t.Cleanup(func() { schedulerJitterFn = old })
}

// TestSchedulerJitter_Applied 验证抖动真的加在槽位之上。
func TestSchedulerJitter_Applied(t *testing.T) {
	withFixedJitter(t, 90*time.Second)
	base := time.Date(2026, 10, 19, 7, 30, 0, 0, time.Local)

	slot := nextCheckinTime(base)
	fire := nextCheckinFireTime(base)

	// 槽位本身仍是绝对整点（纯函数不受抖动污染）。
	if slot.Hour() != 8 || slot.Minute() != 0 {
		t.Fatalf("nextCheckinTime 应保持整点槽位，got %v", slot)
	}
	wantFire := slot.Add(90 * time.Second)
	if !fire.Equal(wantFire) {
		t.Fatalf("唤醒时刻 = %v, want %v（槽位 + 抖动）", fire, wantFire)
	}
}

// TestSchedulerJitter_ZeroInjectsNoDelay 验证抖动为 0 时退化为原槽位。
func TestSchedulerJitter_ZeroInjectsNoDelay(t *testing.T) {
	withFixedJitter(t, 0)
	base := time.Date(2026, 10, 19, 7, 30, 0, 0, time.Local)

	if got, want := nextCheckinFireTime(base), nextCheckinTime(base); !got.Equal(want) {
		t.Fatalf("抖动为 0 时应等于原槽位：got %v, want %v", got, want)
	}
}

// TestSchedulerJitter_StaysWithinMax 验证真实抖动源始终落在 [0, max) 内。
//
// 抖动一旦超过槽位间隔，就可能跨过下一个槽位，导致某个槽位被整轮跳过。
func TestSchedulerJitter_StaysWithinMax(t *testing.T) {
	for i := 0; i < 200; i++ {
		d := schedulerJitterFn()
		if d < 0 || d >= schedulerJitterMax {
			t.Fatalf("第 %d 次抖动 = %v，必须落在 [0, %v) 内", i, d, schedulerJitterMax)
		}
	}
}

// TestSchedulerJitter_NeverSkipsSlot 验证抖动不会跳过任何槽位。
//
// 判据：在某个槽位之后取参考时刻，唤醒时刻必须仍落在该槽位与下一个槽位之间。
func TestSchedulerJitter_NeverSkipsSlot(t *testing.T) {
	for i := 0; i < 100; i++ {
		now := time.Date(2026, 10, 19, 0, 0, 1, 0, time.Local) // 00:00:01
		fire := nextCheckinFireTime(now)
		slot := time.Date(2026, 10, 19, 4, 0, 0, 0, time.Local)          // 最近槽位
		followingSlot := time.Date(2026, 10, 19, 8, 0, 0, 0, time.Local) // 再下一个槽位
		if !fire.After(now) {
			t.Fatalf("第 %d 次：唤醒时刻 %v 必须晚于 now %v", i, fire, now)
		}
		// 抖动只会把本轮推迟，绝不能推迟到「跳过本轮、直接命中下一轮」。
		if !fire.Before(followingSlot) {
			t.Fatalf("第 %d 次：唤醒时刻 %v 越过下一轮槽位 %v（抖动过大）", i, fire, followingSlot)
		}
		if fire.Before(slot) {
			t.Fatalf("第 %d 次：唤醒时刻 %v 早于本轮槽位 %v", i, fire, slot)
		}
	}
}

// TestSchedulerJitter_GuardAgainstPast 验证兜底分支：抖动为负不会把时刻推到过去。
func TestSchedulerJitter_GuardAgainstPast(t *testing.T) {
	withFixedJitter(t, -3*time.Hour)
	now := time.Date(2026, 10, 19, 7, 59, 59, 0, time.Local)

	fire := nextCheckinFireTime(now)
	if !fire.After(now) {
		t.Fatalf("负抖动兜底后唤醒时刻 %v 必须晚于 now %v", fire, now)
	}
}
