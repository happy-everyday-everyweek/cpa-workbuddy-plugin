package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件实现面板「测试」按钮的账号探活。
//
// 当前只做认证层探活，原因：扣子对话链路（coze_claw 的 message/chat）需要
// claw_id 与 session_id，二者来自桌面端运行时上下文，尚未接入（CYCLE-01）。
// 认证层探活能真实回答面板最关心的问题——「这个账号现在还能不能用」：
//   - account/info/v2 判据通过 → 会话 Cookie 仍有效；
//   - 模型目录可拉取 → 该账号的模型权限仍在（顺带验证 /v1/model/list 可达）。
//
// 它不消耗推理配额，也没有生图/生视频那种数十秒等待，适合作为面板的即时反馈。

// testActiveTimeout 是单次探活的等待上限。
//
// 探活只打两个轻量接口，正常 1~3 秒返回；20 秒覆盖上游排队与网络抖动。
const testActiveTimeout = 20 * time.Second

// testActiveResult 是探活结果。
type testActiveResult struct {
	OK        bool   `json:"ok"`
	AuthID    string `json:"auth_id"`
	Label     string `json:"label,omitempty"`
	Model     string `json:"model,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
	// ModelCount 是该账号可见的模型数量，供面板展示。
	ModelCount int `json:"model_count,omitempty"`
	// Scope 说明本次探活的覆盖范围，面板据此提示用户。
	Scope string `json:"scope"`
}

// handleTestActive 处理面板「测试」按钮发起的单账号探活。
//
// 请求体：{auth_id, model?}。model 仅用于回显，不参与探活逻辑。
// [参数] req: 管理请求。
// [返回] 探活结果；失败时带 error 字段。
// 最近修改时间：2026-10-18；改动原因：新增，面板测试模型入口。
func handleTestActive(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthID    string `json:"auth_id"`
		AuthIndex string `json:"auth_index"`
		Model     string `json:"model"`
	}
	_ = json.Unmarshal(req.Body, &body)

	authID := strings.TrimSpace(body.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(body.AuthIndex)
	}
	if authID == "" {
		return map[string]any{"ok": false, "error": "auth_id 不能为空"}
	}

	sa, errFind := findAccountForTest(authID)
	if errFind != nil {
		return map[string]any{"ok": false, "error": errFind.Error()}
	}
	return runActiveTest(sa, strings.TrimSpace(body.Model))
}

// findAccountForTest 按账号标识定位账号。
//
// 同时接受裸 id 与带 coze- 前缀的写法：面板下发的是 auth_id，而用户在宿主侧
// 看到的可能是去掉 .json 的文件名。
// [参数] authID: 账号标识。
// [返回] 账号与错误。
// 最近修改时间：2026-10-18；改动原因：新增，测试账号定位。
func findAccountForTest(authID string) (*StoredAuth, error) {
	accounts, errList := listAllAuthFiles()
	if errList != nil {
		return nil, fmt.Errorf("读取账号失败：%w", errList)
	}
	normalized := strings.TrimSuffix(authID, ".json")
	if !strings.HasPrefix(normalized, cozeAuthFilePrefix) {
		normalized = cozeAuthFilePrefix + normalized
	}
	for _, acc := range accounts {
		if acc.AuthID == authID || acc.AuthID == normalized {
			return acc, nil
		}
	}
	return nil, fmt.Errorf("账号不存在：%s", authID)
}

// runActiveTest 对指定账号执行一次认证层探活。
//
// 两步都成功才判定可用：判据端点确认会话有效，模型目录确认权限仍在。
// 只做第一步会把「会话活着但模型被收回」的账号误报为可用。
// [参数] sa: 账号；model: 面板选中的模型（仅回显）。
// [返回] 探活结果。
// 最近修改时间：2026-10-18；改动原因：新增，双条件探活内核。
func runActiveTest(sa *StoredAuth, model string) map[string]any {
	start := time.Now()
	res := testActiveResult{
		AuthID: sa.AuthID,
		Label:  sa.Label,
		Model:  model,
		Scope:  "auth+models",
	}

	info, errProbe := probeAccountInfo(sa.CookieHeader)
	if errProbe != nil {
		res.ElapsedMS = time.Since(start).Milliseconds()
		res.Error = errProbe.Error()
		return testActiveResultMap(res)
	}
	// 判据端点的 sec_user_id 必须与账号一致：不一致说明 Cookie 已被另一个
	// 账号复用（重新授权换了人），继续当作可用会写错归属。
	if got := info.secUserID(); got != "" && sa.SecUserID != "" && got != sa.SecUserID {
		res.ElapsedMS = time.Since(start).Milliseconds()
		res.Error = "登录态与账号不匹配：判据返回的 sec_user_id 与账号记录不同，请重新授权"
		return testActiveResultMap(res)
	}

	models, errDiscover := cachedDiscoveredModels(sa, true)
	if errDiscover != nil {
		res.ElapsedMS = time.Since(start).Milliseconds()
		res.Error = "会话有效，但模型目录不可用：" + errDiscover.Error()
		return testActiveResultMap(res)
	}
	res.ModelCount = len(models)

	// 面板选了模型时校验它确实在该账号目录里：选到别的账号的模型属于配置错误，
	// 提前报出来比等到真实推理失败更好排查。
	if model != "" && !modelInCatalog(models, model) {
		res.ElapsedMS = time.Since(start).Milliseconds()
		res.Error = fmt.Sprintf("模型 %s 不在该账号的可用目录中（共 %d 个模型）", model, len(models))
		return testActiveResultMap(res)
	}

	res.OK = true
	res.ElapsedMS = time.Since(start).Milliseconds()
	res.Message = fmt.Sprintf("会话有效，可见 %d 个模型", len(models))
	return testActiveResultMap(res)
}

// modelInCatalog 判断模型是否在目录中（忽略大小写与 :latest 后缀）。
// [参数] models: 模型目录；model: 待查模型名。
// [返回] 命中时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，模型存在性校验。
func modelInCatalog(models []pluginapi.ModelInfo, model string) bool {
	want := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(model, ":latest")))
	for _, m := range models {
		if strings.ToLower(m.ID) == want || strings.EqualFold(m.Name, model) {
			return true
		}
	}
	return false
}

// testActiveResultMap 把结果转成管理响应载荷。
// [参数] res: 探活结果。
// [返回] 载荷 map。
// 最近修改时间：2026-10-18；改动原因：新增，响应构造。
func testActiveResultMap(res testActiveResult) map[string]any {
	out := map[string]any{
		"ok":         res.OK,
		"auth_id":    res.AuthID,
		"elapsed_ms": res.ElapsedMS,
		"scope":      res.Scope,
	}
	if res.Label != "" {
		out["label"] = res.Label
	}
	if res.Model != "" {
		out["model"] = res.Model
	}
	if res.Message != "" {
		out["message"] = res.Message
	}
	if res.Error != "" {
		out["error"] = res.Error
	}
	if res.ModelCount > 0 {
		out["model_count"] = res.ModelCount
	}
	return out
}

// handleModelsQuery 返回指定账号可测试的模型列表。
//
// 面板「测试」弹窗用它填充模型下拉：优先动态目录，失败时回落到静态种子，
// 并在 notice 字段说明原因，避免把回落结果误当成账号真实模型。
// [参数] req: 管理请求，query 参数 auth_id 指定账号。
// [返回] {models:[...], source, notice}；缺少 auth_id 时返回 {error}。
// 最近修改时间：2026-10-18；改动原因：新增，测试弹窗模型数据源。
func handleModelsQuery(req pluginapi.ManagementRequest) map[string]any {
	authID := queryAuthID(req)
	if authID == "" {
		return map[string]any{"error": "auth_id 不能为空"}
	}

	sa, errFind := findAccountForTest(authID)
	if errFind != nil {
		return map[string]any{"error": errFind.Error()}
	}

	models, errDiscover := cachedDiscoveredModels(sa, false)
	source := "upstream"
	notice := ""
	if errDiscover != nil {
		models = staticFallbackModels()
		source = "fallback"
		notice = "模型目录拉取失败，当前为内置清单：" + errDiscover.Error()
	}

	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":           m.ID,
			"name":         m.Name,
			"display_name": m.DisplayName,
			"type":         m.Type,
		})
	}
	return map[string]any{"models": out, "source": source, "notice": notice}
}

// handleCreditsQuery 查询指定账号的积分余额与即将过期明细。
//
// force=true 时跳过缓存：面板「刷新」按钮需要拿到最新数字，
// 而列表接口默认走 5 分钟缓存以避免频繁打上游。
// [参数] req: 管理请求，query 参数 auth_id 指定账号，可选 force=1。
// [返回] 积分摘要；失败时带 error 字段。
// 最近修改时间：2026-10-18；改动原因：新增，面板积分查询入口。
func handleCreditsQuery(req pluginapi.ManagementRequest) map[string]any {
	authID := queryAuthID(req)
	if authID == "" {
		return map[string]any{"ok": false, "error": "auth_id 不能为空"}
	}
	sa, errFind := findAccountForTest(authID)
	if errFind != nil {
		return map[string]any{"ok": false, "error": errFind.Error()}
	}
	force := strings.TrimSpace(req.Query.Get("force")) == "1"
	summary, errFetch := cachedCreditSummary(sa, force)
	if errFetch != nil {
		return map[string]any{"ok": false, "error": errFetch.Error()}
	}
	out := creditSummaryMap(summary)
	out["ok"] = true
	return out
}

// creditSummaryMap 把积分摘要转成响应载荷。
// [参数] summary: 积分摘要。
// [返回] 载荷 map。
// 最近修改时间：2026-10-18；改动原因：新增，积分响应构造。
func creditSummaryMap(summary *creditSummary) map[string]any {
	if summary == nil {
		return map[string]any{}
	}
	out := map[string]any{
		"total":      summary.Total,
		"remain":     summary.Remain,
		"expiring":   summary.ExpiringRemain,
		"fetched_at": summary.FetchedAt,
	}
	if summary.ExpiringAt != "" {
		out["expiring_at"] = summary.ExpiringAt
	}
	if summary.Error != "" {
		out["error"] = summary.Error
	}
	if summary.DetailError != "" {
		out["detail_error"] = summary.DetailError
	}
	if len(summary.Items) > 0 {
		items := make([]map[string]any, 0, len(summary.Items))
		for _, item := range summary.Items {
			items = append(items, map[string]any{
				"title":      item.Title,
				"remaining":  item.Remaining,
				"expired_at": item.ExpiredAt,
			})
		}
		out["items"] = items
	}
	return out
}

// queryAuthID 从管理请求的 query 中提取账号标识。
// [参数] req: 管理请求。
// [返回] 账号标识；缺失时返回空串。
// 最近修改时间：2026-10-18；改动原因：新增，多个查询路由共用取值逻辑。
func queryAuthID(req pluginapi.ManagementRequest) string {
	for _, key := range []string{"auth_id", "auth_index"} {
		if vals := req.Query[key]; len(vals) > 0 {
			if v := strings.TrimSpace(vals[0]); v != "" {
				return v
			}
		}
	}
	return ""
}
