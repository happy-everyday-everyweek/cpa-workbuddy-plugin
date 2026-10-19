// checkin_retry_concurrency_test.go 覆盖签到重试的并发上限。
//
// 背景：原实现给每个到期账号直接 `go processCheckinRetry(e)`，账号规模上来
// 后一次 tick 可能瞬时打出 N 个上游请求，是全仓唯一无上限的定时并发点。
//
// 变异对照：
//   - 把 runCheckinRetryRound 的信号量去掉（每项直接 go）→
//     TestCheckinRetryRound_CapsConcurrency 的峰值断言 FAIL；
//   - 把 checkinRetryConcurrency 调大 → 上限断言 FAIL。
package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCheckinRetryRound_CapsConcurrency 验证单轮真实分派并发不超过上限。
//
// 通过 processCheckinRetryFn 注入阻塞式假处理卡住所有 worker，测量同时在跑
// 的峰值数量：若信号量缺失，峰值会等于到期条目数。
func TestCheckinRetryRound_CapsConcurrency(t *testing.T) {
	const total = 20 // 远大于并发上限，确保能观测到上限

	// 准备 total 个「已到期」条目。
	checkinRetryMu.Lock()
	checkinRetryQueue = make(map[string]*checkinRetryEntry, total)
	for i := 0; i < total; i++ {
		idx := "retry-cap-" + string(rune('a'+i))
		checkinRetryQueue[idx] = &checkinRetryEntry{
			AuthIndex: idx,
			AuthID:    idx,
			NextAt:    time.Now().Add(-time.Minute),
		}
	}
	checkinRetryMu.Unlock()
	t.Cleanup(func() {
		checkinRetryMu.Lock()
		checkinRetryQueue = make(map[string]*checkinRetryEntry)
		checkinRetryMu.Unlock()
	})

	var (
		inFlight atomic.Int64
		peak     atomic.Int64
		done     sync.WaitGroup
	)
	done.Add(total)
	release := make(chan struct{})

	oldFn := processCheckinRetryFn
	processCheckinRetryFn = func(*checkinRetryEntry) {
		defer done.Done()
		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
	}
	t.Cleanup(func() { processCheckinRetryFn = oldFn })

	roundDone := make(chan struct{})
	go func() {
		runCheckinRetryRound()
		close(roundDone)
	}()

	// 等上限内的 worker 全部就位，再检查峰值。
	deadline := time.Now().Add(2 * time.Second)
	for peak.Load() < int64(checkinRetryConcurrency) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := peak.Load(); got > int64(checkinRetryConcurrency) {
		t.Fatalf("峰值并发 = %d，超过上限 %d（旧缺陷：每项直接 go 无上限）", got, checkinRetryConcurrency)
	}
	if got := peak.Load(); got != int64(checkinRetryConcurrency) {
		t.Fatalf("峰值并发 = %d，未达到上限 %d（信号量未生效）", got, checkinRetryConcurrency)
	}

	close(release)
	select {
	case <-roundDone:
	case <-time.After(5 * time.Second):
		t.Fatal("runCheckinRetryRound 未在本轮全部完成后返回")
	}
}

// TestCheckinRetryRound_WaitsForAllEntries 验证本轮会等全部条目处理完才返回，
// 否则被 pop 但未处理的条目会短暂悬空。
func TestCheckinRetryRound_WaitsForAllEntries(t *testing.T) {
	const total = 6
	checkinRetryMu.Lock()
	checkinRetryQueue = make(map[string]*checkinRetryEntry, total)
	for i := 0; i < total; i++ {
		idx := "retry-wait-" + string(rune('a'+i))
		checkinRetryQueue[idx] = &checkinRetryEntry{
			AuthIndex: idx,
			AuthID:    idx,
			NextAt:    time.Now().Add(-time.Minute),
		}
	}
	checkinRetryMu.Unlock()
	t.Cleanup(func() {
		checkinRetryMu.Lock()
		checkinRetryQueue = make(map[string]*checkinRetryEntry)
		checkinRetryMu.Unlock()
	})

	var processed atomic.Int64
	oldFn := processCheckinRetryFn
	processCheckinRetryFn = func(*checkinRetryEntry) {
		time.Sleep(10 * time.Millisecond)
		processed.Add(1)
	}
	t.Cleanup(func() { processCheckinRetryFn = oldFn })

	runCheckinRetryRound()
	if got := processed.Load(); got != total {
		t.Fatalf("本轮应处理全部 %d 条目，got %d", total, got)
	}
}

// TestCheckinRetryConcurrency_Sane 护栏：上限必须为正且不高于签到主链路。
func TestCheckinRetryConcurrency_Sane(t *testing.T) {
	if checkinRetryConcurrency <= 0 {
		t.Fatalf("checkinRetryConcurrency = %d，必须 > 0", checkinRetryConcurrency)
	}
	if checkinRetryConcurrency > 8 {
		t.Fatalf("checkinRetryConcurrency = %d，重试不应比正常签到更激进", checkinRetryConcurrency)
	}
}
