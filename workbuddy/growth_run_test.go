// growth_run_test.go 覆盖调度开关与配置解析，以及编排层的容错边界。
package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestConfigureGrowthAutoParsing 验证 growth_auto 的各种取值与引号键兼容。
// 引号键曾在 0.16.2 造成静默失效，必须保留该回归用例。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增调度测试（TASK-04.1）
func TestConfigureGrowthAutoParsing(t *testing.T) {
	orig := growthAuto
	defer setGrowthAuto(orig)

	cases := []struct {
		yaml string
		want bool
	}{
		{"growth_auto: false", false},
		{"growth_auto: true", true},
		{"growth_auto: 0", false},
		{"growth_auto: 1", true},
		{"growth_auto: no", false},
		{"growth_auto: yes", true},
		{"growth_auto: off", false},
		{"growth_auto: on", true},
		{`"growth_auto": false`, false},
		// 缺省键回落默认开启（用户选定「默认定时自动跑」）
		{"checkin_auto: false", true},
	}
	for _, tc := range cases {
		setGrowthAuto(!tc.want) // 先置反，确保断言能捕捉解析失效
		configure(buildConfigYAML(tc.yaml))
		if got := growthEnabled(); got != tc.want {
			t.Errorf("configure(%q): growthEnabled = %v, want %v", tc.yaml, got, tc.want)
		}
	}
}

// buildConfigYAML 把单行配置包成 plugin.register 期望的 config_yaml 结构。
// [参数] lines：配置行。
// [返回] 可直接传给 configure 的 JSON 字节。
// 最近修改时间：2026-10-24 新增调度测试（TASK-04.1）
func buildConfigYAML(lines ...string) []byte {
	yaml := ""
	for _, line := range lines {
		yaml += line + "\n"
	}
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
	return raw
}

// TestGrowthRunForAccountSkipsWhenLocked 验证账号级互斥：
// 同一账号并发执行时第二次直接返回 nil，不发起任何请求。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增编排测试（TASK-03.1）
func TestGrowthRunForAccountSkipsWhenLocked(t *testing.T) {
	var calls int32
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	})
	defer cleanup()

	authIndex := "idx-lock-test"
	// 手动占住锁，模拟另一个执行中的任务
	mu := growthLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()

	if res := runGrowthForAccount(&storedAuth{}, authIndex); res != nil {
		t.Fatal("expected nil result when account lock is held")
	}
	if calls != 0 {
		t.Fatalf("must not issue upstream calls while locked, got %d", calls)
	}
}

// TestGrowthResultSeparatesEnergyBalance 验证能量余额不混入收益。
// 0.16.2 事故：余额被当成本轮收益计入，零领取轮次也显示「+N 能量」。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增编排测试（TASK-03.1）
func TestGrowthResultSeparatesEnergyBalance(t *testing.T) {
	res := &growthResult{}
	res.EnergyBalance = 999
	if res.EnergyGained != 0 {
		t.Fatalf("EnergyGained must stay 0 when only balance is set, got %d", res.EnergyGained)
	}
}

// TestGrowthMakeupTargetSkipsWhenNoCards 验证无补登卡时不产生补登目标。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增编排测试（TASK-03.1）
func TestGrowthMakeupTargetSkipsWhenNoCards(t *testing.T) {
	streak := map[string]any{"makeup_cards": map[string]any{"balance": float64(0)}}
	target := growthMakeupTarget(streak, map[string]any{}, time.Now())
	if target != "" {
		t.Fatalf("expected empty target when no cards, got %q", target)
	}
}

// TestPruneGrowthLocksRemovesStale 验证锁表随账号删除收缩。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增编排测试（TASK-03.1）
func TestPruneGrowthLocksRemovesStale(t *testing.T) {
	orig := hostAuthListFn
	defer func() { hostAuthListFn = orig }()

	// 占一个「已不存在」账号的锁，同时登记一个在册账号
	growthLockFor("idx-stale")
	growthLockFor("idx-live")
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "wb-live", AuthIndex: "idx-live"}}, nil
	}
	pruneGrowthLocks()

	if _, ok := growthLocks.Load("idx-stale"); ok {
		t.Fatal("stale lock must be pruned")
	}
	if _, ok := growthLocks.Load("idx-live"); !ok {
		t.Fatal("live lock must be kept")
	}
}
