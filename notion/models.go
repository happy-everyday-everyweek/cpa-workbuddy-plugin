package main

// 本文件负责工作空间发现、额度查询与模型目录。
//
// getSpaces 返回的是双层结构：外层键是用户根 id，内层 space 才是
// workspace id。直接拿外层键当 spaceId 会得到
// 「403 Space is not part of an organization」，这是接入时最容易踩的坑。

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// getSpacesResponse 是 getSpaces 的响应结构。
//
// 外层 map 的键是用户根 id，值里的 space 字段才是 workspace 映射。
type getSpacesResponse map[string]struct {
	Space map[string]json.RawMessage `json:"space"`
}

// spaceValue 是单个 workspace 的字段子集。
type spaceValue struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	PlanType string `json:"plan_type"`
}

// fetchSpaces 拉取账号下的全部工作空间。
// [参数] ctx: 上下文；cl: 客户端；scope: 账号作用域（SpaceID 可为空）。
// [返回] 空间列表与错误。
// 最近修改时间：2026-10-18；改动原因：新增，双层结构解析。
func fetchSpaces(ctx context.Context, cl *http.Client, scope requestScope) ([]NotionSpace, error) {
	var resp getSpacesResponse
	if err := doNotionJSON(ctx, cl, scope, "/getSpaces", []byte(`{}`), &resp); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var out []NotionSpace
	for _, root := range resp {
		for spaceID, rawValue := range root.Space {
			if _, ok := seen[spaceID]; ok {
				continue
			}
			seen[spaceID] = struct{}{}

			var value spaceValue
			_ = json.Unmarshal(rawValue, &value)
			if value.ID == "" {
				value.ID = spaceID
			}
			out = append(out, NotionSpace{
				ID:       spaceID,
				Name:     firstNonEmpty(value.Name, value.Domain, spaceID),
				PlanType: value.PlanType,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// usageEligibilityResponse 是 getAIUsageEligibilityV2 的响应结构。
type usageEligibilityResponse struct {
	PremiumCredits struct {
		TotalCreditBalance int `json:"totalCreditBalance"`
		PerSource          struct {
			MonthlyAllocated struct {
				Limit      int `json:"limit"`
				UsageTotal int `json:"usageTotal"`
			} `json:"monthlyAllocated"`
		} `json:"perSource"`
	} `json:"premiumCredits"`
	BasicCredits struct {
		SpaceLimit int `json:"spaceLimit"`
		SpaceUsage int `json:"spaceUsage"`
		UserLimit  int `json:"userLimit"`
		UserUsage  int `json:"userUsage"`
	} `json:"basicCredits"`
}

// enrichSpaceUsage 为每个空间补齐额度快照。
//
// 额度只用于展示与路由参考，不是推理门槛：实测免费版 0 premium credits
// 同样能出正文，因此查询失败只记 0 而不阻断账号导入。
// [参数] ctx: 上下文；cl: 客户端；scope: 账号作用域；spaces: 待补齐的空间。
// [返回] 无（就地修改）。
// 最近修改时间：2026-10-18；改动原因：新增，多空间额度快照。
func enrichSpaceUsage(ctx context.Context, cl *http.Client, scope requestScope, spaces []NotionSpace) {
	for i := range spaces {
		queryScope := scope
		queryScope.SpaceID = spaces[i].ID

		body, _ := json.Marshal(map[string]string{"spaceId": spaces[i].ID})
		var resp usageEligibilityResponse
		if errQuery := doNotionJSON(ctx, cl, queryScope, "/getAIUsageEligibilityV2", body, &resp); errQuery != nil {
			continue
		}
		spaces[i].PremiumCredits = resp.PremiumCredits.TotalCreditBalance
		spaces[i].BasicLimit = resp.BasicCredits.SpaceLimit
		spaces[i].BasicUsage = resp.BasicCredits.SpaceUsage
		spaces[i].UsageCheckedAt = time.Now().Format(time.RFC3339)
	}
}

// availableModelsResponse 是 getAvailableModels 的响应结构。
//
// 上游把模型放在数组里，每项含内部代号与可用性标记。
type availableModelsResponse struct {
	Models []struct {
		Model    string `json:"model"`
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Disabled bool   `json:"isDisabled"`
		// Available 在部分灰度返回里替代 isDisabled 表达可用性。
		Available *bool `json:"available"`
	} `json:"models"`
}

// fetchAvailableModels 拉取指定空间的可用模型。
// [参数] ctx: 上下文；cl: 客户端；scope: 账号作用域（SpaceID 必填）。
// [返回] 可用模型代号列表与错误。
// 最近修改时间：2026-10-18；改动原因：新增，动态模型发现。
func fetchAvailableModels(ctx context.Context, cl *http.Client, scope requestScope) ([]string, error) {
	body, _ := json.Marshal(map[string]string{"spaceId": scope.SpaceID})
	var resp availableModelsResponse
	if err := doNotionJSON(ctx, cl, scope, "/getAvailableModels", body, &resp); err != nil {
		return nil, err
	}

	var out []string
	for _, item := range resp.Models {
		code := strings.TrimSpace(item.Model)
		if code == "" {
			continue
		}
		if item.Disabled {
			continue
		}
		if item.Available != nil && !*item.Available {
			continue
		}
		out = append(out, code)
	}
	return out, nil
}

// modelStatic 响应 model.static / model.register。
// [参数] request: 宿主请求体。
// [返回] 信封 JSON 与错误。
// 最近修改时间：2026-10-18；改动原因：新增，静态模型目录。
func modelStatic(request []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerName,
		Models:   filterExcludedModels(staticModelList(), req.Host),
	})
}

// modelForAuth 响应 model.for_auth。
//
// 按账号返回真实可见的模型目录；探测失败时回落到静态种子，
// 一次网络失败不该让用户没模型可选。
// [参数] request: 宿主请求体。
// [返回] 信封 JSON 与错误。
// 最近修改时间：2026-10-18；改动原因：新增，按账号动态目录。
func modelForAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}

	models := staticModelList()
	if auth := authFromModelRequest(&req); auth != nil {
		if discovered, errDiscover := discoverModels(auth); errDiscover == nil && len(discovered) > 0 {
			models = buildModelInfos(discovered)
		}
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerName,
		Models:   filterExcludedModels(models, req.Host),
	})
}

