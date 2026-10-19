package main

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	schedulerIndex int
	schedulerMu    sync.Mutex
)

// accountRoutable 判断一个账号是否具备承载流量的资格。
// [参数] sa: 账号凭据；model: 已归一化的上游模型名，传空串时退化为纯账号维度判定。
// [返回] true 表示可参与调度。
// 最近修改时间：2026-10-19 02:00:00；改动原因：冷却判定带模型维度。
func accountRoutable(sa *StoredAuth, model string) bool {
	if sa == nil || sa.Disabled || credentialOf(sa) == "" {
		return false
	}
	if sa.TestFailed {
		return false
	}
	if isAuthModelCoolingDown(sa.AuthID, model) {
		return false
	}
	return true
}

// pickActiveAuth 从所有落盘账号中挑选一个可承载流量的可用账号。
// [参数] model: 已归一化的上游模型名，可为空。
// [返回] 目标账号；无可选账号时返回 nil。
// 最近修改时间：2026-10-19 02:00:00；改动原因：冷却判定带模型维度。
func pickActiveAuth(model string) *StoredAuth {
	accounts, err := listAllAuthFiles()
	if err != nil || len(accounts) == 0 {
		return nil
	}

	var candidates []*StoredAuth
	for _, acc := range accounts {
		if accountRoutable(acc, model) {
			candidates = append(candidates, acc)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	schedulerMu.Lock()
	defer schedulerMu.Unlock()

	idx := schedulerIndex % len(candidates)
	schedulerIndex++
	return candidates[idx]
}

// pickNextAuth 在当前账号发生故障时，选取下一个可用候选账号进行换号重试。
// [参数] currentAuthID: 当前失败账号标识；model: 已归一化的上游模型名，可为空。
// [返回] 下一个候选账号与是否存在。
// 最近修改时间：2026-10-19 02:00:00；改动原因：冷却判定带模型维度。
func pickNextAuth(currentAuthID, model string) (*StoredAuth, bool) {
	currentAuthID = strings.TrimSpace(currentAuthID)
	accounts, err := listAllAuthFiles()
	if err != nil || len(accounts) <= 1 {
		return nil, false
	}

	var candidates []*StoredAuth
	for _, acc := range accounts {
		if acc.AuthID != currentAuthID && accountRoutable(acc, model) {
			candidates = append(candidates, acc)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}
	return candidates[0], true
}

// candidateDisabled 判断宿主下发的候选是否已被宿主标记为禁用。
// [参数] c: 宿主调度候选。
// [返回] true 表示该候选禁用、不应承载流量。
// 最近修改时间：2026-10-15 21:56:00；改动原因：新增，对齐宿主候选禁用语义。
func candidateDisabled(c pluginapi.SchedulerAuthCandidate) bool {
	// 1. 宿主状态标记为 disabled。
	if strings.EqualFold(strings.TrimSpace(c.Status), "disabled") {
		return true
	}

	// 2. 宿主元数据 disabled 为真。
	if c.Metadata != nil {
		if v, ok := c.Metadata["disabled"]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return strings.EqualFold(strings.TrimSpace(t), "true")
			}
		}
	}
	return false
}

// handleSchedulerPick 响应宿主的 MethodSchedulerPick RPC 请求。
//
// 只在宿主下发的候选集内取号并原样返回候选 ID，宿主按候选 ID 逐字校验。
// 非本 provider 候选或无可用候选时返回 Handled:false，交由宿主内建调度器处理。
// [参数] raw: 宿主调度请求载荷。
// [返回] 调度响应信封与错误。
// 最近修改时间：2026-10-15 21:56:00；改动原因：修复越界返回自有账号 ID 致 unknown auth id。
func handleSchedulerPick(raw []byte) ([]byte, error) {
	// 1. 解析宿主下发的调度请求。
	var req pluginapi.SchedulerPickRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}

	// 2. 过滤出本 provider、未禁用且不在冷却期的候选。
	// 冷却判定带模型维度：账号×模型级冷却只应挡住同一模型的请求。
	upstreamModel := mappedModel(req.Model, currentConfig())
	var candidates []pluginapi.SchedulerAuthCandidate
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		if candidateDisabled(c) {
			continue
		}
		if isAuthModelCoolingDown(strings.TrimSuffix(c.ID, ".json"), upstreamModel) {
			continue
		}
		candidates = append(candidates, c)
	}

	// 3. 无可用候选时让位宿主内建调度器。
	if len(candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// 4. 在候选集内轮转取号，返回原样候选 ID。
	schedulerMu.Lock()
	idx := schedulerIndex % len(candidates)
	schedulerIndex++
	schedulerMu.Unlock()

	return okEnvelope(pluginapi.SchedulerPickResponse{
		Handled: true,
		AuthID:  candidates[idx].ID,
	})
}
