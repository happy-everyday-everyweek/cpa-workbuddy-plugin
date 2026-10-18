package main

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestSessionKeyIsolatesByAuthAndModel 校验会话键隔离维度。
//
// thread 属于账号与模型：跨账号复用会被上游拒绝，跨模型复用会串上下文。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，会话隔离回归。
func TestSessionKeyIsolatesByAuthAndModel(t *testing.T) {
	base := &StoredAuth{AuthID: "notion-aaa", SpaceID: "space-1"}

	same := &StoredAuth{AuthID: "notion-aaa", SpaceID: "space-1"}
	if sessionKey(base, "notion/omodi") != sessionKey(same, "omodi") {
		t.Fatal("同一账号同一模型的键应一致（含 notion/ 前缀归一）")
	}

	otherAuth := &StoredAuth{AuthID: "notion-bbb", SpaceID: "space-1"}
	if sessionKey(base, "omodi") == sessionKey(otherAuth, "omodi") {
		t.Fatal("不同账号不应共用会话键")
	}

	otherSpace := &StoredAuth{AuthID: "notion-aaa", SpaceID: "space-2"}
	if sessionKey(base, "omodi") == sessionKey(otherSpace, "omodi") {
		t.Fatal("同账号不同空间不应共用会话键")
	}

	if sessionKey(base, "omodi") == sessionKey(base, "orchid-muffin") {
		t.Fatal("不同模型不应共用会话键")
	}
}

// TestSessionLifecycle 校验写入、命中与失效删除。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，会话生命周期回归。
func TestSessionLifecycle(t *testing.T) {
	resetSessions()
	auth := &StoredAuth{AuthID: "notion-aaa", SpaceID: "space-1"}

	if _, ok := lookupSession(auth, "omodi"); ok {
		t.Fatal("空会话表不应命中")
	}

	rememberSession(auth, "omodi", "thread-1")
	threadID, ok := lookupSession(auth, "omodi")
	if !ok || threadID != "thread-1" {
		t.Fatalf("会话未命中：ok=%v thread=%q", ok, threadID)
	}

	// 空 thread id 不应写入，否则下一轮会拿空 id 去续写。
	rememberSession(auth, "orchid-muffin", "")
	if _, ok := lookupSession(auth, "orchid-muffin"); ok {
		t.Fatal("空 thread id 不应被记录")
	}

	forgetSession(auth, "omodi")
	if _, ok := lookupSession(auth, "omodi"); ok {
		t.Fatal("删除后不应再命中")
	}
}

// TestSessionExpiresAfterTTL 校验过期会话自动失效。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，TTL 回归。
func TestSessionExpiresAfterTTL(t *testing.T) {
	resetSessions()
	auth := &StoredAuth{AuthID: "notion-aaa", SpaceID: "space-1"}
	key := sessionKey(auth, "omodi")

	notionSessions.mu.Lock()
	notionSessions.entries[key] = sessionEntry{
		ThreadID:  "thread-old",
		UpdatedAt: time.Now().Add(-sessionTTL - time.Minute),
	}
	notionSessions.mu.Unlock()

	if _, ok := lookupSession(auth, "omodi"); ok {
		t.Fatal("过期会话不应命中")
	}

	notionSessions.mu.RLock()
	_, stillThere := notionSessions.entries[key]
	notionSessions.mu.RUnlock()
	if stillThere {
		t.Fatal("过期条目应被顺手清理")
	}
}

// TestSessionPrunesWhenFull 校验容量上限触发淘汰。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，容量控制回归。
func TestSessionPrunesWhenFull(t *testing.T) {
	resetSessions()

	notionSessions.mu.Lock()
	for i := 0; i < maxSessionEntries; i++ {
		notionSessions.entries["filler-"+strconv.Itoa(i)] = sessionEntry{
			ThreadID:  "t",
			UpdatedAt: time.Now().Add(-sessionTTL - time.Minute),
		}
	}
	notionSessions.mu.Unlock()

	auth := &StoredAuth{AuthID: "notion-new", SpaceID: "space-1"}
	rememberSession(auth, "omodi", "thread-new")

	notionSessions.mu.RLock()
	size := len(notionSessions.entries)
	notionSessions.mu.RUnlock()
	if size > maxSessionEntries {
		t.Fatalf("会话表超出上限：%d > %d", size, maxSessionEntries)
	}
	if _, ok := lookupSession(auth, "omodi"); !ok {
		t.Fatal("新增会话应可命中")
	}
}

// TestForgetSessionsForAuthClearsWholeSpace 校验切空间时的整批清理。
//
// 只按模型名逐条删除会漏掉上游新增的模型代号；这里要求按
// `authID|spaceID|` 前缀整批清理，且不误删其它空间与账号。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，切空间清理回归。
func TestForgetSessionsForAuthClearsWholeSpace(t *testing.T) {
	resetSessions()
	auth := &StoredAuth{AuthID: "notion-aaa", SpaceID: "space-1"}
	otherSpace := &StoredAuth{AuthID: "notion-aaa", SpaceID: "space-2"}
	otherAuth := &StoredAuth{AuthID: "notion-bbb", SpaceID: "space-1"}

	for _, model := range []string{"omodi", "orchid-muffin", "brand-new-code"} {
		rememberSession(auth, model, "thread-"+model)
	}
	rememberSession(otherSpace, "omodi", "thread-s2")
	rememberSession(otherAuth, "omodi", "thread-bbb")

	forgetSessionsForAuth(auth)

	for _, model := range []string{"omodi", "orchid-muffin", "brand-new-code"} {
		if _, ok := lookupSession(auth, model); ok {
			t.Fatalf("空间内会话 %s 未被清理", model)
		}
	}
	if _, ok := lookupSession(otherSpace, "omodi"); !ok {
		t.Fatal("其它空间的会话被误删")
	}
	if _, ok := lookupSession(otherAuth, "omodi"); !ok {
		t.Fatal("其它账号的会话被误删")
	}
}

// TestSessionConcurrentAccess 校验并发读写不竞态。
//
// 用 -race 跑时才有意义，但即便不开 race，也能验证逻辑不会互相破坏。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，并发安全回归。
func TestSessionConcurrentAccess(t *testing.T) {
	resetSessions()
	auth := &StoredAuth{AuthID: "notion-aaa", SpaceID: "space-1"}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			model := "model-" + strconv.Itoa(idx%4)
			for j := 0; j < 50; j++ {
				rememberSession(auth, model, "thread-"+strconv.Itoa(idx))
				_, _ = lookupSession(auth, model)
				forgetSession(auth, model)
			}
		}(i)
	}
	wg.Wait()
}
