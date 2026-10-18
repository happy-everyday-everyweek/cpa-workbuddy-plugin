// alias_resolution_test.go 覆盖执行器的 oauth-model-alias 反解接线。
//
// 缺陷背景（2026-10-18 生产实证）：宿主只按别名做路由匹配，传给执行器的
// req.Model 仍是客户端原始别名；上游只认真实模型 ID，透传别名会被判为
// invalid_request（400，param=None）。面板「测试」按钮直接指定真实 ID 故成功，
// 经 new-api 走别名则失败——两条路径的差异掩盖了该缺陷。
package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// setModelAliasCache 用给定别名表替换全局缓存并在用例结束后清空。
// 缓存是跨用例共享的全局状态，必须显式重置，避免测试间相互污染。
func setModelAliasCache(t *testing.T, byAlias map[string]string) {
	t.Helper()
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
	t.Cleanup(func() {
		modelAliasCache.Lock()
		modelAliasCache.byAlias = nil
		modelAliasCache.Unlock()
	})
}

// TestResolveUpstreamModel_HostAliasCache 验证宿主级 oauth-model-alias 缓存通道。
// 这是 cacheModelAliases 填充的通道，与 auth 属性通道相互独立。
func TestResolveUpstreamModel_HostAliasCache(t *testing.T) {
	setModelAliasCache(t, map[string]string{
		"qwen-3.8-max": "qwen3.8-max-preview",
		"qwen-3.8":     "qwen3.8-max-preview",
		"qwen3.8-max":  "qwen3.8-max-preview",
	})

	// 1. 三条生产别名都必须反解到真实上游 ID。
	for _, alias := range []string{"qwen-3.8-max", "qwen-3.8", "qwen3.8-max"} {
		if got := resolveUpstreamModel(alias, nil); got != "qwen3.8-max-preview" {
			t.Errorf("resolveUpstreamModel(%q) = %q, want qwen3.8-max-preview", alias, got)
		}
	}
	// 2. 大小写不敏感：缓存键按小写归一。
	if got := resolveUpstreamModel("QWEN-3.8-MAX", nil); got != "qwen3.8-max-preview" {
		t.Errorf("case-insensitive alias resolution = %q, want qwen3.8-max-preview", got)
	}
	// 3. 真实 ID 不受影响（不在别名表内，原样返回）。
	if got := resolveUpstreamModel("qwen3.8-max-preview", nil); got != "qwen3.8-max-preview" {
		t.Errorf("real model id must pass through, got %q", got)
	}
	// 4. 未知模型原样返回，不得凭空改写。
	if got := resolveUpstreamModel("unknown-model", nil); got != "unknown-model" {
		t.Errorf("unknown model must pass through, got %q", got)
	}
}

// TestResolveUpstreamModel_AuthAttributeWins 验证 auth 属性优先级高于宿主缓存。
func TestResolveUpstreamModel_AuthAttributeWins(t *testing.T) {
	setModelAliasCache(t, map[string]string{"shared-alias": "from-host-cache"})

	attrs := map[string]string{"model_alias": "shared-alias=from-auth-attribute"}
	if got := resolveUpstreamModel("shared-alias", attrs); got != "from-auth-attribute" {
		t.Errorf("auth attribute must win over host cache, got %q", got)
	}
}

// TestCacheModelAliases_ReadsHostSummary 验证 cacheModelAliases 能从宿主摘要
// 填充缓存，并跳过 name==alias 与空值条目。
func TestCacheModelAliases_ReadsHostSummary(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		OAuthModelAlias: map[string][]pluginapi.ModelAlias{
			providerName: {
				{Name: "qwen3.8-max-preview", Alias: "qwen-3.8-max"},
				{Name: "qwen3.8-max-preview", Alias: ""}, // 空别名必须跳过
				{Name: "flash", Alias: "flash"},          // 同名必须跳过
				{Name: "", Alias: "orphan"},              // 空 name 必须跳过
			},
		},
	}
	cacheModelAliases(host)
	t.Cleanup(func() {
		modelAliasCache.Lock()
		modelAliasCache.byAlias = nil
		modelAliasCache.Unlock()
	})

	if got := resolveUpstreamModel("qwen-3.8-max", nil); got != "qwen3.8-max-preview" {
		t.Errorf("cacheModelAliases did not register alias, got %q", got)
	}
	modelAliasCache.RLock()
	n := len(modelAliasCache.byAlias)
	modelAliasCache.RUnlock()
	if n != 1 {
		t.Errorf("cache must hold exactly 1 valid entry, got %d", n)
	}
}

// TestCacheModelAliases_CaseInsensitiveChannel 验证 channel 名大小写不敏感匹配。
func TestCacheModelAliases_CaseInsensitiveChannel(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		OAuthModelAlias: map[string][]pluginapi.ModelAlias{
			"QwenWork-Provider": {{Name: "qwen3.8-max-preview", Alias: "qwen-3.8-max"}},
		},
	}
	cacheModelAliases(host)
	t.Cleanup(func() {
		modelAliasCache.Lock()
		modelAliasCache.byAlias = nil
		modelAliasCache.Unlock()
	})

	if got := resolveUpstreamModel("qwen-3.8-max", nil); got != "qwen3.8-max-preview" {
		t.Errorf("case-insensitive channel match failed, got %q", got)
	}
}

// TestUpstreamModelFor_ExecutorPath 直接覆盖执行器使用的解析入口。
//
// 这是针对 2026-10-18 生产缺陷的回归测试：执行器曾只做 stripProviderPrefix，
// 漏掉别名反解，导致 new-api 经别名请求稳定 400，而面板测试（直发真实 ID）
// 成功。断言必须覆盖「剥前缀 → 反解别名」的完整链路。
func TestUpstreamModelFor_ExecutorPath(t *testing.T) {
	setModelAliasCache(t, map[string]string{
		"qwen-3.8-max": "qwen3.8-max-preview",
		"qwen-3.8":     "qwen3.8-max-preview",
		"qwen3.8-max":  "qwen3.8-max-preview",
	})

	cases := []struct {
		name  string
		input string
		attrs map[string]string
		want  string
	}{
		// 1. 生产真实故障形态：new-api 发别名 → 必须还原为真实 ID。
		{"bare alias qwen-3.8-max", "qwen-3.8-max", nil, "qwen3.8-max-preview"},
		{"bare alias qwen-3.8", "qwen-3.8", nil, "qwen3.8-max-preview"},
		{"bare alias qwen3.8-max", "qwen3.8-max", nil, "qwen3.8-max-preview"},
		// 2. provider 前缀形态：先剥前缀再反解。
		{"prefixed alias", "qwenwork/qwen-3.8-max", nil, "qwen3.8-max-preview"},
		// 3. 真实 ID 原样通过（面板测试走的就是这条）。
		{"real id", "qwen3.8-max-preview", nil, "qwen3.8-max-preview"},
		{"prefixed real id", "qwenwork/qwen3.8-max-preview", nil, "qwen3.8-max-preview"},
		// 4. 大小写不敏感。
		{"uppercase alias", "QWEN-3.8-MAX", nil, "qwen3.8-max-preview"},
		// 5. auth 属性通道同样生效。
		{"auth attribute alias", "custom-alias", map[string]string{"model_alias": "custom-alias=pro"}, "pro"},
		// 6. 未知模型不得被改写。
		{"unknown model", "some-other-model", nil, "some-other-model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := upstreamModelFor(c.input, c.attrs); got != c.want {
				t.Errorf("upstreamModelFor(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}
