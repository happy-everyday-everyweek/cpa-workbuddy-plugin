// models.go implements the ModelProvider capability: static and per-auth
// model lists, dynamic model discovery via the upstream models API, alias
// reverse resolution (client-facing alias → upstream model id), and the
// host-config oauth-excluded-models filter.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbModels is the static fallback model list for QoderWork CN. Keys mirror
// /root/qoderwork/models_list.json (KNOWLEDGE §6.2). Aliases use the qoder/
// prefix in AuthAttributes; bare IDs work too. Dynamic refresh via
// /algo/api/v2/model/list replaces this at runtime when an account is present.
func wbModels() []pluginapi.ModelInfo {
	list := []pluginapi.ModelInfo{
		{ID: "auto", Name: "Auto", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "Cantus", Name: "Cantus", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "Qwen3.8-Max", Name: "Qwen3.8-Max", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "Qwen3.8-Flash", Name: "Qwen3.8-Flash", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "Qwen3.7-Max", Name: "Qwen3.7-Max", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "Qwen3.7-Plus", Name: "Qwen3.7-Plus", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "GLM-5.3", Name: "GLM-5.3", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "GLM-5.3-Flash", Name: "GLM-5.3-Flash", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "Kimi-K3", Name: "Kimi-K3", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "Kimi-K2.8-Preview", Name: "Kimi-K2.8-Preview", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "DeepSeek-V4-Pro", Name: "DeepSeek-V4-Pro", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "DeepSeek-Flash", Name: "DeepSeek-Flash", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "MiniMax-M3", Name: "MiniMax-M3", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "cmodel", Name: "Cantus", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel_38max", Name: "Qwen3.8-Max", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qfmodel", Name: "Qwen3.8-Flash", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel_latest", Name: "Qwen3.7-Max", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel", Name: "Qwen3.7-Plus", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "gmodel", Name: "GLM-5.3", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "gfmodel", Name: "GLM-5.3-Flash", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kmodel_latest", Name: "Kimi-K3", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kmodel", Name: "Kimi-K2.8-Preview", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "dmodel", Name: "DeepSeek-V4-Pro", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "dfmodel", Name: "DeepSeek-Flash", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "mmodel", Name: "MiniMax-M3", ContextLength: 1000000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
	for i := range list {
		spec := resolveOfficialModelSpec(list[i].ID)
		if spec.ReasoningMax > 0 {
			list[i].MaxCompletionTokens = applyMaxTokensFloor(spec.ReasoningMax)
		} else {
			list[i].MaxCompletionTokens = applyMaxTokensFloor(spec.MaxOutputTokens)
		}
	}
	return list
}

// configuredModels holds the config_yaml `models:` override. Empty means
// fall back to dynamic discovery / wbModels(). Guarded by its own RWMutex:
// configure() runs on a host RPC thread while model.static / model.for_auth
// run on other RPC threads, so the slice must never be written/read unlocked.
var (
	configuredModels   []pluginapi.ModelInfo
	configuredModelsMu sync.RWMutex
)

// setConfiguredModels 整体替换 config_yaml `models:` 覆盖列表。
// [参数] models：新的模型列表（非 nil 非空）
// [返回] 无
// 最近修改时间 2026-08-28（新增 config_yaml models 覆盖支持）
func setConfiguredModels(models []pluginapi.ModelInfo) {
	configuredModelsMu.Lock()
	configuredModels = models
	configuredModelsMu.Unlock()
}

// clearConfiguredModels 清空覆盖，恢复动态获取 / 静态默认路径。
// [参数] 无
// [返回] 无
// 最近修改时间 2026-08-28（新增 config_yaml models 覆盖支持）
func clearConfiguredModels() {
	configuredModelsMu.Lock()
	configuredModels = nil
	configuredModelsMu.Unlock()
}

// getConfiguredModels 返回当前覆盖列表的引用（调用方不得原地修改）。
// [参数] 无
// [返回] configuredModels 当前值，空切片语义为"未覆盖"
// 最近修改时间 2026-08-28（新增 config_yaml models 覆盖支持）
func getConfiguredModels() []pluginapi.ModelInfo {
	configuredModelsMu.RLock()
	defer configuredModelsMu.RUnlock()
	return configuredModels
}

// parseModelsConfig decodes the config_yaml `models:` list. Items may be a
// string (model id) or an object {id, name, alias, context, max_tokens,
// enabled, reasoning}; enabled=false entries are skipped. An explicit empty
// list clears the override (back to dynamic discovery / static defaults); a
// list whose entries are all malformed leaves the current value untouched so
// a bad edit never silently wipes a working override.
// [参数] v：config_yaml `models:` 的 JSON 值（字符串列表或对象列表）
// [返回] 无（内部写入 configuredModels）
// 最近修改时间 2026-08-28（新增 config_yaml models 覆盖支持）
func parseModelsConfig(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return
	}
	// 显式 `models: []` 表示"停止覆盖"，恢复动态获取 / 静态默认。
	if len(items) == 0 {
		clearConfiguredModels()
		return
	}
	var out []pluginapi.ModelInfo
	for _, item := range items {
		var s string
		if err := json.Unmarshal(item, &s); err == nil && strings.TrimSpace(s) != "" {
			out = append(out, modelInfoFromConfig(strings.TrimSpace(s), "", 0, 0))
			continue
		}
		var mi struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Alias     string `json:"alias"`
			Context   int64  `json:"context"`
			MaxTokens int64  `json:"max_tokens"`
			Enabled   *bool  `json:"enabled"`
			Reasoning bool   `json:"reasoning"`
		}
		if err := json.Unmarshal(item, &mi); err != nil || strings.TrimSpace(mi.ID) == "" {
			continue
		}
		if mi.Enabled != nil && !*mi.Enabled {
			continue
		}
		out = append(out, modelInfoFromConfig(strings.TrimSpace(mi.ID), strings.TrimSpace(mi.Name), mi.Context, mi.MaxTokens))
	}
	if len(out) > 0 {
		setConfiguredModels(out)
	}
}

