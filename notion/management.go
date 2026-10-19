package main

// 本文件实现插件的管理路由。
//
// 路由前缀随 plugin id 变化：/v0/management/plugins/notion-provider/*。
// 面板网页拿不到管理密钥，因此面板自身走 resource 路由（宿主免密钥放行），
// 数据接口走 management 路由。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	managementBasePath   string
	managementBasePathMu sync.RWMutex
)

// setManagementBasePath 设置管理路由前缀。
// [参数] base: 宿主下发的前缀。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，前缀注入。
func setManagementBasePath(base string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return
	}
	managementBasePathMu.Lock()
	defer managementBasePathMu.Unlock()
	managementBasePath = base
}

// getManagementBasePath 读取当前管理路由前缀。
// [参数] 无。
// [返回] 管理基础路径。
// 最近修改时间：2026-10-18；改动原因：新增，前缀读取。
func getManagementBasePath() string {
	managementBasePathMu.RLock()
	defer managementBasePathMu.RUnlock()
	return managementBasePath
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementRegistration 声明插件对宿主暴露的管理路由与资源页。
// [参数] 无。
// [返回] 路由与资源注册结构。
// 最近修改时间：2026-10-18；改动原因：新增，账号导入与管理路由。
func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "列出 Notion 账号。"},
			{Method: http.MethodPost, Path: base + "/import", Description: "导入 Cookie header（body: {cookie, label?, space_id?}）。"},
			{Method: http.MethodPost, Path: base + "/delete", Description: "删除一个 Notion 账号。"},
			{Method: http.MethodPost, Path: base + "/enable", Description: "启用一个 Notion 账号。"},
			{Method: http.MethodPost, Path: base + "/disable", Description: "禁用一个 Notion 账号。"},
			{Method: http.MethodPost, Path: base + "/set-space", Description: "切换账号使用的工作空间（body: {auth_id, space_id}）。"},
			{Method: http.MethodPost, Path: base + "/test-active", Description: "对单个账号发起一次推理探活（body: {auth_id, model?}）。"},
			{Method: http.MethodGet, Path: base + "/models", Description: "列出账号可见模型（query: auth_id）。"},
			{Method: http.MethodGet, Path: base + "/usage", Description: "查询账号各工作空间的额度（query: auth_id）。"},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "Notion", Description: "Notion Provider 管理面板：Cookie 导入、工作空间切换与探活。"},
		},
	}
}

// normalizeMgmtSubPath 归一化管理子路径，兼容宿主的多种前缀形态。
// [参数] rawPath: 宿主传入的原始路径。
// [返回] 归一化子路径与从路径解析出的查询参数。
// 最近修改时间：2026-10-18；改动原因：新增，兼容 resource 与 management 两种前缀。
func normalizeMgmtSubPath(rawPath string) (string, url.Values) {
	u, errParse := url.Parse(rawPath)
	path := rawPath
	var query url.Values
	if errParse == nil {
		path = u.Path
		query = u.Query()
	}
	path = strings.TrimRight(path, "/")

	// resource 路由：面板页本身走这里，宿主免密钥放行。
	resPrefix := "/v0/resource/plugins/" + providerName
	if strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		if sub == "" {
			return "/panel", query
		}
		return sub, query
	}

	fullPrefix := getManagementBasePath() + "/plugins/" + providerName
	shortPrefix := "/plugins/" + providerName
	switch {
	case strings.HasPrefix(path, fullPrefix):
		path = strings.TrimPrefix(path, fullPrefix)
	case strings.HasPrefix(path, shortPrefix):
		path = strings.TrimPrefix(path, shortPrefix)
	}
	if path == "" {
		path = "/"
	}
	return path, query
}

// handleManagement 分发插件管理路由。
// [参数] raw: 宿主 management.handle 载荷。
// [返回] 管理响应信封与错误。
// 最近修改时间：2026-10-18；改动原因：新增，路由分发。
func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	subPath, urlQuery := normalizeMgmtSubPath(req.Path)
	method := strings.ToUpper(req.Method)

	if method == http.MethodGet && (subPath == "/" || subPath == "/panel") {
		return handlePanelPage()
	}

	switch {
	case method == http.MethodGet && subPath == "/accounts":
		return handleListAccounts()

	case method == http.MethodPost && subPath == "/import":
		return handleImportAccount(req.Body)

	case method == http.MethodPost && subPath == "/delete":
		return handleDeleteAccount(req.Body)

	case method == http.MethodPost && subPath == "/enable":
		return handleSetAccountStatus(req.Body, false)

	case method == http.MethodPost && subPath == "/disable":
		return handleSetAccountStatus(req.Body, true)

	case method == http.MethodPost && subPath == "/set-space":
		return handleSetSpace(req.Body)

	case method == http.MethodPost && subPath == "/test-active":
		return handleTestActive(req.Body)

	case method == http.MethodGet && subPath == "/models":
		return makeHTTPResponse(http.StatusOK, mustMarshal(handleModelsQuery(req, urlQuery)))

	case method == http.MethodGet && subPath == "/usage":
		return makeHTTPResponse(http.StatusOK, mustMarshal(handleUsageQuery(req, urlQuery)))

	default:
		return makeHTTPResponse(http.StatusNotFound, []byte(`{"error":"not_found"}`))
	}
}

