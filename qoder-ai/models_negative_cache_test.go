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
// uid 参与缓存分片，因此每个用例用独立 uid 表示独立账号。
func nestedAuthJSON(token string) []byte {
	return []byte(`{"auth":{"accessToken":"` + token + `","refreshToken":"jrt-x"},"account":{"uid":"` + token + `"}}`)
}

// withFakeCallModelsAPI 替换 callModelsAPIFn 并在用例结束后恢复。
// [参数] fn：注入的假上游实现
func withFakeCallModelsAPI(t *testing.T, fn func(*storedAuth) ([]pluginapi.ModelInfo, error)) {
	t.Helper()
	old := callModelsAPIFn
	callModelsAPIFn = fn
	t.Cleanup(func() { callModelsAPIFn = old })
}

// withFakeFallback 替换存储入口的全量扫描回退，并在用例结束后恢复。
// [参数] fn：注入的回退实现
func withFakeFallback(t *testing.T, fn func() []pluginapi.ModelInfo) {
	t.Helper()
	old := fetchDynamicModelsFallbackFn
	fetchDynamicModelsFallbackFn = fn
	t.Cleanup(func() { fetchDynamicModelsFallbackFn = old })
}

// withFakeHostAuthListFiles 替换账号列表来源，并在用例结束后恢复。
// [参数] files：注入的账号列表；每个条目的 AuthIndex 直接作为凭据 uid，
// 使全量扫描链路（列表 → 取凭据 → 调上游）在 shim 下完整跑通。
func withFakeHostAuthListFiles(t *testing.T, files []pluginapi.HostAuthFileEntry) {
	t.Helper()
	oldList := hostAuthListFilesFn
	oldGet := hostAuthGetByIndexFn
	hostAuthListFilesFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return files, nil
	}
	hostAuthGetByIndexFn = func(authIndex string) ([]byte, error) {
		return nestedAuthJSON(authIndex), nil
	}
	t.Cleanup(func() {
		hostAuthListFilesFn = oldList
		hostAuthGetByIndexFn = oldGet
	})
}

// TestNegativeCache_WindowBlocksRescan 验证「同一账号失败后窗口内不再重扫」。
// 变异对照：删除 fetchDynamicModelsFromStorage 的窗口检查 → 第二次调用
// 仍会打到假上游 → calls==2 → 本用例 FAIL。
func TestNegativeCache_WindowBlocksRescan(t *testing.T) {
	resetDynamicModelsCache(t)

	calls := 0
	withFakeCallModelsAPI(t, func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		calls++
		return nil, fmt.Errorf(`403 {"code":"101","message":"Signature invalid"}`)
	})
	// 回退桩返回空，模拟「没有其它账号可救」——否则本账号失败会先被
	// 全量扫描回退接走，走不到开窗分支。
	withFakeFallback(t, func() []pluginapi.ModelInfo { return nil })

	storage := nestedAuthJSON("u-neg-1")
	if got := fetchDynamicModelsFromStorage("u-neg-1", storage); got != nil {
		t.Fatalf("首轮应失败返回 nil，got %d 个模型", len(got))
	}
	if calls != 1 {
		t.Fatalf("首轮应调用上游 1 次，got %d", calls)
	}

	// 同一账号的第二次 model.for_auth 事件：必须命中该账号的窗口，不再打上游。
	if got := fetchDynamicModelsFromStorage("u-neg-1", storage); got != nil {
		t.Fatalf("窗口内应直接返回 nil，got %d 个模型", len(got))
	}
	if calls != 1 {
		t.Fatalf("窗口内不应重扫上游，calls = %d, want 1", calls)
	}
}

