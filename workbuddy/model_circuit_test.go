package main

import (
	"strings"
	"testing"
	"time"
)

// resetModelCircuitState 清空熔断状态并在用例结束后复位，保证用例互不干扰。
func resetModelCircuitState(t *testing.T) {
	t.Helper()
	clearModelCircuits()
	t.Cleanup(clearModelCircuits)
}

// TestIsModelCircuitErrorClassifiesUpstreamRiskControl 验证风控错误分类边界：
// 11128 是模型级、11140 是账号级、6004 是频率限制，三者必须区分开。
func TestIsModelCircuitErrorClassifiesUpstreamRiskControl(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "11128 请求被拦截属模型级",
			status: 400,
			body:   `{"code":11128,"msg":"request illegal","displayMsg":{"zh":"请求被拦截，请重新发送"}}`,
			want:   true,
		},
		{
			name:   "11128 以 403 返回同样属模型级",
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
			name:   "6004 频率限制不属模型级",
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
			if got := isModelCircuitError(tc.status, tc.body); got != tc.want {
				t.Fatalf("isModelCircuitError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestModelCircuitOpensOnlyAcrossDistinctAuths 验证熔断按「不同账号数」而非
// 失败次数触发：同一账号反复失败属账号故障，不应升级成模型级熔断。
func TestModelCircuitOpensOnlyAcrossDistinctAuths(t *testing.T) {
	resetModelCircuitState(t)

	const provider = "workbuddy-ai-provider"
	const model = "deepseek-v4.1-flash"

	// 1. 单账号连续命中多次：不熔断
	for i := 0; i < 10; i++ {
		if open := noteModelCircuitHit(provider, model, "auth-a"); open {
			t.Fatalf("同一账号第 %d 次命中不应开启熔断", i+1)
		}
	}
	if isModelCircuitOpen(provider, model) {
		t.Fatal("单账号重复失败不应熔断")
	}

	// 2. 第二个账号命中：仍未达阈值（阈值 3）
	if open := noteModelCircuitHit(provider, model, "auth-b"); open {
		t.Fatal("第二个账号命中不应开启熔断")
	}

	// 3. 第三个账号命中：达到阈值，开启熔断
	if open := noteModelCircuitHit(provider, model, "auth-c"); !open {
		t.Fatal("第三个不同账号命中应开启熔断")
	}
	if !isModelCircuitOpen(provider, model) {
		t.Fatal("跨三个账号命中后应处于熔断状态")
	}

	// 4. 熔断只作用于该 provider × 模型
	if isModelCircuitOpen(provider, "other-model") {
		t.Fatal("熔断不应影响其他模型")
	}
	if isModelCircuitOpen("other-provider", model) {
		t.Fatal("熔断不应影响其他 provider")
	}
}

// TestModelCircuitKeyNormalizesInput 验证聚合键对大小写与空白归一，
// 避免同一模型因写法差异被拆成两个熔断域。
func TestModelCircuitKeyNormalizesInput(t *testing.T) {
	left := modelCircuitKey("WorkBuddy-AI-Provider", "DeepSeek-V4.1-Flash")
	right := modelCircuitKey(" workbuddy-ai-provider ", "deepseek-v4.1-flash")
	if left == "" || left != right {
		t.Fatalf("归一化失败: %q vs %q", left, right)
	}
	if modelCircuitKey("", "m") != "" || modelCircuitKey("p", "") != "" {
		t.Fatal("provider 或 model 为空时应返回空键")
	}
}

// TestModelCircuitResetOnSuccess 验证一次真实成功会清掉熔断残留，
// 避免恢复后抑制窗口继续拦截正常流量。
func TestModelCircuitResetOnSuccess(t *testing.T) {
	resetModelCircuitState(t)

	const provider = "workbuddy-ai-provider"
	const model = "deepseek-v4.1-flash"

	for _, id := range []string{"auth-a", "auth-b", "auth-c"} {
		noteModelCircuitHit(provider, model, id)
	}
	if !isModelCircuitOpen(provider, model) {
		t.Fatal("前置条件不成立：应已熔断")
	}

	resetModelCircuit(provider, model)

	if isModelCircuitOpen(provider, model) {
		t.Fatal("成功后应解除熔断")
	}
	if _, _, ok := modelCircuitSnapshot(provider, model); ok {
		t.Fatal("成功后应清除熔断记录")
	}
}

// TestModelCircuitWindowExpires 验证窗口外的账号记录会被清理，
// 不会让历史失败长期占位导致误熔断。
func TestModelCircuitWindowExpires(t *testing.T) {
	resetModelCircuitState(t)

	const provider = "workbuddy-ai-provider"
	const model = "deepseek-v4.1-flash"
	key := modelCircuitKey(provider, model)

	// 1. 直接注入两条已过期的账号记录
	modelCircuitMu.Lock()
	st := &modelCircuitState{auths: map[string]time.Time{
		"stale-a": time.Now().Add(-2 * modelCircuitWindow),
		"stale-b": time.Now().Add(-2 * modelCircuitWindow),
	}}
	modelCircuitStates[key] = st
	modelCircuitMu.Unlock()

	// 2. 新账号命中时应先清掉过期记录，仅剩 1 个有效账号 → 不熔断
	if open := noteModelCircuitHit(provider, model, "fresh-c"); open {
		t.Fatal("过期账号记录不应计入熔断阈值")
	}
	count, _, ok := modelCircuitSnapshot(provider, model)
	if !ok || count != 1 {
		t.Fatalf("窗口清理后有效账号数 = %d, want 1", count)
	}
}

// TestModelCircuitBlockedTillAdvances 验证熔断期间继续命中会续期，
// 使持续拦截的模型不会在 60 秒后立刻放行一次再失败。
func TestModelCircuitBlockedTillAdvances(t *testing.T) {
	resetModelCircuitState(t)

	const provider = "workbuddy-ai-provider"
	const model = "deepseek-v4.1-flash"

	for _, id := range []string{"auth-a", "auth-b", "auth-c"} {
		noteModelCircuitHit(provider, model, id)
	}
	_, firstTill, _ := modelCircuitSnapshot(provider, model)

	// Windows 时间分辨率下连续两次 Now() 可能取到同一刻度，
	// 先推进时间再命中，才能稳定观察到续期效果。
	time.Sleep(2 * time.Millisecond)
	noteModelCircuitHit(provider, model, "auth-d")
	_, secondTill, _ := modelCircuitSnapshot(provider, model)

	if !secondTill.After(firstTill) {
		t.Fatalf("熔断期间命中应续期: first=%v second=%v", firstTill, secondTill)
	}
	if !isModelCircuitOpen(provider, model) {
		t.Fatal("续期后应仍处于熔断状态")
	}
}

// TestModelCircuitErrorBodyMarkersCoverProductionShape 用生产实测报文做回归，
// 防止上游文案变化后分类静默失效。
func TestModelCircuitErrorBodyMarkersCoverProductionShape(t *testing.T) {
	// 生产原样报文（2026-10-18，1,173 次）
	production := `{"code":11128,"msg":"request illegal","requestId":"e1a6043f-92ab-4eb8-b5c0-a1791dea9a24","displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"}}`
	if !isModelCircuitError(400, production) {
		t.Fatal("生产 11128 报文必须被识别为模型级风控")
	}
	if !strings.Contains(production, "11128") {
		t.Fatal("回归样本本身应包含 11128")
	}
}
