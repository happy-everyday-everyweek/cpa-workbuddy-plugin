// growth_tasks.go 是成长中心的任务域：任务列表查询、接单、领奖，以及进度口径判据。
//
// 关键契约（2026-10-24 实测，见需求文档 §3）：
//   - 未接单任务的 progress 为 null，接单后变为 {current,target} 对象，两种形态都要兼容。
//   - accept 只是报名，不产生进度；真正的进度由服务端行为事件点亮。
//   - accept 对「前置未满足」返回 HTTP 200 + code=0，但把单条失败写在
//     data.results[i].status=error，必须逐条判定，否则会把失败虚报为成功。
//   - claim 判据必须用进度口径（current >= target），不能用 accept_status。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// growthAcceptBatchSize 是单次接单请求携带的最大任务码数量。
const growthAcceptBatchSize = 20

// growthTask 是成长任务列表中的一条任务。
// Progress 用指针区分「未接单（上游为 null）」与「已接单且进度为 0」。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
type growthTask struct {
	TaskCode     string          `json:"task_code"`
	Title        string          `json:"title"`
	TaskDesc     string          `json:"task_desc"`
	TaskType     string          `json:"task_type"`
	Locked       bool            `json:"locked"`
	AcceptStatus string          `json:"accept_status"`
	Progress     *growthProgress `json:"progress"`
	RewardCredit int64           `json:"reward_credit"`
	RewardEnergy int64           `json:"reward_energy"`
}

// growthProgress 是任务进度。上游未接单时该字段整体为 null。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
type growthProgress struct {
	Current int64 `json:"current"`
	Target  int64 `json:"target"`
}

// growthTaskClaimed 判断任务是否已领取。
// [参数] 无（方法接收者）。
// [返回] true 表示上游已标记为已领取。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
func (t growthTask) growthTaskClaimed() bool {
	return t.AcceptStatus == "claimed"
}

// growthTaskClaimable 判断任务是否可领奖。
// 判据只用进度口径：未领取、未锁定、有明确目标且进度已达标。
// 不用 accept_status == "completed"：实测该状态会短暂回填后回滚，据此领奖会误领。
// [参数] 无（方法接收者）。
// [返回] true 表示满足领奖条件。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
func (t growthTask) growthTaskClaimable() bool {
	if t.growthTaskClaimed() || t.Locked || t.Progress == nil {
		return false
	}
	return t.Progress.Target > 0 && t.Progress.Current >= t.Progress.Target
}

// fetchGrowthTasks 读取成长任务列表。
// [参数] sa：账号凭据。
// [返回] 任务列表；data 无 tasks 字段时返回空切片；失败时返回错误。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
func fetchGrowthTasks(sa *storedAuth) ([]growthTask, error) {
	data, err := growthCall(sa, http.MethodGet, growthPathTasks, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tasks []growthTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	return payload.Tasks, nil
}

// growthAcceptOutcome 汇总一次批量接单的真实结果：逐条读上游 data.results 的
// status 判定成败，Errors 按失败原因聚合（含条数），避免逐项铺开撑爆面板与日志。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
type growthAcceptOutcome struct {
	Accepted int
	Errors   []string
}

// acceptGrowthTasks 批量接单，按 growthAcceptBatchSize 自动分批。
//
// 上游对「前置未满足」的任务返回 HTTP 200 + code=0，但把单条失败写在
// data.results[i].status=error（典型 message 为 "prerequisite not met:
// first_buddy (no buddy instance found)"）。只看状态码会把这类失败全部虚报为
// 接单成功。results 缺失或为空时无法逐条判定，回退按整批成功计数。
// [参数] sa：账号凭据；codes：任务码列表（空列表直接返回零值不发请求）。
// [返回] 逐条汇总结果；任一批传输/业务失败即中止并返回错误（已得计数保留）。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
func acceptGrowthTasks(sa *storedAuth, codes []string) (growthAcceptOutcome, error) {
	var out growthAcceptOutcome
	// 1. 空列表直接返回，避免发无意义的写请求
	if len(codes) == 0 {
		return out, nil
	}
	// 2. 逐批接单；按失败原因计数，最后统一聚合成可读错误行
	failByReason := map[string]int{}
	for start := 0; start < len(codes); start += growthAcceptBatchSize {
		end := start + growthAcceptBatchSize
		if end > len(codes) {
			end = len(codes)
		}
		batch := codes[start:end]
		m, err := growthPostMap(sa, growthPathTasksAccept, map[string]any{"task_codes": batch})
		if err != nil {
			return out, err
		}
		// 3. 逐条读 results：缺失或空时无逐条信息，回退整批成功
		results, ok := m["results"].([]any)
		if !ok || len(results) == 0 {
			out.Accepted += len(batch)
			continue
		}
		for _, raw := range results {
			item, _ := raw.(map[string]any)
			if jsonStr(item, "status") == "error" {
				failByReason[jsonStr(item, "message")]++
				continue
			}
			out.Accepted++
		}
	}
	// 4. 聚合失败原因：同因合并成一行并附条数，原因统一脱敏截断
	for _, reason := range sortedCountKeys(failByReason) {
		n := failByReason[reason]
		label := reason
		if label == "" {
			label = "上游未给出原因"
		}
		out.Errors = append(out.Errors, fmt.Sprintf("tasks_accept: %d 项失败：%s", n, truncateRedacted(label, 120)))
	}
	return out, nil
}

// sortedCountKeys 返回计数 map 的键按字典序排列，保证聚合错误行顺序稳定可断言。
// [参数] m：计数 map。
// [返回] 字典序键切片。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
func sortedCountKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pendingAcceptCodes 从任务列表中挑出「未锁定且未接单」的任务码。
// [参数] tasks：任务列表。
// [返回] 待接单任务码切片。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
func pendingAcceptCodes(tasks []growthTask) []string {
	codes := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if !t.Locked && t.AcceptStatus == "not_accepted" && strings.TrimSpace(t.TaskCode) != "" {
			codes = append(codes, t.TaskCode)
		}
	}
	return codes
}

// claimGrowthTask 领取单个已完成任务的奖励。
// [参数] sa：账号凭据；code：任务码。
// [返回] 响应顶层字段 map；code 为空时直接返回错误且不发请求。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
func claimGrowthTask(sa *storedAuth, code string) (map[string]any, error) {
	// 1. 空任务码直接拒绝，避免拼出 /tasks//claim 这种无效路径
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("empty task_code")
	}
	// 2. 领奖：任务码转义后拼进路径
	m, err := growthPostMap(sa, claimGrowthTaskPath(code), map[string]any{})
	if err != nil {
		return nil, err
	}
	// 3. 归一化成功语义：上游对重复领奖返回 already_claimed=true，同属业务成功
	m["success"] = true
	return m, nil
}

// claimableTasks 挑出可领奖的任务。
// [参数] tasks：任务列表。
// [返回] 可领奖任务切片。
// 最近修改时间：2026-10-24 新增任务域（TASK-02.1）
func claimableTasks(tasks []growthTask) []growthTask {
	out := make([]growthTask, 0, len(tasks))
	for _, t := range tasks {
		if t.growthTaskClaimable() && strings.TrimSpace(t.TaskCode) != "" {
			out = append(out, t)
		}
	}
	return out
}