// resolveModels 按优先级链求最终模型列表：动态发现 > 配置 > 静态默认。
//
// 语义（2026-09-12 优先级反转）：
//   - 动态发现有结果时**完全忽略** config_yaml 覆盖与静态默认：上游是权威
//     全集，动态列表即最终列表，配置不再参与合并；
//   - 动态发现无结果（无凭据 / 上游失败 / 空列表）时，才使用配置覆盖；
//   - 配置也为空时，最后回退静态默认列表。
//
// 之所以要"动态优先"：上游新增模型只能从动态发现拿到；若配置优先，用户手工
// 配过的旧列表会永久遮蔽上游新模型。配置与静态列表降级为**保底**。
//
// [参数] dynamic：动态发现列表（可为空，空表示"动态不可用"）
//
//	configured：config_yaml `models:` 覆盖列表（可为空）
//	fallback：静态默认列表（wbModels()）
//
// [返回] 按优先级链选出的最终列表
// 最近修改时间 2026-09-12（由"配置优先"反转为"动态优先、配置保底"）
func resolveModels(dynamic, configured, fallback []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	// 先过滤空 ID 再看长度：只含空 ID 的动态列表语义上等于"没有结果"，
	// 不能因为它 len>0 就遮蔽配置或静态兜底。
	if dm := nonEmptyModels(dynamic); len(dm) > 0 {
		return dm
	}
	if cm := nonEmptyModels(configured); len(cm) > 0 {
		return cm
	}
	return nonEmptyModels(fallback)
}

// nonEmptyModels 过滤空 ID 条目并返回新切片，避免调用方拿到含空 ID 的列表或
// 共享底层数组（dynamicModelsCache / wbModels 的切片不可原地改写）。
// [参数] models：待过滤列表
// [返回] 去掉空 ID 后的新切片
// 最近修改时间 2026-09-12（随优先级反转新增）
func nonEmptyModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// resolveModelsFromStorage 是 handleModelForAuth 的判定入口：先尝试账号级
// 动态发现，成功即返回动态列表，否则交给优先级链回退配置 / 静态默认。
// [参数] storageJSON：宿主传入的账号凭据 JSON
// [返回] 按优先级链选出的最终列表
// 最近修改时间 2026-09-12（动态成功不再让配置遮蔽上游模型）
func resolveModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	dynamic := fetchDynamicModelsFromStorage(storageJSON)
	return resolveModels(dynamic, getConfiguredModels(), wbModels())
}

