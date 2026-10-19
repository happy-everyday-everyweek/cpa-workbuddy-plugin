package main

import "testing"

// TestClassifyUpstreamFailureTable 按实施总览第 3 节的边界表逐行断言分类结果。
// 这张表是冷却维度与探活策略的唯一判据来源，任何一行漂移都必须在这里失败。
func TestClassifyUpstreamFailureTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   failureScope
	}{
		{
			name:   "传输错误归账号级",
			status: 0,
			body:   "connection reset by peer",
			want:   failureScopeAccount,
		},
		{
			name:   "5xx 归账号级",
			status: 502,
			body:   "bad gateway",
			want:   failureScopeAccount,
		},
		{
			name:   "401 归账号级",
			status: 401,
			body:   `{"error":"invalid token"}`,
			want:   failureScopeAccount,
		},
		{
			name:   "403 归账号级",
			status: 403,
			body:   `{"error":"forbidden"}`,
			want:   failureScopeAccount,
		},
		{
			name:   "404 归账号级",
			status: 404,
			body:   `{"error":"not found"}`,
			want:   failureScopeAccount,
		},
		{
			name:   "405 归账号级",
			status: 405,
			body:   `{"error":"method not allowed"}`,
			want:   failureScopeAccount,
		},
		{
			name:   "402 硬额度归账号级",
			status: 402,
			body:   `{"error":"payment required"}`,
			want:   failureScopeAccount,
		},
		{
			name:   "200 带额度标记归账号级",
			status: 200,
			body:   `{"message":"insufficient credits"}`,
			want:   failureScopeAccount,
		},
		{
			name:   "429 归账号×模型级",
			status: 429,
			body:   `{"error":"too many requests"}`,
			want:   failureScopeAuthModel,
		},
		{
			name:   "200 软限流文案归账号×模型级",
			status: 200,
			body:   `{"message":"rate limit exceeded"}`,
			want:   failureScopeAuthModel,
		},
		{
			name:   "零字节断流归账号×模型级",
			status: 200,
			body:   "upstream stream closed before first payload",
			want:   failureScopeAuthModel,
		},
		{
			name:   "400 纯请求形状不冷却",
			status: 400,
			body:   `{"error":"invalid request body"}`,
			want:   failureScopeNone,
		},
		{
			name:   "422 请求形状不冷却",
			status: 422,
			body:   `{"error":"unprocessable entity"}`,
			want:   failureScopeNone,
		},
		{
			// 历史实现先看正文标记再看状态码：400 携带限流文案仍会记账。
			// 周期 1 要求行为零变化，此边界保留原语义并在此显式锁定，
			// 是否收紧为「400 一律不记账」留待实施总览的待裁定项确认。
			name:   "400 携带限流文案按账号×模型记账（复刻历史语义）",
			status: 400,
			body:   `{"error":"too many requests"}`,
			want:   failureScopeAuthModel,
		},
		{
			name:   "正常 200 正文不冷却",
			status: 200,
			body:   `{"choices":[{"message":{"content":"hi"}}]}`,
			want:   failureScopeNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUpstreamFailure(tc.status, tc.body); got != tc.want {
				t.Fatalf("classifyUpstreamFailure(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestClassifyUpstreamFailureMatchesLegacyCountGate 锁定与历史记账判据的等价关系：
// 重构前 recordAccountFailure 的门槛是
// isAccountFailure(status, body) || isTransientThrottle(status, body)，
// 只要分类不是 failureScopeNone 就必须为真，反之亦然。
// 遍历一组枚举状态与代表性正文，避免改动分类时悄悄放宽或收紧记账范围。
func TestClassifyUpstreamFailureMatchesLegacyCountGate(t *testing.T) {
	bodies := []string{
		"",
		"connection reset by peer",
		`{"error":"invalid request body"}`,
		`{"error":"unprocessable entity"}`,
		`{"error":"unauthorized"}`,
		`{"error":"forbidden"}`,
		`{"error":"not found"}`,
		`{"error":"method not allowed"}`,
		`{"error":"too many requests"}`,
		`{"message":"rate limit exceeded"}`,
		`{"message":"insufficient credits"}`,
		`{"message":"payment required"}`,
		"upstream stream closed before first payload",
		"invalid SSE response: missing output and done event",
		`{"choices":[{"message":{"content":"hi"}}]}`,
	}
	statuses := []int{0, 200, 400, 401, 402, 403, 404, 405, 422, 429, 500, 502, 503, 504}

	for _, status := range statuses {
		for _, body := range bodies {
			legacy := isAccountFailure(status, body) || isTransientThrottle(status, body)
			counted := classifyUpstreamFailure(status, body) != failureScopeNone
			if legacy != counted {
				t.Fatalf("记账判据不一致 status=%d body=%q: legacy=%v classified=%v",
					status, body, legacy, counted)
			}
		}
	}
}

// TestRecordAccountFailureHonorsClassification 验证记账入口真的按分类结果执行：
// 400 不入账、429 入账、402 入账，且计数按阶梯递增。
func TestRecordAccountFailureHonorsClassification(t *testing.T) {
	clearFailoverStates()
	t.Cleanup(clearFailoverStates)

	const authID = "auth-class"

	// 1. 请求形状失败不入账，不产生任何状态
	if recordAccountFailure(authID, 400, `{"error":"invalid request body"}`) {
		t.Fatal("400 不应计入冷却")
	}
	if _, _, ok := failoverStateSnapshot(authID); ok {
		t.Fatal("400 不应留下冷却状态")
	}

	// 2. 429 入账并进入冷却
	if !recordAccountFailure(authID, 429, `{"error":"too many requests"}`) {
		t.Fatal("429 应计入冷却")
	}
	count, cooldownUntil, ok := failoverStateSnapshot(authID)
	if !ok || count != 1 || cooldownUntil.IsZero() {
		t.Fatalf("429 应产生 1 次计数与冷却截止时间，got count=%d ok=%v", count, ok)
	}

	// 3. 402 继续累加，验证阶梯复用同一计数
	if !recordAccountFailure(authID, 402, `{"error":"payment required"}`) {
		t.Fatal("402 应计入冷却")
	}
	count, _, _ = failoverStateSnapshot(authID)
	if count != 2 {
		t.Fatalf("连续失败计数应为 2，got %d", count)
	}
}

// TestFailureScopeString 锁定分类名，供日志与面板展示保持一致。
func TestFailureScopeString(t *testing.T) {
	cases := map[failureScope]string{
		failureScopeNone:      "none",
		failureScopeAccount:   "account",
		failureScopeAuthModel: "account_model",
	}
	for scope, want := range cases {
		if got := scope.String(); got != want {
			t.Fatalf("failureScope(%d).String() = %q, want %q", scope, got, want)
		}
	}
}