// TestNegativeCache_WindowIsPerAccount 验证负缓存按账号隔离：A 账号失败
// 不得连带封锁 B 账号的发现。
//
// 变异对照：把 inDynamicModelsFailWindow 改回全局判定 →
// B 账号会被 A 的窗口拦住 → calls 停在 1 → 本用例 FAIL。
func TestNegativeCache_WindowIsPerAccount(t *testing.T) {
	resetDynamicModelsCache(t)

	calls := map[string]int{}
	withFakeCallModelsAPI(t, func(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
		calls[sa.Account.UID]++
		if sa.Account.UID == "u-acct-a" {
			return nil, fmt.Errorf("upstream 403")
		}
		return []pluginapi.ModelInfo{{ID: "b-model"}}, nil
	})
	withFakeFallback(t, func() []pluginapi.ModelInfo { return nil })

	fetchDynamicModelsFromStorage("u-acct-a", nestedAuthJSON("u-acct-a"))
	if calls["u-acct-a"] != 1 {
		t.Fatalf("A 账号应调用 1 次，got %d", calls["u-acct-a"])
	}

	got := fetchDynamicModelsFromStorage("u-acct-b", nestedAuthJSON("u-acct-b"))
	if calls["u-acct-b"] != 1 {
		t.Fatalf("B 账号不应被 A 的负缓存拦截，got %d 次调用", calls["u-acct-b"])
	}
	if len(got) != 1 || got[0].ID != "b-model" {
		t.Fatalf("B 账号应拿到自己的清单，got %v", got)
	}
}

// TestNegativeCache_CacheIsPerAccount 验证模型缓存按账号隔离：
// 两个账号各自的清单互不覆盖。
//
// 变异对照：把 cachedDynamicModels 改回全局单例 → B 账号读到 A 的清单 →
// 断言 B 拿到 b-model 时 FAIL。
func TestNegativeCache_CacheIsPerAccount(t *testing.T) {
	resetDynamicModelsCache(t)

	withFakeCallModelsAPI(t, func(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
		if sa.Account.UID == "u-cache-a" {
			return []pluginapi.ModelInfo{{ID: "a1"}, {ID: "a2"}}, nil
		}
		return []pluginapi.ModelInfo{{ID: "b1"}}, nil
	})

	gotA := fetchDynamicModelsFromStorage("u-cache-a", nestedAuthJSON("u-cache-a"))
	if len(gotA) != 2 {
		t.Fatalf("A 账号应拿到 2 个模型，got %v", gotA)
	}
	gotB := fetchDynamicModelsFromStorage("u-cache-b", nestedAuthJSON("u-cache-b"))
	if len(gotB) != 1 || gotB[0].ID != "b1" {
		t.Fatalf("B 账号不得读到 A 的缓存，got %v", gotB)
	}

	// A 的缓存不得被 B 覆盖：再次读 A 仍是自己的 2 个。
	if again, ok := cachedDynamicModels("u-cache-a"); !ok || len(again) != 2 {
		t.Fatalf("A 的缓存被 B 覆盖，got %v ok=%v", again, ok)
	}
}

// TestNegativeCache_WindowExpires 验证窗口到期后恢复探测能力。
// 变异对照：把窗口判断改成恒真 → 过期后仍拦截 → 本用例 FAIL。
func TestNegativeCache_WindowExpires(t *testing.T) {
	resetDynamicModelsCache(t)

	calls := 0
	withFakeCallModelsAPI(t, func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		calls++
		return nil, fmt.Errorf("upstream 403")
	})
	withFakeFallback(t, func() []pluginapi.ModelInfo { return nil })

	storage := nestedAuthJSON("u-neg-expire")
	fetchDynamicModelsFromStorage("u-neg-expire", storage)
	if calls != 1 {
		t.Fatalf("首轮应调用 1 次，got %d", calls)
	}

	// 把该账号的失败时刻回拨到窗口之外，模拟 TTL 过期。
	dynamicModelsMu.Lock()
	dynamicModelsFails["u-neg-expire"] = &dynamicModelsFailEntry{
		failedAt: time.Now().Add(-dynamicModelsFailCacheTTL - time.Minute),
	}
	dynamicModelsMu.Unlock()

	fetchDynamicModelsFromStorage("u-neg-expire", storage)
	if calls != 2 {
		t.Fatalf("窗口过期后应恢复探测，calls = %d, want 2", calls)
	}
}

