// growth_run.go 是成长中心的账号级编排：把 7 段玩法串成一次执行。
//
// 执行顺序固定为 签到 → Buddy 领养 → 旅行 → 任务 → 补登 → 兑换 → 抽奖 → 盲盒 → 汇总。
// 「Buddy 领养」必须排在任何任务操作之前：未领养时 accept 对每个任务返回
// prerequisite not met: first_buddy（2026-10-24 实测），这是旧版任务永远完不成的根因。
//
// 容错语义：每段独立执行，任一段失败只记入 Errors，不影响其余段；
// 总时长超过 growthBudget 时放弃剩余段并返回已得结果。
package main

import (
	"sync"
	"time"
)

// growthBudget 是单账号一次执行的软预算，超时后放弃剩余段。
var growthBudget = 180 * time.Second

// growthAuto 控制定时调度是否执行成长中心；默认开启，可由 growth_auto: false 关闭。
var (
	growthAuto   = true
	growthAutoMu sync.RWMutex
)

// growthEnabled 返回定时调度是否应执行成长中心。
// [参数] 无。
// [返回] true 表示开启。
// 最近修改时间：2026-10-24 新增调度开关（TASK-04.1）
func growthEnabled() bool {
	growthAutoMu.RLock()
	defer growthAutoMu.RUnlock()
	return growthAuto
}

// setGrowthAuto 运行时切换成长中心开关（供管理接口使用）。
// [参数] enabled：是否开启。
// [返回] 无。
// 最近修改时间：2026-10-24 新增调度开关（TASK-04.1）
func setGrowthAuto(enabled bool) {
	growthAutoMu.Lock()
	growthAuto = enabled
	growthAutoMu.Unlock()
}

// growthSettleDelay 是上报后等待服务端结算的时长（实测约 3 秒）。
var growthSettleDelay = 3 * time.Second

// growthInterAccountDelayFn 是账号间抖动函数，测试可替换为无等待。
var growthInterAccountDelayFn = growthInterAccountDelay

// growthLocks 是账号级互斥锁表，与 checkinLocks 分离，二者互不阻塞。
var growthLocks sync.Map // auth_index -> *sync.Mutex

// growthResult 汇总一次成长中心执行的收益与分段结果。
// EnergyBalance 是账号当前能量余额（只读快照），与 EnergyGained（本轮收益）语义
// 分离：余额不能混入收益，否则零领取的轮次也会显示「+N 能量」误导用户。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
type growthResult struct {
	CreditsGained   int64    `json:"credits_gained"`
	EnergyGained    int64    `json:"energy_gained"`
	EnergyBalance   int64    `json:"energy_balance"`
	TasksAccepted   int      `json:"tasks_accepted"`
	TasksClaimed    int      `json:"tasks_claimed"`
	TasksProgressed int      `json:"tasks_progressed"`
	BuddyUnlocked   bool     `json:"buddy_unlocked"`
	Segments        []string `json:"segments"`
	Errors          []string `json:"errors"`
}
