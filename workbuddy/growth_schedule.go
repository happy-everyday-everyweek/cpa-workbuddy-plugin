// growth_schedule.go 是成长中心的定时调度接入：tick 入口与账号间抖动。
// 与手动入口的区别仅在于不返回结果给面板，日志是唯一可观测点。
package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// runScheduledGrowth 是定时 tick 的成长中心入口：执行一轮并记录结果日志。
// 与手动入口的区别仅在于不返回结果给面板，日志是唯一可观测点。
// [参数] sa：账号凭据；f：账号条目。
// [返回] 无。
// 最近修改时间：2026-10-24 新增调度接入（TASK-04.1）
func runScheduledGrowth(sa *storedAuth, f pluginapi.HostAuthFileEntry) {
	res := runGrowthForAccount(sa, f.AuthIndex)
	if res == nil {
		// 账号级锁被占用（例如面板正在手动执行），本轮跳过属正常
		return
	}
	log.Printf("[workbuddy] growth scheduled for %s: +%d credits, +%d energy, %d accepted, %d claimed, %d progressed, %d errors",
		sa.Account.UID, res.CreditsGained, res.EnergyGained,
		res.TasksAccepted, res.TasksClaimed, res.TasksProgressed, len(res.Errors))
	// 失败原因必须显式落日志：旧版只打错误条数，用户无法自查为什么任务没完成
	for _, e := range res.Errors {
		log.Printf("[workbuddy] growth error (%s): %s", sa.Account.UID, e)
	}
}

// growthInterAccountDelay 返回 1~3 秒的账号间抖动，避免整批同刻打上游。
// [参数] 无。
// [返回] 抖动时长。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthInterAccountDelay() time.Duration {
	return time.Second + time.Duration(time.Now().UnixNano()%int64(2*time.Second))
}

// growthLockFor 返回账号级互斥锁，保证同一账号成长中心执行串行。
// [参数] authIndex：宿主 RPC 索引。
// [返回] 该账号对应的互斥锁。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthLockFor(authIndex string) *sync.Mutex {
	v, _ := growthLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// pruneGrowthLocks 清理已不存在账号的锁条目，避免 sync.Map 无界增长。
// 锁键是 auth_index，故 live 集合需同时收 ID 与 auth_index。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func pruneGrowthLocks() {
	files, err := hostAuthListFn()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files)*2)
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{}
	}
	growthLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			growthLocks.Delete(key)
		}
		return true
	})
}

// runGrowthForAccount 按固定顺序分段执行成长中心全部玩法。
// [参数] sa：账号凭据；authIndex：账号索引（用于账号级互斥）。
// [返回] 本轮收益与分段结果；nil 表示账号级锁未取得（已在别处执行）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func runGrowthForAccount(sa *storedAuth, authIndex string) *growthResult {
	// 1. 账号级互斥：同一账号不允许并发执行（写端点重复调用会重复扣减）
	mu := growthLockFor(authIndex)
	if !mu.TryLock() {
		return nil
	}
	defer mu.Unlock()

	res := &growthResult{}
	deadline := time.Now().Add(growthBudget)

	// 2. 签到：复用既有 billing 链路，未签则补签
	growthRunCheckin(sa, res, deadline)
	// 3. Buddy 领养：全部任务的前置门槛，必须早于任务段
	growthRunBuddyAdopt(sa, res, deadline)
	// 4. 旅行：到达则收礼，随后按配置再派出
	growthRunTravel(sa, res, deadline)
	// 5. 任务：领养 → 上报 → 接单 → 回读 → 按进度口径领奖
	growthRunTasks(sa, res, deadline)
	// 6. 补登 / 兑换 / 抽奖 / 盲盒
	growthRunMakeup(sa, res, deadline)
	growthRunRedeem(sa, res, deadline)
	growthRunLottery(sa, res, deadline)
	growthRunBuddyBox(sa, res, deadline)
	// 7. 汇总：只读快照，余额不得混入收益
	growthCollectSnapshot(sa, res, deadline)
	return res
}

// growthExpired 判断当前是否已超出执行预算。
// [参数] deadline：预算截止时间。
// [返回] true 表示预算已耗尽，应放弃剩余段。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthExpired(deadline time.Time) bool {
	return time.Now().After(deadline)
}

// growthRunCheckin 执行签到段：未签则补签。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthRunCheckin(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	ci, err := fetchCheckinStatus(sa)
	if err != nil {
		res.Errors = append(res.Errors, "checkin_status: "+err.Error())
		return
	}
	if !ci.Active || ci.TodayCheckedIn {
		return
	}
	m, callErr := performCheckinCall(sa)
	if callErr != nil {
		res.Errors = append(res.Errors, "checkin: "+callErr.Error())
		return
	}
	res.CreditsGained += jsonI64(m, "credit")
	res.Segments = append(res.Segments, "checkin")
}

// growthRunBuddyAdopt 执行 Buddy 领养段：上报解锁 → 同意协议 → 领取第一只。
// 这是全部成长任务的前置门槛。已领养时上游返回 already_claimed，视为成功。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthRunBuddyAdopt(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	// 1. 先判断是否已有 Buddy：quota 端点能反映账号是否已领养
	if m, err := fetchBuddyQuota(sa); err == nil && jsonBool(m, "affordable") {
		res.BuddyUnlocked = true
	}
	// 2. 上报一次活跃事件，解锁 first_buddy 前置
	if err := reportChatActivity(sa, "wb-growth-"+growthTokenSuffix(), ""); err != nil {
		res.Errors = append(res.Errors, "adopt_report: "+err.Error())
		return
	}
	time.Sleep(growthSettleDelay)
	// 3. 同意协议（幂等）
	if _, err := buddyAgreement(sa); err != nil {
		res.Errors = append(res.Errors, "buddy_agreement: "+err.Error())
		return
	}
	// 4. 领取第一只 Buddy
	m, err := buddyFirst(sa)
	if err != nil {
		res.Errors = append(res.Errors, "buddy_first: "+err.Error())
		return
	}
	if jsonBool(m, "already_claimed") {
		res.BuddyUnlocked = true
		return
	}
	res.CreditsGained += jsonI64(m, "credit")
	res.EnergyGained += jsonI64(m, "energy")
	res.BuddyUnlocked = true
	res.Segments = append(res.Segments, "buddy_adopt")
}

// growthTokenSuffix 生成上报用的会话后缀。
// [参数] 无。
// [返回] 毫秒时间戳字符串。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthTokenSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixMilli())
}
