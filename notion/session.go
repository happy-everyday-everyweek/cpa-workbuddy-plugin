package main

// 本文件负责 thread 会话复用。
//
// 复用 thread 能显著降低上游开销：实测同一 threadId 续写时，
// 上游返回的 cachedTokensRead 会明显上升，且上下文自动延续。
// 但复用有两个风险，因此这里做了双向约束：
//  1. thread 属于账号，跨账号复用会被上游拒绝，键必须含账号；
//  2. thread 可能被上游回收，过期后必须回落为新建，避免整轮失败。

import (
	"strings"
	"sync"
	"time"
)

const (
	// sessionTTL 是 thread 的复用窗口。
	//
	// 上游线程在闲置过久后可能不可续写；两小时是保守取值，
	// 到期后自动改为新建 thread，行为退化但不报错。
	sessionTTL = 2 * time.Hour

	// maxSessionEntries 是会话表容量上限，防止长期运行后无限增长。
	maxSessionEntries = 2048
)

// sessionEntry 是一条 thread 复用记录。
type sessionEntry struct {
	// ThreadID 是上游线程 id。
	ThreadID string
	// UpdatedAt 是最近一次成功使用的时间。
	UpdatedAt time.Time
}

// sessionStore 是并发安全的会话表。
type sessionStore struct {
	mu      sync.RWMutex
	entries map[string]sessionEntry
}

var notionSessions = &sessionStore{entries: make(map[string]sessionEntry)}

// sessionKey 构造会话键。
//
// 必须同时包含账号与模型：同一账号在不同模型下的 thread 不通用，
// 跨账号的 thread 更是会被上游直接拒绝。
// [参数] auth: 账号；model: 宿主传入的模型名。
// [返回] 会话键。
// 最近修改时间：2026-10-18；改动原因：新增，会话隔离键。
func sessionKey(auth *StoredAuth, model string) string {
	return auth.AuthID + "|" + auth.SpaceID + "|" + normalizeNotionModel(model)
}

// lookupSession 查询可复用的 thread。
//
// 返回 followUp=true 表示命中未过期会话，调用方应构造续写请求。
// [参数] auth: 账号；model: 宿主传入的模型名。
// [返回] thread id 与是否可续写。
// 最近修改时间：2026-10-18；改动原因：新增，续写判定。
func lookupSession(auth *StoredAuth, model string) (string, bool) {
	key := sessionKey(auth, model)

	notionSessions.mu.RLock()
	entry, ok := notionSessions.entries[key]
	notionSessions.mu.RUnlock()

	if !ok || entry.ThreadID == "" {
		return "", false
	}
	if time.Since(entry.UpdatedAt) > sessionTTL {
		// 过期条目顺手清理，避免下次仍被读到。
		notionSessions.mu.Lock()
		delete(notionSessions.entries, key)
		notionSessions.mu.Unlock()
		return "", false
	}
	return entry.ThreadID, true
}

// rememberSession 记录 thread 以便后续续写。
// [参数] auth: 账号；model: 宿主传入的模型名；threadID: 上游线程 id。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，会话写入。
func rememberSession(auth *StoredAuth, model, threadID string) {
	if threadID == "" {
		return
	}
	key := sessionKey(auth, model)

	notionSessions.mu.Lock()
	defer notionSessions.mu.Unlock()

	if len(notionSessions.entries) >= maxSessionEntries {
		pruneSessionsLocked()
	}
	notionSessions.entries[key] = sessionEntry{ThreadID: threadID, UpdatedAt: time.Now()}
}

// forgetSession 删除指定会话。
//
// 续写失败时调用：上游可能已回收该线程，继续复用只会连续失败，
// 删除后下一轮会重新建 thread。
// [参数] auth: 账号；model: 宿主传入的模型名。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，失效会话清理。
func forgetSession(auth *StoredAuth, model string) {
	key := sessionKey(auth, model)
	notionSessions.mu.Lock()
	delete(notionSessions.entries, key)
	notionSessions.mu.Unlock()
}

// forgetSessionsForAuth 删除某账号在指定空间下的全部会话。
//
// 切换工作空间后旧 thread 属于旧空间，继续续写会落到错误空间；
// 这里按 `authID|spaceID|` 前缀整批清理，而不是逐模型枚举——
// 模型代号会随上游灰度变化，枚举必然漏。
// [参数] auth: 账号（按 auth.AuthID 与 auth.SpaceID 匹配）。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：修复切空间后旧会话残留。
func forgetSessionsForAuth(auth *StoredAuth) {
	if auth == nil {
		return
	}
	prefix := auth.AuthID + "|" + auth.SpaceID + "|"

	notionSessions.mu.Lock()
	defer notionSessions.mu.Unlock()
	for key := range notionSessions.entries {
		if strings.HasPrefix(key, prefix) {
			delete(notionSessions.entries, key)
		}
	}
}

// pruneSessionsLocked 清理过期条目；仍有空间时淘汰最旧的一半。
// [参数] 无（调用方必须已持有写锁）。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，容量控制。
func pruneSessionsLocked() {
	now := time.Now()
	for key, entry := range notionSessions.entries {
		if now.Sub(entry.UpdatedAt) > sessionTTL {
			delete(notionSessions.entries, key)
		}
	}
	if len(notionSessions.entries) < maxSessionEntries {
		return
	}

	// 仍然超限时按更新时间淘汰最旧的一半，保证内存有界。
	cutoff := now.Add(-sessionTTL / 2)
	for key, entry := range notionSessions.entries {
		if entry.UpdatedAt.Before(cutoff) {
			delete(notionSessions.entries, key)
		}
	}
}

// resetSessions 清空会话表（供测试使用）。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，测试隔离。
func resetSessions() {
	notionSessions.mu.Lock()
	notionSessions.entries = make(map[string]sessionEntry)
	notionSessions.mu.Unlock()
}
