// model_circuit_test.go 验证 11128「请求被拦截」的记账落点。
//
// 历史：本文件曾覆盖独立熔断状态机（跨账号聚合、60 秒闸门、窗口清理）。
// 2026-10-20 起重构为「11128 直接写账号×模型冷却」，状态全部由
// accountFailover 持有，因此这里的用例聚焦三件事：
//  1. 判据边界（11128 / 11140 / 6004 不能混淆）；
//  2. 命中后确实写入模型级冷却，路由会跳过该组合；
//  3. 冷却探活成功后才解除，失败则续期——这正是取代熔断的那条闭环。
package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// errFakeProbeFailed 是探活失败的注入错误，用于验证续期路径。
var errFakeProbeFailed = errors.New("fake probe failed")

// TestIsModelBlockedErrorClassifiesUpstreamRiskControl 验证风控错误分类边界：
// 11128 是被拦截、11140 是账号级封禁、6004 是频率限制，三者必须区分开。
func TestIsModelBlockedErrorClassifiesUpstreamRiskControl(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "11128 请求被拦截",
			status: 400,
			body:   `{"code":11128,"msg":"request illegal","displayMsg":{"zh":"请求被拦截，请重新发送"}}`,
			want:   true,
		},
		{
			name:   "11128 以 403 返回同样命中",
			status: 403,
			body:   `{"code":11128,"msg":"request illegal"}`,
			want:   true,
		},
		{
			name:   "11140 账号级封禁必须排除",
			status: 403,
			body:   `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been blocked"}}`,
			want:   false,
		},
		{
			name:   "11140 与 11128 混排时以账号级为准",
			status: 403,
			body:   `{"code":11140,"msg":"request illegal","ref":"11128"}`,
			want:   false,
		},
		{
			name:   "6004 频率限制不属拦截",
			status: 400,
			body:   `{"code":6004,"msg":"usage exceeds frequency limit"}`,
			want:   false,
		},
		{
			name:   "429 不在判定范围",
			status: 429,
			body:   `{"code":11128}`,
			want:   false,
		},
		{
			name:   "空正文不算",
			status: 400,
			body:   "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isModelBlockedError(tc.status, tc.body); got != tc.want {
				t.Fatalf("isModelBlockedError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestClassifyUpstreamFailureBlocks11128AtModelScope 验证 11128 被归到账号×模型
// 冷却，而不是「不记账」。这是整个方案的根因修复点：旧实现把它判成
// failureScopeNone，导致被拦截的组合永远不会冷却、也不会被探活验证。
func TestClassifyUpstreamFailureBlocks11128AtModelScope(t *testing.T) {
	body := `{"code":11128,"msg":"request illegal"}`
	if got := classifyUpstreamFailure(400, body); got != failureScopeAuthModel {
		t.Fatalf("11128 应归入账号×模型冷却, got %v", got)
	}
	// 11140 仍是账号级：封号换模型也不可用。
	if got := classifyUpstreamFailure(403, `{"code":11140}`); got != failureScopeAccount {
		t.Fatalf("11140 应归入账号级冷却, got %v", got)
	}
	// 普通业务 400 不记账：换账号同样失败。
	if got := classifyUpstreamFailure(400, `{"code":1234,"msg":"bad request"}`); got != failureScopeNone {
		t.Fatalf("普通 400 不应记账, got %v", got)
	}
}

// TestNoteModelBlockedHitWritesModelCooldown 验证一次 11128 命中就把该账号的
// 这个模型放进冷却，进而被路由跳过。
func TestNoteModelBlockedHitWritesModelCooldown(t *testing.T) {
	resetFailover(t)

	const (
		authA = "workbuddyai-auth-a.json"
		model = "deepseek-v4.1-flash"
	)

	if !noteModelBlockedHit(authA, model) {
		t.Fatal("命中 11128 应写入冷却")
	}
	if !isAuthModelCoolingDown(authA, model) {
		t.Fatal("写入后该账号应在该模型上处于冷却，路由才会跳过它")
	}
	// 只影响该模型：同账号的其它模型不受牵连。
	if isAuthModelCoolingDown(authA, "glm-5") {
		t.Fatal("11128 只应冷却命中的模型，不应牵连同账号的其它模型")
	}
	// 只影响该账号：其它账号的同模型不受牵连。
	if isAuthModelCoolingDown("workbuddyai-auth-b.json", model) {
		t.Fatal("11128 只应冷却命中账号，不应牵连其它账号")
	}
}

// TestModelBlockedCooldownProbedBeforeExpiry 验证被 11128 冷却的组合会进入
// 冷却探活候选——这是「复用冷却探活」而非另建一套的核心断言。
//
// 旧实现只挑账号级冷却，模型级记录到期后自动失效且从不验证，等于把
// 「是否恢复」交给运气。本用例锁住模型级冷却必须被探到。
func TestModelBlockedCooldownProbedBeforeExpiry(t *testing.T) {
	resetFailover(t)

	const (
		authA = "workbuddyai-auth-a.json"
		model = "deepseek-v4.1-flash"
	)

	noteModelBlockedHit(authA, model)
	_, until, ok := authModelFailoverSnapshot(authA, model)
	if !ok || until.IsZero() {
		t.Fatal("前置条件不成立：应已写入模型级冷却")
	}

	// 1. 远未到提前量窗口：不探测。
	if targets := cooldownProbeTargets(until.Add(-cooldownProbeLeadTime - time.Second)); len(targets) != 0 {
		t.Fatalf("未进入提前量窗口不应探测, got %d 个候选", len(targets))
	}

	// 2. 进入提前量窗口：必须探测，且回放的就是当初失败的模型。
	targets := cooldownProbeTargets(until.Add(-cooldownProbeLeadTime + time.Millisecond))
	if len(targets) != 1 {
		t.Fatalf("进入提前量窗口应探测 1 个候选, got %d", len(targets))
	}
	if targets[0].model != model {
		t.Fatalf("探活必须回放失败的模型 %q, got %q", model, targets[0].model)
	}

	// 3. 已探测过同一周期：不重复探测。
	//    用候选自身的 cooldownUntil 标记，而不是外部快照：冷却状态可能同时
	//    存在原始键与归一化别名两条记录，它们的到期时间未必逐位相同，
	//    拿外部值标记会让去重判定落空（真实缺陷，曾导致用例偶发失败）。
	markCooldownProbed(targets[0].authID, targets[0].cooldownUntil)
	if again := cooldownProbeTargets(until.Add(-cooldownProbeLeadTime + time.Millisecond)); len(again) != 0 {
		t.Fatalf("同一冷却周期不应重复探测, got %d", len(again))
	}
}

// TestProbeSuccessClearsOnlyModelCooldown 验证探活成功只清模型维度：
// 账号级冷却代表账号整体不可用，与单个模型的恢复无关。
func TestProbeSuccessClearsOnlyModelCooldown(t *testing.T) {
	resetFailover(t)

	const (
		authA = "workbuddyai-auth-a.json"
		model = "deepseek-v4.1-flash"
	)

	// 账号级冷却 + 模型级冷却并存。
	recordFailureScoped(authA, "", failureScopeAccount)
	noteModelBlockedHit(authA, model)
	if !isAuthModelCoolingDown(authA, model) {
		t.Fatal("前置条件不成立：应处于冷却")
	}

	// 注入成功的探活。
	stubProbePing(t, func(*traeAuth, string) error { return nil })

	candidate := cooldownProbeCandidate{authID: authA, model: model, scope: failureScopeAuthModel}
	if !probeCoolingAccount(candidate, &traeAuth{}) {
		t.Fatal("探活成功应返回 true")
	}
	// 模型级冷却必须已清除——这是本用例的核心断言。注意不能用
	// isAuthModelCoolingDown 判定：它会先看账号级冷却，而账号级在本例中
	// 刻意保留着，会把「模型级已清」这个事实掩盖掉。
	if _, until, ok := authModelFailoverSnapshot(authA, model); ok && !until.IsZero() {
		t.Fatal("探活成功后应清除该模型的冷却")
	}
	// 账号级冷却必须保留。
	if !isAccountCoolingDown(authA) {
		t.Fatal("探活成功不应清除账号级冷却")
	}
}

// TestProbeFailureExtendsModelCooldown 验证探活失败沿模型维度续期，
// 并且不会误写 test_failed（该标签硬排除、永不自动过期）。
func TestProbeFailureExtendsModelCooldown(t *testing.T) {
	resetFailover(t)

	const (
		authA = "workbuddyai-auth-a.json"
		model = "deepseek-v4.1-flash"
	)

	noteModelBlockedHit(authA, model)
	_, firstUntil, _ := authModelFailoverSnapshot(authA, model)

	stubProbePing(t, func(*traeAuth, string) error { return errFakeProbeFailed })

	candidate := cooldownProbeCandidate{authID: authA, model: model, scope: failureScopeAuthModel}
	if probeCoolingAccount(candidate, &traeAuth{}) {
		t.Fatal("探活失败应返回 false")
	}
	if !isAuthModelCoolingDown(authA, model) {
		t.Fatal("探活失败后应仍在冷却中")
	}
	_, secondUntil, ok := authModelFailoverSnapshot(authA, model)
	if !ok {
		t.Fatal("探活失败后应保留模型级冷却记录")
	}
	if !secondUntil.After(firstUntil) {
		t.Fatalf("探活失败应沿阶梯续期: first=%v second=%v", firstUntil, secondUntil)
	}
	if isTestFailed(authA) {
		t.Fatal("冷却探活失败绝不能写 test_failed 标签")
	}
}

// TestBlockedErrorBodyMarkersCoverProductionShape 用生产实测报文做回归，
// 防止上游文案变化后判定静默失效。
func TestBlockedErrorBodyMarkersCoverProductionShape(t *testing.T) {
	// 生产原样报文（2026-10-18，1,173 次）
	production := `{"code":11128,"msg":"request illegal","requestId":"e1a6043f-92ab-4eb8-b5c0-a1791dea9a24","displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"}}`
	if !isModelBlockedError(400, production) {
		t.Fatal("生产 11128 报文必须被识别为请求被拦截")
	}
	if !strings.Contains(production, "11128") {
		t.Fatal("回归样本本身应包含 11128")
	}
}