// modelInfoFromConfig builds a ModelInfo for a config-declared model,
// normalizing fields to match the plugin's other model sources. alias /
// reasoning are accepted for schema compatibility but have no ModelInfo
// counterpart and are intentionally ignored here.
// [参数] id：模型 ID（必填）；name：显示名（空则用 id）；ctxLen：上下文长度；maxTok：最大输出 token 数
// [返回] 规范化后的 pluginapi.ModelInfo
// 最近修改时间 2026-08-28（新增 config_yaml models 覆盖支持）
func modelInfoFromConfig(id, name string, ctxLen, maxTok int64) pluginapi.ModelInfo {
	if name == "" {
		name = id
	}
	return pluginapi.ModelInfo{
		ID:                         id,
		Name:                       name,
		ContextLength:              ctxLen,
		MaxCompletionTokens:        applyMaxTokensFloor(maxTok),
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
}

func cachedDynamicModels() ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.models) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsCacheTTL {
		return dynamicModelsCache.models, true
	}
	return nil, false
}

// callModelsAPIFn 是 callModelsAPI 的可替换入口（同 sendActivePingQoderFn 惯例），
// 供负缓存行为单测注入假上游，避免真实网络与宿主 RPC 依赖。
var callModelsAPIFn = callModelsAPI

// fetchDynamicModelsFallbackFn 是存储入口回退到全量扫描的调用点接缝。
// 单测据此断言「回退发生在开启负缓存窗口之前」——顺序颠倒会让窗口
// 立刻拦住回退本身，使另一账号本可成功的扫描永远走不到。
var fetchDynamicModelsFallbackFn = fetchDynamicModels

// inDynamicModelsFailWindow 判断当前是否处于模型发现负缓存窗口内。
// [返回] true 表示窗口内，调用方应直接放弃本次逐账号扫描。
func inDynamicModelsFailWindow() bool {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	return !dynamicModelsCache.failedAt.IsZero() && time.Since(dynamicModelsCache.failedAt) < dynamicModelsFailCacheTTL
}

// markDynamicModelsFailed 记录一次「全账号扫描失败」，开启负缓存窗口。
func markDynamicModelsFailed() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.failedAt = time.Now()
	dynamicModelsCache.Unlock()
}

func storeDynamicModels(models []pluginapi.ModelInfo) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = models
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.failedAt = time.Time{} // 成功即清除负缓存窗口
	dynamicModelsCache.Unlock()
}

// fetchDynamicModels 按宿主已注册的 qoderwork 凭据逐个尝试动态发现。
//
// **不**在此处兜底静态列表：返回空切片语义为"动态发现不可用"，由调用方按
// resolveModels 的优先级链回退配置或静态默认。若这里返回 wbModels()，
// 调用方无法区分"上游真的只有这些模型"与"上游调用失败"。
// [参数] 无
// [返回] 动态模型列表；动态不可用时返回 nil
// 最近修改时间 2026-10-19 13:10:00；改动原因：新增失败负缓存窗口拦截，切断 N+1 重扫风暴
func fetchDynamicModels() []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		return models
	}
	// 负缓存窗口内直接放弃逐账号重扫：宿主每次 auth 重写都会触发
	// model.for_auth，若失败不留窗口，N 账号 × 每事件一次全扫会放大成
	// N+1 请求风暴（生产 OOM 主因，见 BUG-QODER-MODELS-SIGNATURE-INVALID-20261012）。
	if inDynamicModelsFailWindow() {
		return nil
	}
	files, err := hostAuthListFiles()
	if err != nil || len(files) == 0 {
		return nil
	}
	// Strict filename-prefix match — same filter as host_auth.go hostAuthList.
	// (Earlier code also matched files containing "codebuddy" anywhere, which
	// would wrongly include workbuddy-*.json auths here and cause us to call
	// the qoderwork models API with a workbuddy token.)
	prefix := authFilePrefix
	scanned := 0
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			continue
		}
		scanned++
		raw, err := hostAuthGetByIndex(f.AuthIndex)
		if err != nil {
			continue
		}
		sa, err := parseStored(raw)
		if err != nil || sa == nil {
			continue
		}
		dyn, err := callModelsAPIFn(sa)
		if err == nil && len(dyn) > 0 {
			storeDynamicModels(dyn)
			return dyn
		}
	}
	if scanned > 0 {
		markDynamicModelsFailed()
	}
	return nil
}

