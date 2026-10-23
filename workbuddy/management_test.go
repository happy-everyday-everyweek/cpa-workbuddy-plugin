package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestManagementRegistrationModelsRoute 验证面板「测试」弹窗依赖的只读 /models
// 路由已注册为 GET，且不会被误列为需要 management key 的写路径。
func TestManagementRegistrationModelsRoute(t *testing.T) {
	reg := managementRegistration()
	found := false
	for _, r := range reg.Routes {
		if strings.HasSuffix(r.Path, "/models") {
			found = true
			if r.Method != http.MethodGet {
				t.Fatalf("/models route method = %s, want GET", r.Method)
			}
		}
	}
	if !found {
		t.Fatal("/models route missing from managementRegistration().Routes")
	}
	base := loadedManagementBasePath() + "/plugins/" + providerName
	if mutatingManagementPath(base + "/models") {
		t.Fatal("/models must stay a read-only path (not in mutatingManagementPath)")
	}
}

// TestGrowthRouteRegistered 锁定成长中心入口在位且受写路径保护。
// 2026-10-24 起成长中心重新接入（原 TestGrowthRouteRemoved 负向守卫已作废）：
// 路由必须存在、方法必须是 POST、且必须列入 mutatingManagementPath，
// 防止后续重构把写入口悄悄降级成免鉴权的只读路径。
func TestGrowthRouteRegistered(t *testing.T) {
	reg := managementRegistration()
	found := false
	for _, r := range reg.Routes {
		if strings.HasSuffix(r.Path, "/growth") {
			found = true
			if r.Method != http.MethodPost {
				t.Fatalf("/growth route method = %s, want POST", r.Method)
			}
		}
	}
	if !found {
		t.Fatal("/growth route missing from managementRegistration().Routes")
	}
	base := loadedManagementBasePath() + "/plugins/" + providerName
	if !mutatingManagementPath(base + "/growth") {
		t.Fatal("/growth must be a mutating path so management auth applies")
	}
}

// TestHandleModelsQueryRequiresAuthIndex 验证缺少 auth_index 时返回结构化错误。
func TestHandleModelsQueryRequiresAuthIndex(t *testing.T) {
	res := handleModelsQuery(pluginapi.ManagementRequest{})
	if res["error"] == nil {
		t.Fatalf("expected error when auth_index missing, got %v", res)
	}
}
