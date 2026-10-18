package main

// 本文件维护 Notion 内部模型代号到展示名的映射。
//
// Notion 对外不暴露厂商原名，而是用甜点、人名等代号（如
// omniberry-sundae = GPT-6.1 Sol）加厂商前缀（vertex- / baseten- /
// fireworks-）。代号随灰度变化，动态发现才是主路径；这张表只用于
// 展示名回填与发现失败时的兜底。
//
// 数据来源：上游 getAvailableModels 实测返回的 39 条代号
// （2026-10-18 核实，其中 36 可用、3 条 isDisabled）。
//
// 最近修改时间：2026-10-18；改动原因：新增，模型代号静态表。

import "strings"

// notionModel 描述一个内部代号的元数据。
type notionModel struct {
	// Display 是展示名。
	Display string
	// Family 是厂商家族。
	Family string
	// Disabled 表示上游已标记为不可用，不应注册给宿主。
	Disabled bool
}

// notionModelCatalog 是完整的代号表。
//
// 保持上游顺序：面板与日志按此顺序展示，便于与网页端模型选择器对照。
var notionModelCatalog = []struct {
	Code  string
	Model notionModel
}{
	{"omniberry-sundae", notionModel{Display: "GPT-6.1 Sol", Family: "openai"}},
	{"orchid-muffin", notionModel{Display: "GPT-5.6 Terra", Family: "openai"}},
	{"omodi", notionModel{Display: "GPT-6 Luna", Family: "openai"}},
	{"albuquerque-quinn", notionModel{Display: "Opus 5.5", Family: "anthropic"}},
	{"soursop-shortcake", notionModel{Display: "Grok 4.6", Family: "xai"}},
	{"olive-jellyroll", notionModel{Display: "GPT-5.6 Luna", Family: "openai"}},
	{"orange-mousse", notionModel{Display: "GPT-5.6 Sol", Family: "openai"}},
	{"oatmeal-cookie", notionModel{Display: "GPT-5.2", Family: "openai"}},
	{"oval-kumquat-medium", notionModel{Display: "GPT-5.4", Family: "openai"}},
	{"opal-quince-medium", notionModel{Display: "GPT-5.5", Family: "openai"}},
	{"vertex-gemini-3.5-flash", notionModel{Display: "Gemini 3.5 Flash", Family: "gemini"}},
	{"vertex-gemini-3.6-flash", notionModel{Display: "Gemini 3.6 Flash", Family: "gemini"}},
	{"grapefruit-zeppole", notionModel{Display: "Gemini 3.7 Flash", Family: "gemini"}},
	{"almond-croissant-low", notionModel{Display: "Sonnet 4.6", Family: "anthropic"}},
	{"angel-cake-high", notionModel{Display: "Sonnet 5", Family: "anthropic"}},
	{"achira-donut", notionModel{Display: "Sonnet 5.5", Family: "anthropic"}},
	{"amla-jam", notionModel{Display: "Haiku 5.5", Family: "anthropic"}},
	{"avocado-froyo-medium", notionModel{Display: "Opus 4.6", Family: "anthropic"}},
	{"apricot-sorbet-high", notionModel{Display: "Opus 4.7", Family: "anthropic"}},
	{"ambrosia-tart-high", notionModel{Display: "Opus 4.8", Family: "anthropic"}},
	{"agave-flan", notionModel{Display: "Opus 5", Family: "anthropic"}},
	{"oregon-grape-medium", notionModel{Display: "GPT-5.4 Mini", Family: "openai"}},
	{"otaheite-apple-medium", notionModel{Display: "GPT-5.4 Nano", Family: "openai"}},
	{"fireworks-kimi-k3", notionModel{Display: "Kimi K3", Family: "mystery"}},
	{"baseten-deepseek-v4-pro", notionModel{Display: "DeepSeek V4 Pro", Family: "mystery"}},
	{"baseten-deepseek-v4-flash", notionModel{Display: "DeepSeek V4 Flash", Family: "mystery"}},
	{"baseten-deepseek-v4.1-flash", notionModel{Display: "DeepSeek V4.1 Flash", Family: "mystery"}},
	{"baseten-glm-5.2", notionModel{Display: "GLM 5.2", Family: "mystery"}},
	{"baseten-glm-5.3", notionModel{Display: "GLM 5.3", Family: "mystery"}},
	{"baseten-glm-5.3-flash", notionModel{Display: "GLM 5.3 Flash", Family: "mystery"}},
	{"xigua-mochi-medium", notionModel{Display: "Grok 4.3", Family: "xai"}},
	{"strawberry-whoopiepie", notionModel{Display: "Grok 4.5", Family: "xai"}},
	{"xinomavro-cake", notionModel{Display: "Grok Build 0.1", Family: "xai"}},
	{"galette-medium-thinking", notionModel{Display: "Gemini 3.1 Pro", Family: "gemini"}},
	{"anthropic-haiku-4.5", notionModel{Display: "Haiku 4.5", Family: "anthropic"}},
	{"gingerbread", notionModel{Display: "Gemini 3 Flash", Family: "gemini"}},
	{"orlando-quinn", notionModel{Display: "GPT-6 Astra", Family: "openai", Disabled: true}},
	{"assam-chai", notionModel{Display: "Fable 5.1", Family: "anthropic", Disabled: true}},
	{"acai-budino-high", notionModel{Display: "Fable 5", Family: "anthropic", Disabled: true}},
}

// notionModelDisplayNames 是代号到展示名的只读视图。
var notionModelDisplayNames = func() map[string]string {
	out := make(map[string]string, len(notionModelCatalog))
	for _, entry := range notionModelCatalog {
		out[entry.Code] = entry.Model.Display
	}
	return out
}()

// defaultNotionModel 是账号未指定模型时的兜底代号。
//
// 选 GPT-6 Luna（omodi）：实测出正文稳定、工具调用开销小。
const defaultNotionModel = "omodi"

// normalizeNotionModel 把宿主传入的模型名解析成上游代号。
//
// 宿主可能传三种形态：带前缀的 notion/<code>、裸代号、展示名
// （用户直接在面板里填了 "GPT-6 Luna"）。全部归一到内部代号；
// 无法识别时回落到默认模型而不是把未知字符串透传给上游。
// [参数] model: 宿主传入的模型名。
// [返回] 上游内部代号。
// 最近修改时间：2026-10-18；改动原因：新增，模型名归一。
func normalizeNotionModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return defaultNotionModel
	}
	model = strings.TrimPrefix(model, "notion/")

	if _, ok := notionModelDisplayNames[model]; ok {
		return model
	}

	// 支持用户填展示名。
	lower := strings.ToLower(model)
	for _, entry := range notionModelCatalog {
		if strings.ToLower(entry.Model.Display) == lower {
			return entry.Code
		}
	}
	return defaultNotionModel
}

// enabledNotionModels 返回全部可用代号。
// [参数] 无。
// [返回] 未禁用的代号列表，保持上游顺序。
// 最近修改时间：2026-10-18；改动原因：新增，静态兜底数据源。
func enabledNotionModels() []string {
	out := make([]string, 0, len(notionModelCatalog))
	for _, entry := range notionModelCatalog {
		if entry.Model.Disabled {
			continue
		}
		out = append(out, entry.Code)
	}
	return out
}