// TestNegativeCache_SuccessClearsWindow 验证成功发现即清除该账号的负缓存。
// 变异对照：storeDynamicModels 不清 failedAt → 成功后的下一轮被误拦 → FAIL。
func TestNegativeCache_SuccessClearsWindow(t *testing.T) {
	resetDynamicModelsCache(t)

	calls := 0
	var failNext bool
	withFakeCallModelsAPI(t, func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		calls++
		if failNext {
			return nil, fmt.Errorf("upstream 403")
		}
		return []pluginapi.ModelInfo{{ID: "m-ok", Name: "m-ok"}}, nil
	})
	withFakeFallback(t, func() []pluginapi.ModelInfo { return nil })

	const acct = "u-neg-clear"
	storage := nestedAuthJSON(acct)

	// 先失败开窗口
	failNext = true
	fetchDynamicModelsFromStorage(acct, storage)
	if calls != 1 {
		t.Fatalf("失败轮应调用 1 次，got %d", calls)
	}

	// 成功轮：窗口已被上一轮挡住，先手动过期，模拟恢复后成功
	dynamicModelsMu.Lock()
	dynamicModelsFails[acct] = &dynamicModelsFailEntry{
		failedAt: time.Now().Add(-dynamicModelsFailCacheTTL - time.Minute),
	}
	dynamicModelsMu.Unlock()
	failNext = false
	if got := fetchDynamicModelsFromStorage(acct, storage); len(got) != 1 {
		t.Fatalf("成功轮应返回 1 个模型，got %d", len(got))
	}
	dynamicModelsMu.RLock()
	_, stillFailed := dynamicModelsFails[acct]
	dynamicModelsMu.RUnlock()
	if stillFailed {
		t.Fatal("成功后应清除该账号的负缓存")
	}

	// 清除后再次失败要能重新开窗口。注意先清空正向缓存：成功后的模型列表
	// 本身有效，cachedDynamicModels 会直接命中并短路整条链路（生产正确行为），
	// 不清掉就永远走不到上游，无法验证「二次失败重开窗口」。
	dynamicModelsMu.Lock()
	delete(dynamicModelsCache, acct)
	dynamicModelsMu.Unlock()
	failNext = true
	fetchDynamicModelsFromStorage(acct, storage)
	dynamicModelsMu.RLock()
	_, reopened := dynamicModelsFails[acct]
	dynamicModelsMu.RUnlock()
	if !reopened {
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

	withFakeCallModelsAPI(t, func(*storedAuth) ([]pluginapi.ModelInfo, error) {
		return nil, fmt.Errorf("upstream 403")
	})
	fallbackCalls := 0
	withFakeFallback(t, func() []pluginapi.ModelInfo {
		fallbackCalls++
		if allDynamicModelsFailed() {
			return nil
		}
		return []pluginapi.ModelInfo{{ID: "m-from-other-auth", Name: "m-from-other-auth"}}
	})

	got := fetchDynamicModelsFromStorage("u-fb-order", nestedAuthJSON("u-fb-order"))
	if fallbackCalls != 1 {
		t.Fatalf("回退应被调用 1 次，got %d", fallbackCalls)
	}
	if len(got) != 1 || got[0].ID != "m-from-other-auth" {
		t.Fatalf("回退结果应被采用，got %v", got)
	}
}

