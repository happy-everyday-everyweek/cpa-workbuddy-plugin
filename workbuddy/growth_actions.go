// growth_actions.go 是成长中心的写端点：连登补登/兑换/抽奖、Buddy 领养与旅行、盲盒开启。
// 全部为 POST 且一律单发不重试——这些端点会真实发放积分、消耗补登卡与抽奖次数。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// -----------------------------------------------------------------------------

// useMakeupCard 对指定日期使用一张补登卡，补齐断登日。
// [参数] sa：账号凭据；targetDate：目标日期（YYYY-MM-DD）。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func useMakeupCard(sa *storedAuth, targetDate string) (map[string]any, error) {
	// 1. 空日期直接拒绝，避免对无意义目标消耗补登卡
	if strings.TrimSpace(targetDate) == "" {
		return nil, fmt.Errorf("empty target_date")
	}
	return growthPostMap(sa, growthPathMakeupCardsUse, map[string]any{"target_date": targetDate})
}

// redeemTier 领取指定档位的连登阶梯奖励。
// [参数] sa：账号凭据；tier：档位（7d / 14d / 28d）。
// [返回] 响应顶层字段 map；档位非法时直接返回错误且不发请求。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func redeemTier(sa *storedAuth, tier string) (map[string]any, error) {
	// 1. 档位白名单校验：非法档位直接拒绝，不发写请求
	if !growthRedeemTiers[tier] {
		return nil, fmt.Errorf("invalid redeem tier: %q", tier)
	}
	// 2. 携带 client_token 兑换，供上游去重
	return growthPostMap(sa, growthPathRedeem, map[string]any{
		"tier":         tier,
		"client_token": growthClientToken(),
	})
}

// drawLottery 消耗一次抽奖机会抽取盲盒。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map（含 prize_name 等）；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func drawLottery(sa *storedAuth) (map[string]any, error) {
	return growthPostMap(sa, growthPathLotteryDraw, map[string]any{"client_token": growthClientToken()})
}

// -----------------------------------------------------------------------------
// Buddy 领养与旅行
// -----------------------------------------------------------------------------

// growthTravelStatus 是 Buddy 旅行的当前状态。
// State 三态：idle（待派出）/ traveling（旅行中）/ arrived（已到达可收礼）。
// RecordID 保留上游原始 JSON：上游可能为数字形态，强类型 string 绑定会整段解析失败。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
type growthTravelStatus struct {
	State             string          `json:"state"`
	RecordID          json.RawMessage `json:"record_id"`
	RewardCredit      int64           `json:"reward_credit"`
	DailyLimitReached bool            `json:"daily_limit_reached"`
}

// growthTravelLocation 是一个可选旅行目的地。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
type growthTravelLocation struct {
	ID   json.RawMessage `json:"id"`
	Code string          `json:"code"`
	Name string          `json:"name"`
}

// fetchTravelStatus 读取 Buddy 旅行状态。
// [参数] sa：账号凭据。
// [返回] 旅行状态；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func fetchTravelStatus(sa *storedAuth) (*growthTravelStatus, error) {
	m, err := growthGetMap(sa, growthPathTravelStatus)
	if err != nil {
		return nil, err
	}
	st := &growthTravelStatus{
		State:             jsonStr(m, "state"),
		RewardCredit:      jsonI64(m, "reward_credit"),
		DailyLimitReached: jsonBool(m, "daily_limit_reached"),
	}
	if raw, ok := m["record_id"]; ok {
		if encoded, encErr := json.Marshal(raw); encErr == nil {
			st.RecordID = encoded
		}
	}
	return st, nil
}