// makeHTTPResponse 把载荷包成宿主可识别的 HTTP 响应。
// [参数] status: 状态码；body: 响应体。
// [返回] 信封 JSON。
// 最近修改时间：2026-10-18；改动原因：新增，管理响应构造。
func makeHTTPResponse(status int, body []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": {"application/json"},
		},
		Body: body,
	})
}

// handlePanelPage 返回面板 HTML。
// [参数] 无。
// [返回] 管理响应。
// 最近修改时间：2026-10-18；改动原因：新增，面板渲染入口。
func handlePanelPage() ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  {"text/html; charset=utf-8"},
			"Cache-Control": {"no-cache, no-store, must-revalidate"},
		},
		Body: panelHTML,
	})
}

// accountView 是面板展示用的账号视图。
type accountView struct {
	AuthID    string        `json:"auth_id"`
	Label     string        `json:"label"`
	UserID    string        `json:"user_id"`
	SpaceID   string        `json:"space_id"`
	SpaceName string        `json:"space_name"`
	Spaces    []NotionSpace `json:"spaces"`
	Disabled  bool          `json:"disabled"`
	UpdatedAt string        `json:"updated_at"`
	// Available 表示上次探活是否通过；未知时为 null。
	Available *bool `json:"available"`
}

// handleListAccounts 列出全部账号。
// [参数] 无。
// [返回] 管理响应与错误。
// 最近修改时间：2026-10-18；改动原因：新增，账号列表。
func handleListAccounts() ([]byte, error) {
	accounts, _ := listAllAuthFiles()
	views := make([]accountView, 0, len(accounts))
	for _, auth := range accounts {
		views = append(views, accountView{
			AuthID:    auth.AuthID,
			Label:     auth.Label,
			UserID:    maskIdentifier(auth.UserID),
			SpaceID:   auth.SpaceID,
			SpaceName: auth.SpaceName,
			Spaces:    auth.Spaces,
			Disabled:  auth.Disabled,
			UpdatedAt: auth.UpdatedAt,
		})
	}
	return makeHTTPResponse(http.StatusOK, mustMarshal(map[string]any{"accounts": views}))
}

// importAccountRequest 是导入请求体。
type importAccountRequest struct {
	Cookie  string `json:"cookie"`
	Label   string `json:"label"`
	SpaceID string `json:"space_id"`
}

// handleImportAccount 导入一份 Cookie。
//
// 默认按工作空间拆分：一个账号下的商业版与免费版空间各自落盘为独立
// 账号，额度、冷却与故障转移互不影响。指定 space_id 时只导入该空间。
// [参数] body: 请求体。
// [返回] 管理响应与错误。
// 最近修改时间：2026-10-19；改动原因：返回按空间拆分后的账号列表。
func handleImportAccount(body json.RawMessage) ([]byte, error) {
	var req importAccountRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": "请求体格式错误"}))
	}
	if strings.TrimSpace(req.Cookie) == "" {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": "cookie 不能为空"}))
	}

	accounts, errSave := saveCredentials(req.Cookie, req.Label, req.SpaceID)
	if errSave != nil {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": errSave.Error()}))
	}

	views := make([]map[string]any, 0, len(accounts))
	for _, auth := range accounts {
		views = append(views, map[string]any{
			"auth_id":    auth.AuthID,
			"label":      auth.Label,
			"space_id":   auth.SpaceID,
			"space_name": auth.SpaceName,
		})
	}
	return makeHTTPResponse(http.StatusOK, mustMarshal(map[string]any{
		"accounts": views,
		"count":    len(views),
	}))
}

// authIDRequest 是只带账号标识的请求体。
type authIDRequest struct {
	AuthID string `json:"auth_id"`
}

