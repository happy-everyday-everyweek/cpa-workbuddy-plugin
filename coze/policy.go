package main

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// 本文件定义「积分路由红线」策略。
//
// 扣子不声明 scheduler 能力：宿主只调用排序后的第一个 scheduler 插件
// （internal/pluginhost/scheduler.go 的 schedulerRecord 只取一条记录），
// 抢这个槽位会顶掉现有插件的调度内核。因此本插件走执行入口闸门——
// 宿主选到低额账号时，在执行前直接回 403，宿主按既有账号级故障通道
// 换号。语义与上游 1000521「积分耗尽」完全一致，只是提前到不打上游。
//
// 阈值取 100 而不是兄弟插件的 20：扣子单次对话消耗远高于按次计费的
// 套餐类产品，20 分只够一两次请求，起不到缓冲作用。

const (
	// defaultMinRoutableCredits 是扣子账号的积分路由红线默认值。
	//
	// 低于该值的账号不再承载新请求，避免请求打到上游后才因积分不足失败。
	defaultMinRoutableCredits int64 = 100

	// creditRefreshDefaultSeconds 是后台积分刷新间隔默认值。
	//
	// 积分余额只在面板与闸门两处被读，5 分钟（与积分缓存 TTL 一致）
	// 足以让闸门跟上消耗速度，又不会给上游增加可观请求量。
	creditRefreshDefaultSeconds = 300

	// creditRefreshConcurrency 是单轮积分刷新的并发上限。
	//
	// 与续期循环同口径：账号多时串行会把一轮拖很久，并发过高又容易
	// 触发上游风控，取 4 作为折中。
	creditRefreshConcurrency = 4
)

var (
	minRoutableCredits   int64 = defaultMinRoutableCredits
	minRoutableCreditsMu sync.RWMutex

	// creditPoolSnapshot 是闸门读的账号池快照，由积分循环每轮重建。
	//
	// 闸门位于每个请求的执行入口，绝不能在那里调 host.auth.list——
	// 那是「一次列目录 + 每账号一次读取」的 RPC 扇出，会把每次推理的
	// 首字时间拖慢数毫秒到数十毫秒。快照是纯内存读，代价可忽略。
	creditPoolSnapshot atomic.Value // []creditPoolEntry
)

// creditPoolEntry 是闸门判定所需的单账号内存视图。
type creditPoolEntry struct {
	authID string
	usable bool
	remain int64
	known  bool
}

// loadedMinRoutableCredits 返回当前生效的积分路由红线。
// [参数] 无。
// [返回] 当前红线值。
func loadedMinRoutableCredits() int64 {
	minRoutableCreditsMu.RLock()
	defer minRoutableCreditsMu.RUnlock()
	return minRoutableCredits
}

// setMinRoutableCredits 设置积分路由红线并返回还原函数。
// [参数] n: 目标红线值。
// [返回] 还原原值并解除设置的函数。
func setMinRoutableCredits(n int64) func() {
	minRoutableCreditsMu.Lock()
	old := minRoutableCredits
	minRoutableCredits = n
	minRoutableCreditsMu.Unlock()
	return func() {
		minRoutableCreditsMu.Lock()
		minRoutableCredits = old
		minRoutableCreditsMu.Unlock()
	}
}

// applyMinRoutableCredits 直接设置积分路由红线，不返回还原函数。
//
// 配置热更新用它：还原闭包是给单测做隔离的，生产路径不需要。
// [参数] n: 目标红线值。
// [返回] 无。
func applyMinRoutableCredits(n int64) {
	minRoutableCreditsMu.Lock()
	minRoutableCredits = n
	minRoutableCreditsMu.Unlock()
}

// cachedCreditRemain 读取账号的缓存积分余量。
//
// 只用缓存，绝不在这里发起上游请求：闸门位于每个请求的执行入口，
// 在那里同步打积分接口会把每次推理的首字时间拖慢一次往返。
// 未知积分返回 (-1, false)，由调用方决定放行还是拦截。
// [参数] authID: 账号标识。
// [返回] 余量与「是否已知」。
func cachedCreditRemain(authID string) (int64, bool) {
	if authID == "" {
		return -1, false
	}
	creditsCache.mu.Lock()
	defer creditsCache.mu.Unlock()
	entry, ok := creditsCache.entries[authID]
	if !ok || entry.summary.Error != "" {
		return -1, false
	}
	return entry.summary.Remain, true
}