// fetchDynamicModelsFromStorage 用请求自带的账号凭据优先尝试动态发现，
// 失败时再回退到已注册凭据扫描。**不**兜底配置与静态列表。
// [参数] storageJSON：宿主传入的账号凭据 JSON
// [返回] 动态模型列表；动态不可用时返回 nil
// 最近修改时间 2026-10-19 13:10:00；改动原因：新增失败负缓存窗口拦截与失败标记
func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		return models
	}
	// 负缓存窗口内直接返回 nil，让上层走 resolveModels 静态兜底。
	if inDynamicModelsFailWindow() {
		return nil
	}
	sa, err := parseStored(storageJSON)
	if err != nil || sa == nil {
		return fetchDynamicModelsFallbackFn()
	}
	dyn, dynErr := callModelsAPIFn(sa)
	if dynErr == nil && len(dyn) > 0 {
		storeDynamicModels(dyn)
		return dyn
	}
	// 本账号被拒后先回退全量扫描，回退必须在开窗之前：窗口一旦置位，
	// 全量扫描入口立即返回 nil，本函数声明的「回退到已注册凭据扫描」
	// 就变成死路径（另一账号本可成功）。
	if dyn := fetchDynamicModelsFallbackFn(); len(dyn) > 0 {
		return dyn
	}
	// 回退也拿不到结果才开窗：本账号确实打过上游，属真实失败而非
	// 「本地没账号」，可以安全进入负缓存。
	markDynamicModelsFailed()
	return nil
}

