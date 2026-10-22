// model_circuit_test.go 验证 11128「请求被拦截」的记账落点。
//
// 历史：本文件曾覆盖独立熔断状态机（跨账号聚合、60 秒闸门、窗口清理）。
// 2026-10-20 起重构为「11128 直接写账号×模型冷却」，状态全部由
// accountFailover 持有。2026-10-22 起该落点改为冷却 + 禁用双写，
// 恢复由 10 分钟禁用探活裁决（冷却探活已移除），因此这里的用例聚焦三件事：
//  1. 判据边界（11128 / 11140 / 6004 不能混淆）；
//  2. 命中后确实写入模型级冷却，路由会跳过该组合；
//  3. 命中同时禁用账号 —— 宿主选择器读不到插件内存，只有写盘才能挡住同请求轮转。
package main

import (
	"errors"
	"strings"
	"testing"
)

// errFakeProbeFailed 是探活失败的注入错误。
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
	newFailoverFakeHost(t)

	const (
		authA = "traework-auth-a.json"
		model = "claude-3-5-sonnet"
	)

	if !noteModelBlockedHit(authA, model) {
		t.Fatal("命中 11128 应写入冷却")
	}
	if !isAuthModelCoolingDown(authA, model) {
		t.Fatal("写入后该账号应在该模型上处于冷却，路由才会跳过它")
	}
	// 只影响该模型：同账号的其它模型不受牵连（冷却侧）。
	if isAuthModelCoolingDown(authA, "gpt-5") {
		t.Fatal("11128 只应冷却命中的模型，不应牵连同账号的其它模型")
	}
	// 只影响该账号：其它账号的同模型不受牵连。
	if isAuthModelCoolingDown("traework-auth-b.json", model) {
		t.Fatal("11128 只应冷却命中账号，不应牵连其它账号")
	}
}

// TestNoteModelBlockedHitAlsoDisablesAccountOnDisk 锁定双写的禁用侧：
// 11128 命中的账号必须同时被写盘禁用，否则宿主选择器（读不到插件内存冷却）
// 仍会把后续请求派到这个已知会被拦截的组合上。
//
// 用户裁定：模型级拦截也禁用整个账号，接受精度损失。
func TestNoteModelBlockedHitAlsoDisablesAccountOnDisk(t *testing.T) {
	resetFailover(t)
	h := newFailoverFakeHost(t)
	h.addAccount("trae-11128", "idx-mb", nil)

	const model = "claude-3-5-sonnet"
	if !noteModelBlockedHit("trae-11128", model) {
		t.Fatal("命中 11128 应记账成功")
	}
	doc := h.docOf("idx-mb")
	if doc["disabled"] != true || doc[failoverDisableKey] != true {
		t.Fatalf("11128 必须同时禁用整个账号: %v", doc)
	}
	// 探活要回放命中的那个模型，所以禁用模型键必须写成它。
	if doc[failoverDisableModelKey] != model {
		t.Fatalf("禁用模型 = %v, want %s", doc[failoverDisableModelKey], model)
	}
	if !isAccountCoolingDown("trae-11128") {
		t.Fatal("禁用后账号级排除必须成立（双写两侧都要生效）")
	}
}

// TestModelBlockedDisableProbeReplaysFailedModel 验证被 11128 禁用的账号会进入
// 禁用探活候选，且候选带着失败的那个模型——这就是探活唯一会发出去的目标。
func TestModelBlockedDisableProbeReplaysFailedModel(t *testing.T) {
	resetFailover(t)
	h := newFailoverFakeHost(t)
	h.addAccount("trae-probe", "idx-p", nil)

	const model = "claude-3-5-sonnet"
	noteModelBlockedHit("trae-probe", model)

	targets := failoverProbeTargets()
	if len(targets) != 1 {
		t.Fatalf("11128 禁用的账号必须进入探活候选, got %d", len(targets))
	}
	if targets[0].model != model {
		t.Fatalf("探活必须回放失败的模型 %q, got %q", model, targets[0].model)
	}

	// 探活实际发出的模型必须与候选一致，且只有这一个。
	var pinged []string
	failoverPingFn = func(_ *traeAuth, m string) error {
		pinged = append(pinged, m)
		return errFakeProbeFailed
	}
	if probed := runFailoverProbeTick(func(string) *traeAuth { return &traeAuth{} }); probed != 1 {
		t.Fatalf("应探测 1 个账号, got %d", probed)
	}
	if len(pinged) != 1 || pinged[0] != model {
		t.Fatalf("只应回放失败模型 %q, got %v", model, pinged)
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
