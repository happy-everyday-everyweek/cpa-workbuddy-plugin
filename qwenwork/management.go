// management.go implements the QwenWork management API and web panel:
// account dashboard (nickname, credits, plan) and quota refresh.
package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// Account listing via host auth callbacks
// -----------------------------------------------------------------------------

type creditsSummary struct {
	// TotalRemain is currently usable credits across all active packages.
	TotalRemain int64 `json:"total_remain"`
	// TotalUsed is consumed credits in the current cycle (sum of packages).
	TotalUsed int64 `json:"total_used"`
	// TotalSize is the credit capacity/pool (sum of package sizes). remain+used ≈ size.
	TotalSize int64 `json:"total_size"`
	// PackCount is number of resource packages included in the aggregate.
	PackCount int `json:"pack_count"`
	// FetchedAt is when this snapshot was taken (RFC3339). Upstream billing lag
	// can make remain/used look "stuck" for minutes after chat; compare this
	// timestamp — not only the numbers — when diagnosing frozen credits.
	FetchedAt string `json:"fetched_at,omitempty"`
	// Exceeded 表示上游 account-context 明确标记 quota.exceeded=true。
	// 该信号是「确定耗尽」的权威判据：免费版账号没有额度包时
	// total_used/total_size 恒为 0，仅靠数值无法与「未知」区分。
	Exceeded bool             `json:"exceeded,omitempty"`
	Packages []packageSummary `json:"packages"`
}

type packageSummary struct {
	Name       string `json:"name"`
	Remain     int64  `json:"remain"`
	Used       int64  `json:"used"`
	Size       int64  `json:"size"`
	CycleStart string `json:"cycle_start"`
	CycleEnd   string `json:"cycle_end"`
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

// managementBasePathCache holds the host-injected BasePath so handleManagement
// doesn't hardcode /v0/management. Falls back to the historical default if the
// host doesn't provide one (older CPA builds).
var (
	managementBasePathCache   = "/v0/management"
	managementBasePathCacheMu sync.RWMutex
)

func loadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List QwenWork accounts with credits and plan."},
			{Method: http.MethodPost, Path: base + "/refresh", Description: "Enqueue an async throttled refresh of all accounts (1s per account)."},
			{Method: http.MethodGet, Path: base + "/refresh/status", Description: "Poll the throttled refresh progress (per-account status)."},
			{Method: http.MethodGet, Path: base + "/credits", Description: "Get real-time credits for one (auth_index query) or all accounts."},
			{Method: http.MethodPost, Path: base + "/import-cred", Description: "Restore one credential JSON previously exported by /export (structural check only, no upstream exchange)."},
			{Method: http.MethodGet, Path: base + "/export", Description: "Export all QwenWork credentials as a single JSON backup document (raw physical files, re-importable via /import-cred)."},
			{Method: http.MethodPost, Path: base + "/select", Description: "Select the active account card used for chat routing (body: {auth_index})."},
			{Method: http.MethodPost, Path: base + "/keepalive", Description: "Manually refresh access tokens for all accounts (or one with auth_index)."},
			{Method: http.MethodGet, Path: base + "/keepalive/status", Description: "Last keepalive run summary + config."},
			{Method: http.MethodPost, Path: base + "/delete", Description: "Delete one QwenWork account and its physical auth file (body: {auth_index})."},
			{Method: http.MethodPost, Path: base + "/clear-cooldown", Description: "Manually clear 24h daily cooling limit for an account (body: {auth_index})."},
			{Method: http.MethodPost, Path: base + "/test-active", Description: "Run one manual active inference test for an account (body: {auth_index, model?})."},
			{Method: http.MethodGet, Path: base + "/models", Description: "List the models available to one account for the panel test picker (query: auth_index)."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "QwenWork", Description: "QwenWork dashboard: credits, plan, import."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser UI resource routes (unauthenticated).
	resPrefix := "/v0/resource/plugins/" + providerName
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		return okEnvelope(mgmtHTMLResponse(servePanel(sub)))
	}

	// Plugin-layer auth for mutating endpoints (defence-in-depth on top of
	// the host middleware; skipped when no management_key is configured).
	if req.Method == http.MethodPost || mutatingManagementPath(path) {
		if status, msg := checkManagementAuth(req); status != 0 {
			return okEnvelope(mgmtJSONResponse(status, map[string]any{"error": msg}))
		}
	}

	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(false, false)))
	case req.Method == http.MethodPost && path == base+"/refresh":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleRefreshAsync()))
	case req.Method == http.MethodGet && path == base+"/refresh/status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, globalRefresh.Snapshot()))
	case req.Method == http.MethodGet && path == base+"/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCreditsQuery(req)))
	case req.Method == http.MethodPost && path == base+"/import-cred":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImportCred(req)))
	case req.Method == http.MethodGet && path == base+"/export":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleExportAuth()))
	case req.Method == http.MethodPost && path == base+"/select":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleSelectAuth(req)))
	case req.Method == http.MethodPost && path == base+"/keepalive":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveNow(req)))
	case req.Method == http.MethodGet && path == base+"/keepalive/status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveStatus()))
	case req.Method == http.MethodPost && path == base+"/delete":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleDeleteAuth(req)))
	case req.Method == http.MethodPost && path == base+"/clear-cooldown":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleClearCooldown(req)))
	case req.Method == http.MethodPost && path == base+"/test-active":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleTestActive(req)))
	case req.Method == http.MethodGet && path == base+"/models":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleModelsQuery(req)))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

