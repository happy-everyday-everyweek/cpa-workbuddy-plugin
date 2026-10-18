package main

// 本文件把「失败 → 记账 → 换号 → 重试」串成一次请求内的执行闭环。
//
// 与 failover.go 的分工：那个文件只维护冷却状态与判定口径，本文件负责
// 在一次具体请求里驱动重试预算。

import (
	"context"
	"errors"
	"strings"
)

// errNoAvailableAccount 表示池内没有可用账号。
var errNoAvailableAccount = errors.New("没有可用的 Notion 账号：请先在插件面板导入 Cookie")

// runInferenceWithFailover 执行一次推理，失败时按冷却策略换号重试。
//
// 每次失败先记账（决定是否冷却该账号），再判断是否值得同请求换号：
// 429 / 额度耗尽值得立刻换号；40x 受 retry_on_4xx 预算约束；
// 5xx 与传输错误只记冷却，不烧预算。
// 换号时排除刚失败的账号，避免又打回去。
// [参数] auth: 首选账号；chatReq: 请求；sink: 增量回调（可为 nil）。
// [返回] 推理结果与错误；全部账号失败时返回最后一个错误。
// 最近修改时间：2026-10-18；改动原因：新增，TASK-006 同请求换号。
func runInferenceWithFailover(ctx context.Context, auth *StoredAuth, chatReq *openAIChatRequest, sink func(streamEvent) error) (*inferenceResult, error) {
	cfg := currentConfig()
	// 最大尝试次数 = 1 次首选 + 换号预算；40x 预算为 0 时仍允许 429 换号，
	// 因此下限给到 2，避免用户关掉 40x 后连 429 都无法转移。
	maxAttempts := 1 + cfg.RetryOn4xx
	if maxAttempts < 2 {
		maxAttempts = 2
	}

	current := auth
	budget := cfg.RetryOn4xx
	var lastErr error
	// 已经向客户端下发过增量后不能再换号：重试会把前半段正文再发一遍。
	emitted := false
	forward := sink
	if sink != nil {
		forward = func(event streamEvent) error {
			if event.Text != "" {
				emitted = true
			}
			return sink(event)
		}
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if current == nil {
			break
		}

		res, errRun := runInference(ctx, current, chatReq, forward)
		if errRun == nil {
			resetAccountFailover(current.AuthID)
			return res, nil
		}
		lastErr = errRun

		status := classifyInferenceError(errRun)
		noteAccountFailure(current.AuthID, status, cfg.CooldownSeconds)

		// 客户端主动断开不是上游失败：不换号、不重试，直接返回。
		if isClientGone(errRun) {
			return nil, errRun
		}
		if emitted || !shouldRotate(status, budget) {
			break
		}
		if isRetryOn4xxStatus(status) {
			budget--
		}

		next, errPick := pickAccountExcluding(current.AuthID)
		if errPick != nil {
			break
		}
		current = next
	}

	if lastErr == nil {
		lastErr = errNoAvailableAccount
	}
	return nil, lastErr
}

// pickAccountExcluding 从本地账号池挑一个可用账号。
//
// 跳过已停用、跳过凭据为空、跳过冷却中、跳过刚失败的账号；其余按列表顺序取第一个。
// 凭据为空的账号常见于被宿主覆盖过的旧文件，选中它只会稳定报错。
// [参数] exclude: 需要排除的账号标识（可为空）。
// [返回] 选中的账号与错误。
// 最近修改时间：2026-10-19；改动原因：跳过空凭据账号。
func pickAccountExcluding(exclude string) (*StoredAuth, error) {
	accounts, errList := listAllAuthFiles()
	if errList != nil {
		return nil, errList
	}

	exclude = strings.TrimSpace(exclude)
	for _, account := range accounts {
		if account.Disabled || account.CookieHeader == "" || account.AuthID == exclude || isAccountCoolingDown(account.AuthID) {
			continue
		}
		return account, nil
	}
	return nil, errNoAvailableAccount
}
