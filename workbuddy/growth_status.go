// growth_status.go 是成长中心的只读状态查询：连登、热力图、能量、兑换、抽奖次数。
// 全部为 GET，失败可重试（由 growthCall 按方法判定）。
package main

// fetchGrowthStreak 读取连登域状态：连登天数、补登卡余额、阶梯兑换状态。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func fetchGrowthStreak(sa *storedAuth) (map[string]any, error) {
	return growthGetMap(sa, growthPathStreak)
}

// fetchGrowthHeatmap 读取签到热力图：今日日期与逐日活跃分值。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func fetchGrowthHeatmap(sa *storedAuth) (map[string]any, error) {
	return growthGetMap(sa, growthPathHeatmap)
}

// fetchGrowthEnergy 读取 Buddy 能量余额。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func fetchGrowthEnergy(sa *storedAuth) (map[string]any, error) {
	return growthGetMap(sa, growthPathEnergy)
}

// fetchGrowthRedeemSummary 读取连登阶梯兑换状态。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func fetchGrowthRedeemSummary(sa *storedAuth) (map[string]any, error) {
	return growthGetMap(sa, growthPathRedeemSummary)
}

// fetchGrowthLotteryChances 读取盲盒抽奖剩余次数。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func fetchGrowthLotteryChances(sa *storedAuth) (map[string]any, error) {
	return growthGetMap(sa, growthPathLotteryChances)
}