// TestFetchDynamicModels_TriesHealthyAccountFirst 验证全量扫描真的按额度优先
// 遍历：耗尽账号排在文件名最前也不应被优先尝试。
//
// 与 TestSortAuthsByCredits_HealthyFirst 的区别：那个只测排序函数本身，
// 本用例走 fetchDynamicModels 全链路（注入账号列表与假上游），覆盖
// 「排序是否真的参与了遍历」。变异对照：删掉 fetchDynamicModels 里的
// sortAuthsByCredits 调用 → 耗尽账号仍排第一 → 首个成功来自耗尽账号 →
// got 2 个模型 → FAIL（此变异曾被纯排序用例放过，属真实测试缺口）。
func TestFetchDynamicModels_TriesHealthyAccountFirst(t *testing.T) {
	resetDynamicModelsCache(t)

	// 耗尽账号在名单里排第一（模拟生产 070a9a... 的位置）。
	accountCache.Store("u-scan-exhausted", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 0, TotalUsed: 100, TotalSize: 100},
	})
	accountCache.Store("u-scan-healthy", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 500, TotalUsed: 0, TotalSize: 500},
	})
	t.Cleanup(func() {
		accountCache.Delete("u-scan-exhausted")
		accountCache.Delete("u-scan-healthy")
	})

	// AuthIndex 即假凭据的 uid，供 withFakeHostAuthListFiles 构造可解析凭据。
	withFakeHostAuthListFiles(t, []pluginapi.HostAuthFileEntry{
		{ID: "u-scan-exhausted", AuthIndex: "u-scan-exhausted", Name: "qoderai-u-scan-exhausted.json"},
		{ID: "u-scan-healthy", AuthIndex: "u-scan-healthy", Name: "qoderai-u-scan-healthy.json"},
	})
	var order []string
	withFakeCallModelsAPI(t, func(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
		order = append(order, sa.Account.UID)
		if sa.Account.UID == "u-scan-healthy" {
			return make([]pluginapi.ModelInfo, 14), nil
		}
		return make([]pluginapi.ModelInfo, 2), nil
	})

	got := fetchDynamicModels()
	if len(order) == 0 {
		t.Fatal("扫描未触达任何账号")
	}
	if order[0] != "u-scan-healthy" {
		t.Fatalf("首次尝试应为健康账号，实际顺序 %v", order)
	}
	if len(got) != 14 {
		t.Fatalf("应取健康账号的 14 个模型，got %d", len(got))
	}
}

// TestSortAuthsByCredits_HealthyFirst 验证模型发现的排序档位：
// 有额度 > 未测 > 耗尽，同档保持入参顺序。
//
// 变异对照：改坏 modelDiscoveryRank 的档位映射 → 断言首位/末位时 FAIL。
// 注意本用例只覆盖排序函数本身；「fetchDynamicModels 是否调用它」由
// TestFetchDynamicModels_TriesHealthyAccountFirst 覆盖。
func TestSortAuthsByCredits_HealthyFirst(t *testing.T) {
	resetDynamicModelsCache(t)

	// 直接写入积分快照：耗尽=0、健康=500、未测=不写。
	accountCache.Store("u-exhausted", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 0, TotalUsed: 100, TotalSize: 100},
	})
	accountCache.Store("u-healthy", &accountCacheEntry{
		credits: &creditsSummary{TotalRemain: 500, TotalUsed: 0, TotalSize: 500},
	})
	t.Cleanup(func() {
		accountCache.Delete("u-exhausted")
		accountCache.Delete("u-healthy")
	})

	files := []pluginapi.HostAuthFileEntry{
		{ID: "u-exhausted", AuthIndex: "idx-1", Name: "qoderai-u-exhausted.json"},
		{ID: "u-unknown", AuthIndex: "idx-2", Name: "qoderai-u-unknown.json"},
		{ID: "u-healthy", AuthIndex: "idx-3", Name: "qoderai-u-healthy.json"},
	}
	got := sortAuthsByCredits(files)
	if len(got) != 3 {
		t.Fatalf("排序不得增删元素，got %d", len(got))
	}
	if got[0].ID != "u-healthy" {
		t.Fatalf("首位应为有额度账号，got %q", got[0].ID)
	}
	if got[1].ID != "u-unknown" {
		t.Fatalf("次位应为未测账号，got %q", got[1].ID)
	}
	if got[2].ID != "u-exhausted" {
		t.Fatalf("末位应为耗尽账号，got %q", got[2].ID)
	}
	// 入参不得被就地改写。
	if files[0].ID != "u-exhausted" {
		t.Fatalf("排序不得修改入参，got %q", files[0].ID)
	}
}