// newModelsAPIRequest 构造一次 COSY 签名的模型清单 GET 请求。
//
// model/list 语义上无入参，但 COSY 签名覆盖 body，且服务端会校验
// 「签名声明的 body == 实际收到的 body」：裸 GET（body=nil）恒被判
// Signature invalid（403 code 101，生产累计 5449 条，见
// BUG-QODER-MODELS-SIGNATURE-INVALID-20261012）。参照可正常工作的
// qoder2api-hub read_dynamic_models，必须把编码后的 "{}" 真作为 GET
// 请求体发出，与签名使用同一字符串。
// [参数] ctx：请求上下文；sa：签名与鉴权所用账号；rawURL：模型清单端点。
// [返回] 已附带 COSY 头的请求；签名构建失败时返回错误。
// 最近修改时间：2026-10-19 12:30:00；改动原因：从 callModelsAPI 抽出，便于断言签名 body 与实际 body 一致。
func newModelsAPIRequest(ctx context.Context, sa *storedAuth, rawURL string) (*http.Request, error) {
	encodedBody := qoderEncode([]byte("{}"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, strings.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	// COSY signing needs the encoded body even for GET (signature includes body).
	if err := applyCosyHeaders(req, sa, encodedBody, rawURL, "", false); err != nil {
		return nil, fmt.Errorf("cosy sign: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	return req, nil
}

// callModelsAPI GETs /algo/api/v2/model/list from the QoderWork gateway
// with COSY signing and parses the plain-JSON chat scene into ModelInfo.
// 请求构造（含签名）见 newModelsAPIRequest。
// [参数] sa：用于签名与鉴权的账号凭据
// [返回] 动态模型列表；请求或解析失败时返回错误，由调用方按优先级链回退配置 / 静态默认。
// 最近修改时间：2026-10-19 12:30:00；改动原因：GET 真携带签名所用 body，修复 Signature invalid。
func callModelsAPI(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := newModelsAPIRequest(ctx, sa, endpointModels)
	if err != nil {
		return nil, err
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("[qoderwork] callModelsAPI: upstream status %d, body: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	// Response is plain JSON: {"chat":[{key,display_name,...}], "developer":[...], ...}
	var apiResp map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	// Prefer the "chat" scene (matches our inference use case).
	chatRaw, ok := apiResp["chat"]
	if !ok {
		return nil, fmt.Errorf("no chat scene in models response")
	}
	var models []struct {
		Key            string  `json:"key"`
		DisplayName    string  `json:"display_name"`
		Enable         bool    `json:"enable"`
		IsReasoning    bool    `json:"is_reasoning"`
		IsVL           bool    `json:"is_vl"`
		MaxInputTokens int64   `json:"max_input_tokens"`
		PriceFactor    float64 `json:"price_factor"`
	}
	if err := json.Unmarshal(chatRaw, &models); err != nil {
		return nil, fmt.Errorf("chat scene parse: %w", err)
	}
	var out []pluginapi.ModelInfo
	for _, m := range models {
		if !m.Enable {
			continue
		}
		ctx2 := int64(180000)
		if m.MaxInputTokens > 0 {
			ctx2 = m.MaxInputTokens
		}
		out = append(out, pluginapi.ModelInfo{
			ID:            m.Key,
			Name:          m.DisplayName,
			ContextLength: ctx2,
			MaxCompletionTokens: func() int64 {
				spec := resolveOfficialModelSpec(m.Key)
				if spec.ReasoningMax > 0 {
					return applyMaxTokensFloor(spec.ReasoningMax)
				}
				return applyMaxTokensFloor(spec.MaxOutputTokens)
			}(),
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no enabled chat models")
	}
	log.Printf("[qoderwork] callModelsAPI: dynamic discovery ok: %d models", len(out))
	return out, nil
}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		// Host may key the channel case-insensitively; fall back to a scan.
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// defaultModelAliasMap provides canonical mapping from client-facing model
// names to upstream Qoder gateway model keys.
var defaultModelAliasMap = map[string]string{
	// Cantus
	"cantus": "cmodel",
	// Qwen 3.8
	"qwen3.8-max":         "qmodel_38max",
	"qwen3.8-max-preview": "qmodel_38max",
	"qmodel_preview":      "qmodel_38max",
	"qwen3.8-flash":       "qfmodel",
	"qwen3.6-flash":       "qfmodel",
	"q36fmodel":           "qfmodel",
	// Qwen 3.7
	"qwen3.7-max":  "qmodel_latest",
	"qwen3.7-plus": "qmodel",
	// GLM 5.3
	"glm-5.3":       "gmodel",
	"glm-5.2":       "gmodel",
	"gm51model":     "gmodel",
	"glm-5.3-flash": "gfmodel",
	// Kimi
	"kimi-k3":           "kmodel_latest",
	"kimi-k2.8-preview": "kmodel",
	"kimi-k2.7-code":    "kmodel",
	// DeepSeek
	"deepseek-v4-pro":   "dmodel",
	"deepseek-flash":    "dfmodel",
	"deepseek-v4-flash": "dfmodel",
	// MiniMax
	"minimax-m3":   "mmodel",
	"minimax-m2.7": "mmodel",
}

// resolveUpstreamModel maps an aliased requested model back to the real
// upstream model ID. Returns the input unchanged when nothing matches.
func resolveUpstreamModel(model string, attributes map[string]string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return model
	}
	key := strings.ToLower(m)
	if name, ok := parseModelAliasAttribute(attributes)[key]; ok {
		return name
	}
	modelAliasCache.RLock()
	name, ok := modelAliasCache.byAlias[key]
	modelAliasCache.RUnlock()
	if ok {
		return name
	}
	if canonical, ok := defaultModelAliasMap[key]; ok {
		return canonical
	}
	return m
}

// parseModelAliasAttribute decodes a per-auth alias override from auth
// attributes. Accepts JSON ([{"name":...,"alias":...}] or {alias:name}) or
// comma-separated "alias=name" pairs.
func parseModelAliasAttribute(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	raw := ""
	for _, k := range []string{"model_alias", "model-alias", "oauth-model-alias"} {
		if v := strings.TrimSpace(attributes[k]); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	add := func(name, alias string) {
		name, alias = strings.TrimSpace(name), strings.TrimSpace(alias)
		if name != "" && alias != "" && !strings.EqualFold(name, alias) {
			out[strings.ToLower(alias)] = name
		}
	}
	if strings.HasPrefix(raw, "[") {
		var list []struct {
			Name  string `json:"name"`
			Alias string `json:"alias"`
		}
		if json.Unmarshal([]byte(raw), &list) == nil {
			for _, e := range list {
				add(e.Name, e.Alias)
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(raw), &m) == nil {
			for alias, name := range m {
				add(name, alias)
			}
			return out
		}
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			add(kv[1], kv[0])
		}
	}
	return out
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the qoderwork provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	// Try exact provider match, then case-insensitive scan.
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Use a fresh slice — models[:0] would alias the input's backing array,
	// which may be the dynamicModelsCache's own slice. Mutating it in place
	// would corrupt the cache for subsequent callers (P0 bug: after one
	// filterExcludedModels call, cache returns the filtered list as the
	// "full" list on the next fetch).
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publishUsage reports one upstream attempt into CPAMP request monitoring.
// requestedModel is client-facing (may be alias); upstreamModel is resolved.

// handleModelStatic 返回宿主要求的全局模型列表，优先级为动态 > 配置 > 静态默认。
// 动态发现仅能命中已有缓存（StaticModelRequest 不带账号凭据），缓存未命中时
// fetchDynamicModels 会扫描已注册凭据；都失败则回退配置 / 静态默认。
// [参数] raw：宿主传入的 StaticModelRequest
// [返回] 成功 envelope；请求解析失败时返回错误
// 最近修改时间 2026-09-12（由"配置非空即不查动态"反转为动态优先）
func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := resolveModels(fetchDynamicModels(), getConfiguredModels(), wbModels())
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

// handleModelForAuth 返回指定账号的模型列表，优先级为动态 > 配置 > 静态默认。
// [参数] raw：宿主传入的 AuthModelRequest（含 StorageJSON 凭据）
// [返回] 成功 envelope；请求解析失败时返回错误
// 最近修改时间 2026-09-12（动态成功时完全忽略配置，避免遮蔽上游新增模型）
func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key. The host skips any
	// response whose Provider doesn't match the auth's provider, so echoing
	// req.AuthProvider back would silently drop the model list whenever the
	// auth file carries a non-canonical provider string.
	cacheModelAliases(req.Host)
	models := resolveModelsFromStorage(req.StorageJSON)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

// authModelsForIndex 返回指定账号当前可用的模型列表，供面板「测试」弹窗选择。
//
// 复用账号级动态发现链路：优先未过期缓存，未命中时用该账号凭据实时拉取；
// 末尾追加随机兜底模型，保证上游瞬时失败时弹窗仍有一个可测选项。
//
// [参数] authIndex：宿主账号索引。
// [返回] 去重后的模型 ID 列表；账号凭据不可用时返回 nil。
// 最近修改时间 2026-10-10（新增测试按钮指定模型选择支持，同步自 workbuddy）
func authModelsForIndex(authIndex string) []string {
	sa, err := hostAuthGet(authIndex)
	if err != nil || sa == nil {
		return nil
	}
	models, _ := cachedDynamicModels()
	if len(models) == 0 && strings.TrimSpace(sa.Auth.AccessToken) != "" {
		if dyn, ferr := callModelsAPI(sa); ferr == nil && len(dyn) > 0 {
			storeDynamicModels(dyn)
			models = dyn
		} else if ferr != nil {
			log.Printf("[qoderwork] authModelsForIndex: dynamic discovery failed (%v), falling back to static list", ferr)
		}
	}
	if len(models) == 0 {
		models = resolveModels(nil, getConfiguredModels(), wbModels())
	}
	seen := make(map[string]struct{}, len(models)+1)
	out := make([]string, 0, len(models)+1)
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if fallback := pickRandomQoderModel(sa); fallback != "" {
		if _, ok := seen[fallback]; !ok {
			out = append(out, fallback)
		}
	}
	return out
}

// handleModelsQuery 返回指定账号当前可用模型列表，供面板「测试」弹窗选择。
//
// [参数] req：管理请求，query 参数 auth_index 指定账号。
// [返回] {models:[...]}；缺少 auth_index 时返回 {error}。
// 最近修改时间 2026-10-10（新增测试按钮指定模型选择支持，同步自 workbuddy）
func handleModelsQuery(req pluginapi.ManagementRequest) map[string]any {
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	return map[string]any{"models": authModelsForIndex(authIndex)}
}
