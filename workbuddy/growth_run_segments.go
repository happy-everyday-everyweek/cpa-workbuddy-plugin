// growth_run_segments.go 是成长中心各玩法分段的具体实现，供 growthRunForAccount 调用。
// 每段独立捕获错误：任一段失败只记入 res.Errors，不中断其余段。
package main

import (
	"time"
)

// growthRunTravel 执行 Buddy 旅行段：到达收礼 → 读取配置 → 再派出。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthRunTravel(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	// 1. 读旅行状态；失败即记录并退出本段
	st, err := fetchTravelStatus(sa)
	if err != nil {
		res.Errors = append(res.Errors, "travel_status: "+err.Error())
		return
	}
	// 2. 仅在 arrived 状态收礼；traveling 直接返回，idle 继续走派出
	if st.State == "arrived" {
		if m, claimErr := claimTravelReward(sa, st.RecordID); claimErr != nil {
			res.Errors = append(res.Errors, "travel_claim: "+claimErr.Error())
		} else {
			res.CreditsGained += jsonI64(m, "reward_credit")
			res.Segments = append(res.Segments, "travel_claim")
		}
	} else if st.State != "idle" && st.State != "" {
		return
	}
	// 3. 已达当日上限则不再派出，避免无谓写请求
	if st.DailyLimitReached {
		return
	}
	// 4. 读配置取首个目的地后派出
	locations, err := fetchTravelConfig(sa)
	if err != nil {
		res.Errors = append(res.Errors, "travel_config: "+err.Error())
		return
	}
	if len(locations) == 0 {
		return
	}
	if _, departErr := departBuddy(sa, locations[0].ID); departErr != nil {
		res.Errors = append(res.Errors, "travel_depart: "+departErr.Error())
		return
	}
	res.Segments = append(res.Segments, "travel_depart")
}

// growthRunTasks 执行任务段：上报点亮 → 接单 → 回读 → 按进度口径领奖。
//
// 顺序关键：必须先生效未接单任务（accept），再等结算后回读，最后只领
// 进度已达标的项。判据用 progress.current >= progress.target，不用 accept_status，
// 因为后者会短暂回填后回滚，据此领奖会误领。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthRunTasks(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	// 1. 拉任务列表；失败即记录并退出本段
	tasks, err := fetchGrowthTasks(sa)
	if err != nil {
		res.Errors = append(res.Errors, "tasks: "+err.Error())
		return
	}
	// 2. 统计已有进度（含领养前已积累的进度），用于回报可观测性
	for _, t := range tasks {
		if t.Progress != nil && t.Progress.Current > 0 {
			res.TasksProgressed++
		}
	}
	// 3. 批量接单未接单项；逐条失败原因一并计入本段错误
	pending := pendingAcceptCodes(tasks)
	if len(pending) > 0 {
		outcome, acceptErr := acceptGrowthTasks(sa, pending)
		res.Errors = append(res.Errors, outcome.Errors...)
		if acceptErr != nil {
			res.Errors = append(res.Errors, "tasks_accept: "+acceptErr.Error())
		} else if outcome.Accepted > 0 {
			res.TasksAccepted += outcome.Accepted
			res.Segments = append(res.Segments, "tasks_accept")
		}
		// 接单后等结算，让 accept 状态与进度落定
		time.Sleep(growthSettleDelay)
	}
	// 4. 回读任务列表，按进度口径挑出可领奖项
	if growthExpired(deadline) {
		return
	}
	refreshed, err := fetchGrowthTasks(sa)
	if err != nil {
		res.Errors = append(res.Errors, "tasks_refresh: "+err.Error())
		return
	}
	// 5. 逐个领奖；alreadary_claimed 视为成功
	for _, t := range claimableTasks(refreshed) {
		m, claimErr := claimGrowthTask(sa, t.TaskCode)
		if claimErr != nil {
			res.Errors = append(res.Errors, "task_claim "+t.TaskCode+": "+claimErr.Error())
			continue
		}
		res.CreditsGained += jsonI64(m, "credit")
		res.EnergyGained += jsonI64(m, "energy")
		res.TasksClaimed++
	}
	if res.TasksClaimed > 0 {
		res.Segments = append(res.Segments, "tasks_claim")
	}
}
