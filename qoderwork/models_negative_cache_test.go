// models_negative_cache_test.go 覆盖周期 P1：模型发现失败负缓存。
//
// 生产 OOM 主因链：callModelsAPI 恒失败 → 不写任何缓存 → 宿主每个
// model.for_auth 事件都重新逐账号全扫（N+1 风暴）。负缓存要求在
// 「全量失败」后开一个窗口，窗口内不再发注定失败的请求，成功即清除。
package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// nestedAuthJSON 构造 parseStored 可识别的嵌套凭据形态。
func nestedAuthJSON(token string) []byte {
	return []byte(`{"auth":{"accessToken":"` + token + `","refreshToken":"jrt-x"},"account":{"uid":"u-neg"}}`)
}

// withFakeCallModelsAPI 替换 callModelsAPIFn 并在用例结束后恢复。
// [参数] fn：注入的假上游实现
func withFakeCallModelsAPI(t *testing.T, fn func(*storedAuth) ([]pluginapi.ModelInfo, error)) {
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

// withFakeFallback 替换存储入口的全量扫描回退，并在用例结束后恢复。
// [参数] fn：注入的回退实现
func withFakeFallback(t *testing.T, fn func() []pluginapi.ModelInfo) {
	t.Helper()
	old := fetchDynamicModelsFallbackFn
	fetchDynamicModelsFallbackFn = fn
	t.Cleanup(func() { fetchDynamicModelsFallbackFn = old })
}

// TestNegativeCache_WindowBlocksRescan 验证「失败后窗口内不再重扫」。
// 变异对照：删除 fetchDynamicModelsFromStorage 的窗口检查 → 第二次调用
// 仍会打到假上游 → calls==2 → 本用例 FAIL。
func TestNegativeCache_WindowBlocksRescan(t *testing.T) {
	resetDynamicModelsCache(t)
	resetDynamicFailCache()
	t.Cleanup(resetDynamicFailCache)

	calls := 0
	withFakeCallModelsAPI(t, func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		calls++
		return nil, fmt.Errorf(`403 {"code":"101","message":"Signature invalid"}`)
	})

	storage := nestedAuthJSON("jt-neg-1")
	if got := fetchDynamicModelsFromStorage(storage); got != nil {
		t.Fatalf("首轮应失败返回 nil，got %d 个模型", len(got))
	}
	if calls != 1 {
		t.Fatalf("首轮应调用上游 1 次，got %d", calls)
	}

	// 第二个账号的 model.for_auth 事件：必须命中窗口，不再打上游。
	if got := fetchDynamicModelsFromStorage(nestedAuthJSON("jt-neg-2")); got != nil {
		t.Fatalf("窗口内应直接返回 nil，got %d 个模型", len(got))
	}
	if calls != 1 {
		t.Fatalf("窗口内不应重扫上游，calls = %d, want 1", calls)
	}
}

// TestNegativeCache_WindowExpires 验证窗口到期后恢复探测能力。
// 变异对照：把窗口判断改成恒真 → 过期后仍拦截 → 本用例 FAIL。
func TestNegativeCache_WindowExpires(t *testing.T) {
	resetDynamicModelsCache(t)
	resetDynamicFailCache()
	t.Cleanup(resetDynamicFailCache)

	calls := 0
	withFakeCallModelsAPI(t, func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		calls++
		return nil, fmt.Errorf("upstream 403")
	})

	fetchDynamicModelsFromStorage(nestedAuthJSON("jt-neg-expire"))
	if calls != 1 {
		t.Fatalf("首轮应调用 1 次，got %d", calls)
	}

	// 把失败时刻回拨到窗口之外，模拟 TTL 过期。
	dynamicModelsCache.Lock()
	dynamicModelsCache.failedAt = time.Now().Add(-dynamicModelsFailCacheTTL - time.Minute)
	dynamicModelsCache.Unlock()

	fetchDynamicModelsFromStorage(nestedAuthJSON("jt-neg-expire"))
	if calls != 2 {
		t.Fatalf("窗口过期后应恢复探测，calls = %d, want 2", calls)
	}
}

