package main

import (
	"testing"
	"time"
)

// storeCredits seeds accountCache with a credits entry for one auth ID and
// auto-clears it on test end. traework's credits snapshot is traeCredits.
func storeCredits(t *testing.T, id string, remain int64) {
	t.Helper()
	accountCache.Store(id, &accountCacheEntry{credits: &traeCredits{
		TotalRemain: remain,
		FetchedAt:   time.Now().Format(time.RFC3339),
	}})
	t.Cleanup(func() { accountCache.Delete(id) })
}

// resetActiveAuth resets the panel selection and pins scheduler mode for the
// duration of a routing test (traework default mode is off).
func resetActiveAuth(t *testing.T) {
	t.Helper()
	setActiveAuthID("")
	restoreMode := setSchedulerMode(schedulerModeCredits)
	t.Cleanup(func() {
		setActiveAuthID("")
		restoreMode()
	})
}

// TestWatchdogIntervalDefault pins the fixed cadence: 保号 (preserve) 池已于
// 2026-09-29 移除，watchdog 不再接受 interval/enabled 配置，只剩「刷新积分 +
// 定时活跃测试」两项职责。
// TestWatchdogFirstTickDelay 验证首轮刷新前的抖动来自 schedulerJitterFn。
//
// 六个插件的 watchdog 都在宿主就绪后立刻跑首轮，就绪轮询会把它们吸附到同一
// 窗口。本用例确认抖动确实接在这条路径上，而不是只存在于 4h 槽位。
func TestWatchdogFirstTickDelay(t *testing.T) {
	withFixedJitter(t, 90*time.Second)
	if got := watchdogFirstTickDelay(); got != 90*time.Second {
		t.Fatalf("首轮抖动 = %v, want 90s", got)
	}
}

// TestWatchdogFirstTickDelay_UsesSharedBound 验证首轮抖动复用槽位抖动的上界。
//
// 独立上界会让两条路径的错峰幅度失去同步依据；共用同一常量也保证
// 上界收紧时首轮一起收紧。
func TestWatchdogFirstTickDelay_UsesSharedBound(t *testing.T) {
	for i := 0; i < 200; i++ {
		d := watchdogFirstTickDelay()
		if d < 0 || d >= schedulerJitterMax {
			t.Fatalf("第 %d 次首轮抖动 = %v，必须落在 [0, %v) 内", i, d, schedulerJitterMax)
		}
	}
}

func TestWatchdogIntervalDefault(t *testing.T) {
	if watchdogIntervalDefault != 10*time.Minute {
		t.Fatalf("watchdogIntervalDefault = %v, want 10m", watchdogIntervalDefault)
	}
}

// TestWaitHostReadyForWatchdog covers the startup-readiness probe used to
// close the init-race blind window.
func TestWaitHostReadyForWatchdog(t *testing.T) {
	// Case 1: always-ready, short maxWait — instant true, zero sleeps.
	calls := 0
	ready := func() bool { calls++; return true }
	if !waitHostReadyForWatchdog(50*time.Millisecond, ready) {
		t.Fatal("always-ready should return true")
	}
	if calls != 1 {
		t.Fatalf("expected one ready() call, got %d", calls)
	}

	// Case 2: becomes-ready on the second call.
	calls = 0
	ready = func() bool { calls++; return calls >= 2 }
	if !waitHostReadyForWatchdog(2*time.Second, ready) {
		t.Fatal("eventually-ready should return true")
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 probes, got %d", calls)
	}

	// Case 3: never-ready, maxWait=0 — returns whatever ready() says.
	calls = 0
	ready = func() bool { calls++; return false }
	if waitHostReadyForWatchdog(0, ready) {
		t.Fatal("maxWait=0 + never-ready must return false")
	}
	if calls != 1 {
		t.Fatalf("maxWait=0 must probe exactly once, got %d", calls)
	}

	// Case 4: never-ready with a tight maxWait — returns false once the
	// deadline passes.
	calls = 0
	ready = func() bool { calls++; return false }
	start := time.Now()
	if waitHostReadyForWatchdog(20*time.Millisecond, ready) {
		t.Fatal("never-ready under a finite maxWait must return false")
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("wait overshot the deadline: %v", elapsed)
	}
	if calls < 1 {
		t.Fatalf("never-ready should be probed at least once, got %d", calls)
	}
}

// TestRequestWatchdogTickCoalesces proves the trigger channel collapses
// concurrent requesters onto a single queued tick.
func TestRequestWatchdogTickCoalesces(t *testing.T) {
	// Drain anything queued by a previous test (the chan is package-global).
	for {
		select {
		case <-watchdogTickCh:
		default:
			goto drained
		}
	}
drained:
	requestWatchdogTick()
	requestWatchdogTick()
	requestWatchdogTick()
	select {
	case <-watchdogTickCh:
		// OK — exactly one queued.
	default:
		t.Fatal("expected at least one tick queued after coalesced requests")
	}
	select {
	case <-watchdogTickCh:
		t.Fatal("requestWatchdogTick must coalesce; got a second queued value")
	default:
	}
}
