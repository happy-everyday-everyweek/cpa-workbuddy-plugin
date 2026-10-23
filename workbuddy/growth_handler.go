// growth_handler.go 是成长中心的管理入口：处理 POST /plugins/workbuddy-provider/growth。
//
// 与 handleManualCheckin 同构：单账号（body.auth_index）直连，缺省时全量扇出（sem=4）。
// Global 账号不支持成长中心，直接返回 skipped_global。
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleGrowth 处理 POST /growth。
// [参数] req：宿主透传的管理请求。
// [返回] {results, summary} 结构；无匹配账号时返回 error。
// 最近修改时间：2026-10-24 新增管理入口（TASK-03.2）
func handleGrowth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthListFn()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	targets := make([]pluginapi.HostAuthFileEntry, 0, len(files))
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	// 1. 账号间并发扇出（sem=4），账号内由 growthLockFor 串行
	results := make([]map[string]any, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if i > 0 {
				time.Sleep(growthInterAccountDelayFn())
			}
			results[i] = growthOneAccount(f)
		}(i, f)
	}
	wg.Wait()

	// 2. 汇总契约与签到保持一致：total/success/fail/skipped_global
	successN, failN, globalN := 0, 0, 0
	var credits, energy int64
	var claimed, accepted int
	for _, out := range results {
		if out == nil {
			continue
		}
		if out["error"] != nil {
			failN++
			continue
		}
		if out["reason"] == "global" {
			globalN++
			continue
		}
		if errs, ok := out["errors"].([]string); ok && len(errs) > 0 {
			failN++
		} else {
			successN++
		}
		credits += toInt64(out["credits_gained"])
		energy += toInt64(out["energy_gained"])
		claimed += int(toInt64(out["tasks_claimed"]))
		accepted += int(toInt64(out["tasks_accepted"]))
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":          len(targets),
			"success":        successN,
			"fail":           failN,
			"skipped_global": globalN,
			"credits_gained": credits,
			"energy_gained":  energy,
			"tasks_accepted": accepted,
			"tasks_claimed":  claimed,
		},
	}
}

// growthOneAccount 对单个账号执行一轮成长中心。
// [参数] f：账号条目。
// [返回] 单账号结果 map；Global 账号返回 skipped_global。
// 最近修改时间：2026-10-24 新增管理入口（TASK-03.2）
func growthOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	out := map[string]any{"auth_index": f.AuthIndex}

	sa, err := hostAuthGetFn(f.AuthIndex)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["nickname"] = sa.Account.Nickname

	// Global 账号没有成长中心活动，直接跳过（不发任何上游请求）
	if isGlobalDomain(sa.Auth.Domain) {
		out["success"] = false
		out["skipped"] = true
		out["reason"] = "global"
		out["message"] = "国际版账号无成长中心，请使用签到 / 专家加油包"
		return out
	}

	res := runGrowthForAccount(sa, f.AuthIndex)
	if res == nil {
		out["success"] = false
		out["skipped"] = true
		out["reason"] = "busy"
		out["message"] = "该账号正在执行成长中心，请稍后重试"
		return out
	}
	out["success"] = len(res.Errors) == 0
	out["credits_gained"] = res.CreditsGained
	out["energy_gained"] = res.EnergyGained
	out["energy_balance"] = res.EnergyBalance
	out["tasks_accepted"] = res.TasksAccepted
	out["tasks_claimed"] = res.TasksClaimed
	out["tasks_progressed"] = res.TasksProgressed
	out["buddy_unlocked"] = res.BuddyUnlocked
	out["segments"] = res.Segments
	out["errors"] = res.Errors
	return out
}

// toInt64 把 map 中以 any 存放的整数安全转换。
// [参数] v：原始值（int/int64/float64 均可能）。
// [返回] 转换后的 int64；无法转换时返回 0。
// 最近修改时间：2026-10-24 新增管理入口（TASK-03.2）
func toInt64(v any) int64 {
	switch value := v.(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	}
	return 0
}