// handleClearCooldown 手动解除指定账号的 24 小时每日调用上限冷却状态。
// [参数] req: 管理接口请求体，含 auth_index 或 auth_id。
// [返回] 解除成功状态或错误信息。
func handleClearCooldown(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
		AuthID    string `json:"auth_id"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	authID := strings.TrimSpace(body.AuthID)
	if authIndex == "" && authID == "" {
		return map[string]any{"error": "auth_index or auth_id is required"}
	}
	if err := clearDailyCooldown(authIndex, authID); err != nil {
		return map[string]any{"error": "clear cooling failed: " + err.Error(), "auth_index": authIndex}
	}
	return map[string]any{
		"ok":         true,
		"auth_index": authIndex,
		"auth_id":    authID,
		"message":    "daily cooling cleared successfully",
	}
}

// handleRefreshAsync enqueues a full-fleet refresh through the throttled
// RefreshRunner and returns immediately. The panel polls GET /refresh/status
// for per-account progress instead of blocking on the whole batch.
func handleRefreshAsync() map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	targets := make([]refreshTarget, 0, len(files))
	for _, f := range files {
		if strings.TrimSpace(f.AuthIndex) == "" || strings.TrimSpace(f.ID) == "" {
			continue
		}
		targets = append(targets, refreshTarget{AuthIndex: f.AuthIndex, AuthID: f.ID})
	}
	queued := globalRefresh.EnqueueAll(targets, "panel")
	return map[string]any{
		"started": queued > 0,
		"source":  "panel",
		"queued":  queued,
	}
}

// -----------------------------------------------------------------------------
// Plugin-layer management auth
// -----------------------------------------------------------------------------
//
// When management_key is configured (config_yaml or WB_MANAGEMENT_KEY env), all
// mutating endpoints under /v0/management/plugins/qwenwork/* require a matching
// Bearer token. Read-only GET endpoints (accounts/credits/panel) pass through so
// the panel can render before the user has pasted a key — the panel itself
// supplies the key on every call via Authorization header.
func loadedManagementKey() string {
	managementAPIKeyMu.RLock()
	defer managementAPIKeyMu.RUnlock()
	return managementAPIKey
}

// checkManagementAuth returns an HTTP status + error message when the request
// should be rejected. status=0 means allow.
// checkManagementAuth mirrors the community-plugin convention (grok-panel):
// trust the host middleware. The host already authenticated the request with
// the CPA management key before forwarding to the plugin — re-checking here
// would force operators to configure a second key just for the plugin.
//
// If a management_key is explicitly configured (config_yaml management_key: or
// WB_MANAGEMENT_KEY env), we enforce it as defence-in-depth on top of the
// host check. Otherwise (the default) we return 0 and let the request through.
func checkManagementAuth(req pluginapi.ManagementRequest) (int, string) {
	want := loadedManagementKey()
	if want == "" {
		return 0, "" // trust host middleware (community convention)
	}
	got := strings.TrimSpace(req.Headers.Get("Authorization"))
	if !strings.HasPrefix(got, "Bearer ") {
		return http.StatusUnauthorized, "missing Bearer token"
	}
	token := strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return http.StatusForbidden, "invalid management key"
	}
	return 0, ""
}

// mutatingManagementPath reports whether the path performs a write (import,
// select, refresh, keepalive, delete). Read endpoints pass.
func mutatingManagementPath(path string) bool {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch path {
	case base + "/refresh",
		base + "/import-cred",
		base + "/export",
		base + "/select",
		base + "/keepalive",
		base + "/delete":
		return true
	}
	return false
}

// handleExportAuth returns every QwenWork credential as a parsed JSON backup
// ({version, exported_at, plugin, count, accounts:[{name, auth_index, uid,
// nickname, credential}]}). The frontend downloads it as a dated file; the
// same wrapper can be split by the panel and re-imported via /import-cred.
// Carries full credentials — kept in mutatingManagementPath so the management
// key is required despite being GET.
func handleExportAuth() map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": "host.auth.list failed: " + err.Error(), "count": 0, "accounts": []any{}}
	}
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		if strings.TrimSpace(f.AuthIndex) == "" {
			continue
		}
		a, phys, gerr := hostAuthGetBundle(f.AuthIndex)
		if gerr != nil || phys == nil {
			out = append(out, map[string]any{
				"name":       f.Name,
				"auth_index": f.AuthIndex,
				"load_error": errString(gerr),
			})
			continue
		}
		var cred any
		_ = json.Unmarshal(phys.JSON, &cred)
		entry := map[string]any{
			"name":       f.Name,
			"auth_index": f.AuthIndex,
			"credential": cred,
		}
		if a != nil {
			entry["uid"] = a.Account.UID
			entry["nickname"] = a.Account.Nickname
		}
		out = append(out, entry)
	}
	return map[string]any{
		"version":     1,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"plugin":      providerName,
		"count":       len(out),
		"accounts":    out,
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}
