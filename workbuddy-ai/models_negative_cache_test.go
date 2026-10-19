// models_negative_cache_test.go 覆盖周期 P1：模型发现失败负缓存（workbuddy-ai 形态）。
//
// 与 workbuddy 同一防风暴不变量：全量失败后开窗口，窗口内入口直接返回 nil，
// 不再向上游发起注定失败的请求；成功即清除窗口。workbuddy-ai 的动态发现
// 入参带企业 ID（accessToken + enterpriseID），失败标记点在被拒绝的调用返回处。
package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// withFakeCallModelsAPI 替换 callModelsAPIFn 并在用例结束后恢复。
// [参数] fn：注入的假上游实现（workbuddy-ai 形态：入参 accessToken + enterpriseID）
func withFakeCallModelsAPI(t *testing.T, fn func(string, string) ([]pluginapi.ModelInfo, error)) {
	t.Helper()
	old := callModelsAPIFn
	callModelsAPIFn = fn
	t.Cleanup(func() { callModelsAPIFn = old })
}

// resetDynamicFailCache 清空负缓存时间戳，避免用例间串扰。
func resetDynamicFailCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.failedAt = time.Time{}
	dynamicModelsCache.Unlock()
}

// resetDynamicCache 同时清空正向与负向动态缓存，供每个用例建立干净起点。
func resetDynamicCache(t *testing.T) {
	t.Helper()
	storeDynamicModels(nil)
	resetDynamicFailCache()
	t.Cleanup(func() {
		storeDynamicModels(nil)
		resetDynamicFailCache()
	})
}

// TestNegativeCache_WindowBlocksRescan 验证「失败后窗口内不再重扫」。
// 变异对照：删除 fetchDynamicModelsFromStorage 的窗口检查 → 第二次调用
// 仍会打到假上游 → calls==2 → 本用例 FAIL。
func TestNegativeCache_WindowBlocksRescan(t *testing.T) {
	resetDynamicCache(t)

	calls := 0
	withFakeCallModelsAPI(t, func(string, string) ([]pluginapi.ModelInfo, error) {
		calls++
		return nil, fmt.Errorf("upstream 403")
	})

	if got := fetchDynamicModelsFromStorage([]byte(`{"accessToken":"wa-neg-1"}`)); got != nil {
		t.Fatalf("首轮应失败返回 nil，got %d 个模型", len(got))
	}
	if calls != 1 {
		t.Fatalf("首轮应调用上游 1 次，got %d", calls)
	}

	if got := fetchDynamicModelsFromStorage([]byte(`{"accessToken":"wa-neg-2"}`)); got != nil {
		t.Fatalf("窗口内应直接返回 nil，got %d 个模型", len(got))
	}
	if calls != 1 {
		t.Fatalf("窗口内不应重扫上游，calls = %d, want 1", calls)
	}
}

// TestNegativeCache_WindowExpires 验证窗口到期后恢复探测能力。
// 变异对照：把窗口判断改成恒真 → 过期后仍拦截 → 本用例 FAIL。
func TestNegativeCache_WindowExpires(t *testing.T) {
	resetDynamicCache(t)

	calls := 0
	withFakeCallModelsAPI(t, func(string, string) ([]pluginapi.ModelInfo, error) {
		calls++
		return nil, fmt.Errorf("upstream 403")
	})

	fetchDynamicModelsFromStorage([]byte(`{"accessToken":"wa-neg-expire"}`))
	if calls != 1 {
		t.Fatalf("首轮应调用 1 次，got %d", calls)
	}

	dynamicModelsCache.Lock()
	dynamicModelsCache.failedAt = time.Now().Add(-dynamicModelsFailCacheTTL - time.Minute)
	dynamicModelsCache.Unlock()

	fetchDynamicModelsFromStorage([]byte(`{"accessToken":"wa-neg-expire"}`))
	if calls != 2 {
		t.Fatalf("窗口过期后应恢复探测，calls = %d, want 2", calls)
	}
}

// TestNegativeCache_SuccessClearsWindow 验证成功发现即清除负缓存。
// 变异对照：storeDynamicModels 不清 failedAt → 成功后的下一轮被误拦 → FAIL。
func TestNegativeCache_SuccessClearsWindow(t *testing.T) {
	resetDynamicCache(t)

	calls := 0
	var failNext bool
	withFakeCallModelsAPI(t, func(string, string) ([]pluginapi.ModelInfo, error) {
		calls++
		if failNext {
			return nil, fmt.Errorf("upstream 403")
		}
		return []pluginapi.ModelInfo{{ID: "m-ok", Name: "m-ok"}}, nil
	})

	failNext = true
	fetchDynamicModelsFromStorage([]byte(`{"accessToken":"wa-neg-clear"}`))
	if calls != 1 {
		t.Fatalf("失败轮应调用 1 次，got %d", calls)
	}

	dynamicModelsCache.Lock()
	dynamicModelsCache.failedAt = time.Now().Add(-dynamicModelsFailCacheTTL - time.Minute)
	dynamicModelsCache.Unlock()
	failNext = false
	if got := fetchDynamicModelsFromStorage([]byte(`{"accessToken":"wa-neg-clear"}`)); len(got) != 1 {
		t.Fatalf("成功轮应返回 1 个模型，got %d", len(got))
	}
	dynamicModelsCache.RLock()
	failed := dynamicModelsCache.failedAt
	dynamicModelsCache.RUnlock()
	if !failed.IsZero() {
		t.Fatalf("成功后应清除负缓存，failedAt = %v", failed)
	}

	// 清除后再次失败要能重新开窗口。先清空正向缓存：成功后的模型列表本身
	// 有效，cachedDynamicModels 会直接命中并短路整条链路（生产正确行为）。
	storeDynamicModels(nil)
	failNext = true
	fetchDynamicModelsFromStorage([]byte(`{"accessToken":"wa-neg-clear2"}`))
	dynamicModelsCache.RLock()
	failed2 := dynamicModelsCache.failedAt
	dynamicModelsCache.RUnlock()
	if failed2.IsZero() {
		t.Fatal("二次失败必须重新开启负缓存窗口")
	}
}