// TestNegativeCache_SuccessClearsWindow 验证成功发现即清除负缓存。
// 变异对照：storeDynamicModels 不清 failedAt → 成功后的下一轮被误拦 → FAIL。
func TestNegativeCache_SuccessClearsWindow(t *testing.T) {
	resetDynamicModelsCache(t)
	resetDynamicFailCache()
	t.Cleanup(resetDynamicFailCache)

	calls := 0
	var failNext bool
	withFakeCallModelsAPI(t, func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		calls++
		if failNext {
			return nil, fmt.Errorf("upstream 403")
		}
		return []pluginapi.ModelInfo{{ID: "m-ok", Name: "m-ok"}}, nil
	})

	// 先失败开窗口
	failNext = true
	fetchDynamicModelsFromStorage(nestedAuthJSON("jt-neg-clear"))
	if calls != 1 {
		t.Fatalf("失败轮应调用 1 次，got %d", calls)
	}

	// 成功轮：窗口已被上一轮的 failedAt 挡住，先手动过期，模拟恢复后成功
	dynamicModelsCache.Lock()
	dynamicModelsCache.failedAt = time.Now().Add(-dynamicModelsFailCacheTTL - time.Minute)
	dynamicModelsCache.Unlock()
	failNext = false
	if got := fetchDynamicModelsFromStorage(nestedAuthJSON("jt-neg-clear")); len(got) != 1 {
		t.Fatalf("成功轮应返回 1 个模型，got %d", len(got))
	}
	dynamicModelsCache.RLock()
	failed := dynamicModelsCache.failedAt
	dynamicModelsCache.RUnlock()
	if !failed.IsZero() {
		t.Fatalf("成功后应清除负缓存，failedAt = %v", failed)
	}

	// 清除后再次失败要能重新开窗口。注意先清空正向缓存：成功后的模型列表
	// 本身有效，cachedDynamicModels 会直接命中并短路整条链路（生产正确行为），
	// 不清掉就永远走不到上游，无法验证「二次失败重开窗口」。
	storeDynamicModels(nil)
	failNext = true
	fetchDynamicModelsFromStorage(nestedAuthJSON("jt-neg-clear2"))
	dynamicModelsCache.RLock()
	failed2 := dynamicModelsCache.failedAt
	dynamicModelsCache.RUnlock()
	if failed2.IsZero() {
		t.Fatal("二次失败必须重新开启负缓存窗口")
	}
}

// TestNegativeCache_StorageFallbackRunsBeforeWindow 验证存储入口的
// 「本账号失败 → 回退全量扫描」发生在开启负缓存之前。
//
// 判别方式：回退桩复刻真实全量扫描入口的窗口检查。若实现先开窗再回退，
// 桩会被窗口拦住并返回 nil，本用例即 FAIL。变异对照：把
// markDynamicModelsFailed() 移到 fetchDynamicModelsFallbackFn() 之前 →
// 桩返回 nil → got 为空 → FAIL（原始缺陷为「先 mark 再 return
// fetchDynamicModels()」，等价于窗口自堵）。
func TestNegativeCache_StorageFallbackRunsBeforeWindow(t *testing.T) {
	resetDynamicModelsCache(t)
	resetDynamicFailCache()
	t.Cleanup(resetDynamicFailCache)

	withFakeCallModelsAPI(t, func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		return nil, fmt.Errorf("upstream 403")
	})
	fallbackCalls := 0
	withFakeFallback(t, func() []pluginapi.ModelInfo {
		fallbackCalls++
		if inDynamicModelsFailWindow() {
			return nil
		}
		return []pluginapi.ModelInfo{{ID: "m-from-other-auth", Name: "m-from-other-auth"}}
	})

	got := fetchDynamicModelsFromStorage(nestedAuthJSON("jt-fb-order"))
	if fallbackCalls != 1 {
		t.Fatalf("回退应被调用 1 次，got %d", fallbackCalls)
	}
	if len(got) != 1 || got[0].ID != "m-from-other-auth" {
		t.Fatalf("回退结果应被采用，got %v", got)
	}
}