// handleDeleteAccount 删除账号。
// [参数] body: 请求体。
// [返回] 管理响应与错误。
// 最近修改时间：2026-10-18；改动原因：新增，账号删除。
func handleDeleteAccount(body json.RawMessage) ([]byte, error) {
	var req authIDRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": "请求体格式错误"}))
	}
	if errDelete := deleteAuthFileDirect(req.AuthID); errDelete != nil {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": errDelete.Error()}))
	}
	return makeHTTPResponse(http.StatusOK, mustMarshal(map[string]bool{"ok": true}))
}

// handleSetAccountStatus 启用或禁用账号。
// [参数] body: 请求体；disabled: 目标状态。
// [返回] 管理响应与错误。
// 最近修改时间：2026-10-18；改动原因：新增，账号启停。
func handleSetAccountStatus(body json.RawMessage, disabled bool) ([]byte, error) {
	var req authIDRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": "请求体格式错误"}))
	}

	auth, errFind := findAccount(req.AuthID)
	if errFind != nil {
		return makeHTTPResponse(http.StatusNotFound, mustMarshal(map[string]string{"error": errFind.Error()}))
	}
	auth.Disabled = disabled
	auth.UpdatedAt = time.Now().Format(time.RFC3339)
	if errWrite := writeAuthFileDirect(auth.PhysicalPath, auth); errWrite != nil {
		return makeHTTPResponse(http.StatusInternalServerError, mustMarshal(map[string]string{"error": errWrite.Error()}))
	}
	return makeHTTPResponse(http.StatusOK, mustMarshal(map[string]bool{"ok": true}))
}

// setSpaceRequest 是切换工作空间的请求体。
type setSpaceRequest struct {
	AuthID  string `json:"auth_id"`
	SpaceID string `json:"space_id"`
}

// handleSetSpace 把账号移动到另一个工作空间。
//
// 账号已按空间拆分，标识由「用户 + 空间」派生，因此切换空间必须连同
// 账号文件一起改名，否则会出现两个账号指向同一空间、并发消耗同一份额度。
// 目标空间已有独立账号时直接拒绝，让用户在面板上删掉多余的那个。
// [参数] body: 请求体。
// [返回] 管理响应与错误。
// 最近修改时间：2026-10-19；改动原因：按空间拆分后改为移动语义并防重复。
func handleSetSpace(body json.RawMessage) ([]byte, error) {
	var req setSpaceRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": "请求体格式错误"}))
	}

	auth, errFind := findAccount(req.AuthID)
	if errFind != nil {
		return makeHTTPResponse(http.StatusNotFound, mustMarshal(map[string]string{"error": errFind.Error()}))
	}

	var target *NotionSpace
	for i := range auth.Spaces {
		if strings.EqualFold(auth.Spaces[i].ID, strings.TrimSpace(req.SpaceID)) {
			target = &auth.Spaces[i]
			break
		}
	}
	if target == nil {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": "该工作空间不在账号下"}))
	}
	if target.ID == auth.SpaceID {
		return makeHTTPResponse(http.StatusOK, mustMarshal(map[string]any{
			"space_id":   auth.SpaceID,
			"space_name": auth.SpaceName,
			"auth_id":    auth.AuthID,
		}))
	}

	newID := authIDFor(&CookieIdentity{UserID: auth.UserID, TokenV2: auth.TokenV2}, target.ID)
	if existing, errExisting := findAccount(newID); errExisting == nil && existing.AuthID != auth.AuthID {
		return makeHTTPResponse(http.StatusConflict, mustMarshal(map[string]string{
			"error": "该工作空间已是独立账号 " + existing.Label + "，无需移动；如需合并请先删除其中一个",
		}))
	}

	oldPath := auth.PhysicalPath
	auth.AuthID = newID
	auth.SpaceID = target.ID
	auth.SpaceName = target.Name
	auth.UpdatedAt = time.Now().Format(time.RFC3339)
	auth.PhysicalPath = filepath.Join(getAccountsDir(), newID+notionAuthFileSuffix)
	if errWrite := writeAuthFileDirect(auth.PhysicalPath, auth); errWrite != nil {
		return makeHTTPResponse(http.StatusInternalServerError, mustMarshal(map[string]string{"error": errWrite.Error()}))
	}
	// 新文件已就位，旧文件改名归档：留着会被当成第二个账号重复路由。
	if oldPath != "" && oldPath != auth.PhysicalPath && isSafeNotionAuthPath(oldPath) {
		_ = os.Rename(oldPath, oldPath+".migrated")
	}
	// 切空间后旧 thread 属于旧空间，必须整批清掉，否则续写会落到错误空间。
	forgetSessionsForAuth(auth)

	return makeHTTPResponse(http.StatusOK, mustMarshal(map[string]any{
		"space_id":   auth.SpaceID,
		"space_name": auth.SpaceName,
		"auth_id":    auth.AuthID,
	}))
}

