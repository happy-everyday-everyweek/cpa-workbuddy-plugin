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

// TestGrowthRouteRemoved 锁定成长中心整体下线：面板按钮、管理路由、写路径判定
// 三处都不得再出现 /growth。用户裁定只保留签到，不再做任务领取。
func TestGrowthRouteRemoved(t *testing.T) {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	for _, r := range managementRegistration().Routes {
		if strings.HasSuffix(r.Path, "/growth") {
			t.Fatalf("成长中心已下线，但路由仍注册：%s %s", r.Method, r.Path)
		}
	}
	if mutatingManagementPath(base + "/growth") {
		t.Fatal("/growth 不再是管理路径，不得保留在 mutatingManagementPath 中")
	}
}

// TestHandleModelsQueryRequiresAuthIndex 验证缺少 auth_index 时返回结构化错误。
func TestHandleModelsQueryRequiresAuthIndex(t *testing.T) {
	res := handleModelsQuery(pluginapi.ManagementRequest{})
	if res["error"] == nil {
		t.Fatalf("expected error when auth_index missing, got %v", res)
	}
}