// isCreditsBelowRedline 判断账号积分是否已低于路由红线。
//
// 未知积分一律返回 false（放行）：查不到余额时把账号挡在门外，
// 会在积分接口抖动时让整池账号同时不可用，代价远大于多打一次上游。
// [参数] authID: 账号标识。
// [返回] 已知且低于红线时为 true。
func isCreditsBelowRedline(authID string) bool {
	remain, known := cachedCreditRemain(authID)
	if !known {
		return false
	}
	return remain < loadedMinRoutableCredits()
}

// publishCreditPool 用账号列表与缓存重建闸门快照。
//
// 只由积分循环调用：它已经持有账号列表，顺手把「谁可用、谁还有多少分」
// 拍平成纯内存结构，请求路径就不必再碰宿主 RPC。
// [参数] accounts: 全量账号。
// [返回] 无。
func publishCreditPool(accounts []*StoredAuth) {
	entries := make([]creditPoolEntry, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		remain, known := cachedCreditRemain(acc.AuthID)
		entries = append(entries, creditPoolEntry{
			authID: acc.AuthID,
			usable: !acc.Disabled && authStateOf(acc) == "ok",
			remain: remain,
			known:  known,
		})
	}
	creditPoolSnapshot.Store(entries)
}

// loadCreditPool 读取闸门快照。
// [参数] 无。
// [返回] 快照；尚未发布过时返回 nil。
func loadCreditPool() []creditPoolEntry {
	if v, ok := creditPoolSnapshot.Load().([]creditPoolEntry); ok {
		return v
	}
	return nil
}

// hasRoutableAccountExcept 判断除指定账号外是否还有账号可以承载流量。
//
// 这是闸门的核心取舍：只有当「池里还有能用的号」时才拦截当前请求，
// 否则把最后一个残血号也挡掉，用户会直接看到无号可用。全池都低时
// 宁可放行让上游自己判，也不做无意义的拒绝。
// [参数] excludeAuthID: 当前请求已经选中的账号；pool: 账号池快照。
// [返回] 存在其他可用账号时为 true。
func hasRoutableAccountExcept(excludeAuthID string, pool []creditPoolEntry) bool {
	redline := loadedMinRoutableCredits()
	for _, entry := range pool {
		if entry.authID == excludeAuthID || !entry.usable {
			continue
		}
		// 未知积分的账号按可用算：宁可换过去试一次，
		// 也不要在余额接口抖动时判定整池不可用。
		if entry.known && entry.remain < redline {
			continue
		}
		return true
	}
	return false
}

// verifyAccountCredits 在执行前校验所选账号的积分是否足够承载本次请求。
//
// 只有「本号低于红线」且「池里还有别的可用号」时才拒绝：前者保证不会
// 白打一次上游，后者保证拒绝是有意义的。返回的错误带 403，宿主据此
// 走账号级换号通道（与上游 1000521 积分耗尽同一条路径）。
//
// 全程只读内存快照与积分缓存，不发任何宿主 RPC 或上游请求。
// [参数] sa: 本次请求选中的账号。
// [返回] 应换号时返回带状态码的错误，否则 nil。
func verifyAccountCredits(sa *StoredAuth) error {
	if sa == nil {
		return nil
	}
	remain, known := cachedCreditRemain(sa.AuthID)
	if !known {
		return nil
	}
	redline := loadedMinRoutableCredits()
	if remain >= redline {
		return nil
	}
	if !hasRoutableAccountExcept(sa.AuthID, loadCreditPool()) {
		return nil
	}
	// 只在真正换号时留痕：这是宿主的选中结果与积分现状不一致的证据，
	// 常驻日志里出现即说明账号池的余额分布需要人工关注。
	hostLogf("warn", "账号积分低于路由红线，转由宿主换号", map[string]any{
		"auth_id": sa.AuthID,
		"remain":  remain,
		"redline": redline,
	})
	return &creditsBelowRedlineError{remain: remain, redline: redline}
}

// creditsBelowRedlineError 表示账号积分低于红线、应换号的失败。
//
// 状态码取 403 而不是 402/429：宿主的换号白名单
// （isCredentialRetryRoundStatus）只认 403/408/429/500/502/503/504，
// 而 429 的冷却下限只有 10 秒，不足以让账号等到积分恢复。
// [参数] 无。
// [返回] 无。
type creditsBelowRedlineError struct {
	remain  int64
	redline int64
}

// Error 返回面向用户的失败说明。
// [参数] 无。
// [返回] 错误文案。
func (e *creditsBelowRedlineError) Error() string {
	return fmt.Sprintf("账号积分不足：剩余 %d 分，低于路由红线 %d 分，正在切换账号", e.remain, e.redline)
}

// StatusCode 返回宿主分类用的 HTTP 状态码。
// [参数] 无。
// [返回] 403。
func (e *creditsBelowRedlineError) StatusCode() int {
	return http.StatusForbidden
}