// fetchTravelConfig 读取可选旅行目的地列表。
// [参数] sa：账号凭据。
// [返回] 目的地列表；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func fetchTravelConfig(sa *storedAuth) ([]growthTravelLocation, error) {
	m, err := growthGetMap(sa, growthPathTravelConfig)
	if err != nil {
		return nil, err
	}
	rawList, _ := m["locations"].([]any)
	locations := make([]growthTravelLocation, 0, len(rawList))
	for _, raw := range rawList {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		loc := growthTravelLocation{Code: jsonStr(item, "code"), Name: jsonStr(item, "name")}
		if id, exists := item["id"]; exists {
			if encoded, encErr := json.Marshal(id); encErr == nil {
				loc.ID = encoded
			}
		}
		locations = append(locations, loc)
	}
	return locations, nil
}

// growthIDEmpty 判断上游 id 字段是否为空（null / 空串 / 零值）。
// [参数] raw：原始 JSON 片段。
// [返回] true 表示该 id 不可用。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func growthIDEmpty(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text == "" || text == "null" || text == `""` || text == "0"
}

// claimTravelReward 领取已到站的旅行奖励。
// [参数] sa：账号凭据；recordID：旅行记录 id（原始 JSON）。
// [返回] 响应顶层字段 map；id 为空时直接返回错误且不发请求。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func claimTravelReward(sa *storedAuth, recordID json.RawMessage) (map[string]any, error) {
	// 1. 空 id 直接拒绝，避免无效写请求
	if growthIDEmpty(recordID) {
		return nil, fmt.Errorf("empty record_id")
	}
	// 2. record_id 原样透传，避免数字/字符串形态被强转破坏
	var id any
	if err := json.Unmarshal(recordID, &id); err != nil {
		return nil, err
	}
	return growthPostMap(sa, growthPathTravelClaim, map[string]any{"record_id": id})
}

// departBuddy 派出 Buddy 前往指定目的地开始旅行。
// [参数] sa：账号凭据；locationID：目的地 id（原始 JSON）。
// [返回] 响应顶层字段 map；id 为空时直接返回错误且不发请求。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func departBuddy(sa *storedAuth, locationID json.RawMessage) (map[string]any, error) {
	// 1. 空 id 直接拒绝
	if growthIDEmpty(locationID) {
		return nil, fmt.Errorf("empty location_id")
	}
	// 2. location_id 原样透传
	var id any
	if err := json.Unmarshal(locationID, &id); err != nil {
		return nil, err
	}
	return growthPostMap(sa, growthPathTravelDepart, map[string]any{"location_id": id})
}

// fetchBuddyQuota 读取盲盒开启额度。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func fetchBuddyQuota(sa *storedAuth) (map[string]any, error) {
	return growthGetMap(sa, growthPathBuddyQuota)
}

// openBuddyBox 消耗能量开启指定数量的 Buddy 盲盒。
// [参数] sa：账号凭据；count：开启数量（<=0 时直接返回错误）。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func openBuddyBox(sa *storedAuth, count int) (map[string]any, error) {
	if count <= 0 {
		return nil, fmt.Errorf("invalid open count: %d", count)
	}
	return growthPostMap(sa, growthPathBuddyOpen, map[string]any{"count": count})
}

// buddyAgreement 同意 Buddy 领养协议（幂等，重复调用无副作用）。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增 Buddy 领养链（TASK-02.1）
func buddyAgreement(sa *storedAuth) (map[string]any, error) {
	return growthPostMap(sa, growthPathBuddyAgreement, map[string]any{"agree": true})
}

// buddyFirst 领取第一只 Buddy。
// 这是全部成长任务的前置门槛：未领养时 accept 对每个任务返回
// "prerequisite not met: first_buddy"（2026-10-24 实测，见需求文档 §3.1）。
// 上游无猫且已过活跃门槛时赠 300 分；已领养时返回 already_claimed。
// [参数] sa：账号凭据。
// [返回] 响应顶层字段 map（含 already_claimed / buddy / credit / energy）；失败时返回错误。
// 最近修改时间：2026-10-24 新增 Buddy 领养链（TASK-02.1）
func buddyFirst(sa *storedAuth) (map[string]any, error) {
	return growthPostMap(sa, growthPathBuddyFirst, map[string]any{})
}
