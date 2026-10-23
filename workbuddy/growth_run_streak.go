// growth_run_streak.go 是成长中心的连登与盲盒分段：补登、阶梯兑换、抽奖、盲盒开启。
// 这些段都涉及消耗类写操作（补登卡、兑换额度、抽奖次数、能量），调用前先查状态。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// growthRunMakeup 执行补登段：定位当月首个断登日并使用一张补登卡。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthRunMakeup(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	// 1. 读连登与热力图，二者缺一不可（补登判据同时依赖）
	streak, err := fetchGrowthStreak(sa)
	if err != nil {
		res.Errors = append(res.Errors, "streak: "+err.Error())
		return
	}
	heatmap, err := fetchGrowthHeatmap(sa)
	if err != nil {
		res.Errors = append(res.Errors, "heatmap: "+err.Error())
		return
	}
	// 2. 定位当月首个断登日；无缺口或余额不足则本段无操作
	target := growthMakeupTarget(streak, heatmap, time.Now())
	if target == "" {
		return
	}
	if _, useErr := useMakeupCard(sa, target); useErr != nil {
		res.Errors = append(res.Errors, "makeup_use: "+useErr.Error())
		return
	}
	res.Segments = append(res.Segments, "makeup:"+target)
}

// growthMakeupTarget 计算当月首个可补登日期。
// 判据：补登卡余额 > 0；从当月 1 日到昨日逐日检查热力图，返回第一个无记录日。
// [参数] streak：连登状态；heatmap：签到热力图；now：当前时间。
// [返回] YYYY-MM-DD 形式的补登目标日；无缺口时返回空串。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthMakeupTarget(streak, heatmap map[string]any, now time.Time) string {
	// 1. 检查补登卡余额；无卡直接返回
	cards, _ := streak["makeup_cards"].(map[string]any)
	if jsonI64(cards, "balance") <= 0 {
		return ""
	}
	// 2. 收集本月已有签到的日期集合
	signed := map[string]bool{}
	cells, _ := heatmap["cells"].([]any)
	for _, raw := range cells {
		cell, _ := raw.(map[string]any)
		if date := jsonStr(cell, "date"); date != "" && jsonI64(cell, "score") > 0 {
			signed[date] = true
		}
	}
	// 3. 从当月 1 日逐日检查到昨日，返回第一个断登日
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	yesterday := now.AddDate(0, 0, -1)
	for d := monthStart; !d.After(yesterday); d = d.AddDate(0, 0, 1) {
		if !signed[d.Format("2006-01-02")] {
			return d.Format("2006-01-02")
		}
	}
	return ""
}

// growthRunRedeem 执行连登阶梯兑换段：达成档位则兑换最高档。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthRunRedeem(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	summary, err := fetchGrowthRedeemSummary(sa)
	if err != nil {
		res.Errors = append(res.Errors, "redeem_summary: "+err.Error())
		return
	}
	// 从最高档往下找第一个可兑换档位
	for _, tier := range growthRedeemTierOrder {
		if !growthStatusRedeemable(summary[tier.StatusKey]) {
			continue
		}
		m, redeemErr := redeemTier(sa, tier.Tier)
		if redeemErr != nil {
			res.Errors = append(res.Errors, "redeem "+tier.Tier+": "+redeemErr.Error())
			return
		}
		res.CreditsGained += jsonI64(m, "credit_granted")
		res.Segments = append(res.Segments, "redeem:"+tier.Tier)
		return
	}
}

// growthRedeemTierOrder 是兑换档位从高到低的顺序与对应状态字段。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
var growthRedeemTierOrder = []struct {
	Tier      string
	StatusKey string
}{
	{"28d", "legendary_status"},
	{"14d", "advanced_status"},
	{"7d", "starter_status"},
}

// growthStatusRedeemable 判断兑换状态是否可领取。
// [参数] v：状态字段值（上游可能是字符串或对象）。
// [返回] true 表示该档位可兑换。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthStatusRedeemable(v any) bool {
	switch value := v.(type) {
	case string:
		return value == "redeemable" || value == "available"
	case map[string]any:
		status := jsonStr(value, "status")
		return status == "redeemable" || status == "available"
	}
	return false
}

// growthRunLottery 执行抽奖段：有次数则最多抽 1 次。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthRunLottery(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	chances, err := fetchGrowthLotteryChances(sa)
	if err != nil {
		res.Errors = append(res.Errors, "lottery_chances: "+err.Error())
		return
	}
	if jsonI64(chances, "balance") <= 0 {
		return
	}
	m, drawErr := drawLottery(sa)
	if drawErr != nil {
		res.Errors = append(res.Errors, "lottery_draw: "+drawErr.Error())
		return
	}
	res.CreditsGained += jsonI64(m, "credit")
	res.EnergyGained += jsonI64(m, "energy")
	res.Segments = append(res.Segments, "lottery")
}

// growthRunBuddyBox 执行盲盒段：额度可负担则开 1 个。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthRunBuddyBox(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	quota, err := fetchBuddyQuota(sa)
	if err != nil {
		res.Errors = append(res.Errors, "buddy_quota: "+err.Error())
		return
	}
	if !jsonBool(quota, "affordable") {
		return
	}
	m, openErr := openBuddyBox(sa, 1)
	if openErr != nil {
		res.Errors = append(res.Errors, "buddy_open: "+openErr.Error())
		return
	}
	res.CreditsGained += jsonI64(m, "credit")
	res.EnergyGained += jsonI64(m, "energy")
	res.Segments = append(res.Segments, "buddy_box")
}

// growthCollectSnapshot 汇总只读快照：能量余额与连登天数。
// 余额记入 EnergyBalance 而非 EnergyGained，避免零领取轮次显示虚假收益。
// [参数] sa：账号凭据；res：结果累加器；deadline：预算截止时间。
// [返回] 无（结果写入 res）。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthCollectSnapshot(sa *storedAuth, res *growthResult, deadline time.Time) {
	if growthExpired(deadline) {
		return
	}
	if m, err := fetchGrowthEnergy(sa); err == nil {
		res.EnergyBalance = jsonI64(m, "balance")
	}
	if m, err := fetchGrowthStreak(sa); err == nil {
		streak, _ := m["streak"].(map[string]any)
		res.Segments = append(res.Segments, fmt.Sprintf("streak=%d", jsonI64(streak, "days")))
	}
}

// growthTravelRecordID 把原始 JSON 片段转成可比较的字符串，供日志与断言使用。
// [参数] raw：record_id 原始 JSON。
// [返回] 去引号后的字符串；空值为空串。
// 最近修改时间：2026-10-24 新增编排（TASK-03.1）
func growthTravelRecordID(raw json.RawMessage) string {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "null" {
		return ""
	}
	return text
}