// testActiveRequest 是探活请求体。
type testActiveRequest struct {
	AuthID string `json:"auth_id"`
	Model  string `json:"model"`
}

// handleTestActive 对账号发起一次最小推理探活。
//
// 必须走真实的推理链路而不是只查模型列表：模型可见不代表能出正文，
// 静默空流只有真正跑一轮才会暴露。
// [参数] body: 请求体。
// [返回] 管理响应与错误。
// 最近修改时间：2026-10-18；改动原因：新增，面板测试按钮。
func handleTestActive(body json.RawMessage) ([]byte, error) {
	var req testActiveRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return makeHTTPResponse(http.StatusBadRequest, mustMarshal(map[string]string{"error": "请求体格式错误"}))
	}

	auth, errFind := findAccount(req.AuthID)
	if errFind != nil {
		return makeHTTPResponse(http.StatusNotFound, mustMarshal(map[string]string{"error": errFind.Error()}))
	}

	model := req.Model
	if strings.TrimSpace(model) == "" {
		model = defaultNotionModel
	}

	start := time.Now()
	chatReq := &openAIChatRequest{
		Model:        model,
		MessagesText: []chatMessage{{Role: "user", Content: "只回复两个字符：OK"}},
	}
	result, errRun := runInference(context.Background(), auth, chatReq, nil)
	elapsed := time.Since(start).Round(time.Millisecond).String()

	if errRun != nil {
		// 探活失败与真实请求失败口径一致：计入账号级冷却，
		// 否则面板测出账号坏了但路由仍把流量打过去。
		cfg := currentConfig()
		noteAccountFailure(auth.AuthID, classifyInferenceError(errRun), cfg.CooldownSeconds)
		return makeHTTPResponse(http.StatusOK, mustMarshal(map[string]any{
			"ok":      false,
			"error":   errRun.Error(),
			"elapsed": elapsed,
		}))
	}
	resetAccountFailover(auth.AuthID)
	return makeHTTPResponse(http.StatusOK, mustMarshal(map[string]any{
		"ok":         true,
		"elapsed":    elapsed,
		"reply":      truncateForError([]byte(result.text)),
		"thread":     result.threadID,
		"model":      result.model,
		"in_tokens":  result.inputTokens,
		"out_tokens": result.outputTokens,
	}))
}

// handleModelsQuery 返回账号可见的模型列表。
// [参数] req: 管理请求；query: 查询参数。
// [返回] 响应体。
// 最近修改时间：2026-10-18；改动原因：新增，面板模型下拉。
func handleModelsQuery(req pluginapi.ManagementRequest, query url.Values) map[string]any {
	authID := query.Get("auth_id")
	if authID == "" {
		authID = req.Query.Get("auth_id")
	}
	auth, errFind := findAccount(authID)
	if errFind != nil {
		return map[string]any{"error": errFind.Error()}
	}

	codes, errDiscover := discoverModels(auth)
	if errDiscover != nil || len(codes) == 0 {
		codes = enabledNotionModels()
	}

	type modelView struct {
		Code    string `json:"code"`
		Display string `json:"display"`
	}
	out := make([]modelView, 0, len(codes))
	for _, code := range codes {
		display := notionModelDisplayNames[code]
		if display == "" {
			display = code
		}
		out = append(out, modelView{Code: code, Display: display})
	}
	return map[string]any{"models": out}
}

// handleUsageQuery 返回账号各工作空间的额度。
//
// 面板的「刷新额度」按钮与页面启动时的自动补刷都走这里：每次都现场
// 请求上游并把结果写回账号文件。
// [参数] req: 管理请求；query: 查询参数。
// [返回] 响应体。
// 最近修改时间：2026-10-19；改动原因：刷新结果落盘，修复刷新后仍显示旧快照。
func handleUsageQuery(req pluginapi.ManagementRequest, query url.Values) map[string]any {
	authID := query.Get("auth_id")
	if authID == "" {
		authID = req.Query.Get("auth_id")
	}
	auth, errFind := findAccount(authID)
	if errFind != nil {
		return map[string]any{"error": errFind.Error()}
	}

	if errRefresh := refreshUsageForAuth(auth); errRefresh != nil {
		return map[string]any{
			"auth_id": auth.AuthID,
			"error":   errRefresh.Error(),
			"spaces":  auth.Spaces,
		}
	}

	return map[string]any{
		"auth_id":    auth.AuthID,
		"space_id":   auth.SpaceID,
		"space_name": auth.SpaceName,
		"spaces":     auth.Spaces,
	}
}