// authFromModelRequest 从宿主请求里取出账号。
// [参数] req: 宿主模型请求。
// [返回] 账号；无法解析时返回 nil。
// 最近修改时间：2026-10-18；改动原因：新增，请求到账号的转换。
func authFromModelRequest(req *pluginapi.AuthModelRequest) *StoredAuth {
	if len(req.StorageJSON) > 0 {
		var auth StoredAuth
		if errUnmarshal := json.Unmarshal(req.StorageJSON, &auth); errUnmarshal == nil && auth.CookieHeader != "" {
			if auth.SpaceID == "" && len(auth.Spaces) > 0 {
				auth.SpaceID = auth.Spaces[0].ID
			}
			return &auth
		}
	}
	if strings.TrimSpace(req.AuthID) != "" {
		for _, auth := range listHostAuthFiles() {
			if auth.AuthID == req.AuthID {
				return auth
			}
		}
	}
	return nil
}

// discoverModels 用账号实时拉取模型清单。
// [参数] auth: 账号。
// [返回] 模型代号列表与错误。
// 最近修改时间：2026-10-18；改动原因：新增，动态发现出口。
func discoverModels(auth *StoredAuth) ([]string, error) {
	cfg := currentConfig()
	ctx, cancel := context.WithTimeout(context.Background(), notionRequestTimeout())
	defer cancel()

	cl := ensureHTTPClient(notionRequestTimeout())
	return fetchAvailableModels(ctx, cl, scopeForAuth(auth, cfg.ClientVersion))
}

// buildModelInfos 把内部代号转成宿主模型信息。
// [参数] codes: 内部代号列表。
// [返回] 宿主模型信息列表。
// 最近修改时间：2026-10-18；改动原因：新增，代号到模型元数据映射。
func buildModelInfos(codes []string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(codes))
	for _, code := range codes {
		out = append(out, modelInfoFor(code))
	}
	return out
}

// modelInfoFor 构造单个模型的宿主元数据。
//
// 模型 ID 带 notion/ 前缀，避免与其它 provider 的同名模型冲突；
// Name 保留内部代号，回写请求时直接用它。
// [参数] code: 内部代号。
// [返回] 模型信息。
// 最近修改时间：2026-10-18；改动原因：新增，单模型映射。
func modelInfoFor(code string) pluginapi.ModelInfo {
	display, known := notionModelDisplayNames[code]
	if !known {
		display = code
	}
	return pluginapi.ModelInfo{
		ID:            "notion/" + code,
		Object:        "model",
		OwnedBy:       providerName,
		Type:          "chat",
		DisplayName:   display,
		Name:          code,
		Description:   "Notion AI（" + display + "）",
		ContextLength: 1000000,
	}
}

// staticModelList 返回静态种子模型目录。
//
// 上游模型代号会随灰度变化，动态发现才是主路径；这里保留实测可用的
// 代号作为发现失败时的兜底，避免面板在探测失败时完全空目录。
// [参数] 无。
// [返回] 模型信息列表。
// 最近修改时间：2026-10-18；改动原因：新增，静态 fallback。
func staticModelList() []pluginapi.ModelInfo {
	return buildModelInfos(enabledNotionModels())
}

// filterExcludedModels 剔除宿主排除配置里的模型。
//
// model.static 的返回会被原样登记进全局模型目录，宿主不会替插件过滤，
// 因此插件必须在出口自行处理，否则无法把同名模型从本服务商摘掉。
// [参数] models: 模型列表；host: 宿主配置摘要。
// [返回] 过滤后的列表。
// 最近修改时间：2026-10-18；改动原因：新增，与既有插件一致的排除语义。
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
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

	blocked := make(map[string]struct{}, len(excluded))
	for _, id := range excluded {
		blocked[strings.TrimSpace(id)] = struct{}{}
	}

	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, hit := blocked[m.ID]; hit {
			continue
		}
		if _, hit := blocked[m.Name]; hit {
			continue
		}
		out = append(out, m)
	}
	return out
}