// usageHTTPClientFn 构造额度查询用的客户端，测试可注入替身。
var usageHTTPClientFn = ensureHTTPClient

// refreshUsageForAuth 实时拉取额度并写回账号文件。
//
// 两步必须绑在一起：只刷新不落盘时，面板下一次读 /accounts 又会退回
// 导入时的旧快照，手动刷新看起来「没生效」。
// [参数] auth: 目标账号。
// [返回] 错误。
// 最近修改时间：2026-10-19；改动原因：抽出额度刷新链路，补齐落盘。
func refreshUsageForAuth(auth *StoredAuth) error {
	if auth == nil {
		return fmt.Errorf("账号为空")
	}
	// 用实时值覆盖落盘快照：余额会随使用变化，快照只用于离线展示。
	cfg := currentConfig()
	cl := usageHTTPClientFn(notionRequestTimeout())
	ctx, cancel := context.WithTimeout(context.Background(), notionRequestTimeout())
	defer cancel()
	enrichSpaceUsage(ctx, cl, scopeForAuth(auth, cfg.ClientVersion), auth.Spaces)

	if errPersist := persistUsageSnapshot(auth); errPersist != nil {
		return fmt.Errorf("额度已刷新，但写回账号文件失败：%w", errPersist)
	}
	return nil
}

// persistUsageSnapshot 把刷新后的空间额度写回账号文件。
//
// 只更新 Spaces 与 UpdatedAt，其余字段原样保留；写盘复用带防覆盖校验的
// 原子写入口，避免刷新额度时盖掉用户刚重新导入的凭据。
// [参数] auth: 已就地更新额度的账号。
// [返回] 写入错误。
// 最近修改时间：2026-10-19；改动原因：新增，刷新额度后同步落盘。
func persistUsageSnapshot(auth *StoredAuth) error {
	if auth == nil || auth.PhysicalPath == "" {
		return nil
	}
	// 多空间账号拆成多个文件：每个文件都保留完整空间列表，额度必须同步，
	// 否则另一张卡片会一直显示旧数字。
	for _, sibling := range siblingAuthFiles(auth) {
		sibling.Spaces = auth.Spaces
		sibling.UpdatedAt = time.Now().Format(time.RFC3339)
		if errWrite := writeAuthFileDirect(sibling.PhysicalPath, sibling); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

// siblingAuthFiles 返回与给定账号同属一份 Cookie 的全部账号文件。
//
// 识别口径是「同 UserID」：同一用户的不同空间共用一个登录态，额度快照
// 结构一致。用户已单独重新导入、token 不同的文件也会被一起刷新——它的
// 额度同样来自同一个上游账号，刷新不会改写凭据本身。
// [参数] auth: 当前账号。
// [返回] 包含当前账号在内的文件列表。
// 最近修改时间：2026-10-19；改动原因：新增，拆分账号的额度同步。
func siblingAuthFiles(auth *StoredAuth) []*StoredAuth {
	if auth == nil {
		return nil
	}
	accounts, errList := listAllAuthFiles()
	if errList != nil {
		return []*StoredAuth{auth}
	}

	out := make([]*StoredAuth, 0, 2)
	for _, account := range accounts {
		if account.PhysicalPath == "" {
			continue
		}
		if account.PhysicalPath == auth.PhysicalPath {
			out = append(out, account)
			continue
		}
		if account.UserID != "" && account.UserID == auth.UserID {
			out = append(out, account)
		}
	}
	if len(out) == 0 {
		return []*StoredAuth{auth}
	}
	return out
}

// findAccount 按标识查找账号。
// [参数] authID: 账号标识。
// [返回] 账号与错误。
// 最近修改时间：2026-10-18；改动原因：新增，账号查找。
func findAccount(authID string) (*StoredAuth, error) {
	authID = strings.TrimSuffix(strings.TrimSpace(authID), notionAuthFileSuffix)
	accounts, _ := listAllAuthFiles()
	for _, auth := range accounts {
		if auth.AuthID == authID {
			return auth, nil
		}
	}
	return nil, errAccountNotFound
}

// errAccountNotFound 表示账号不存在。
var errAccountNotFound = &accountNotFoundError{}

type accountNotFoundError struct{}

// Error 实现 error 接口。
// [参数] 无。
// [返回] 错误描述。
// 最近修改时间：2026-10-18；改动原因：新增，标准错误形态。
func (e *accountNotFoundError) Error() string { return "账号不存在" }
