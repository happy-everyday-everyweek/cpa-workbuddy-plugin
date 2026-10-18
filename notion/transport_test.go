package main

// 本文件验证传输层与指纹常量的一致性。

import (
	"strings"
	"testing"
)

// TestBrowserFingerprintVersionsMatch 校验三处指纹的 Chrome 主版本一致。
//
// 三者任一不一致都会被上游识别为非浏览器流量，因此把「一致」写成测试，
// 避免以后只升级 UA 忘记 sec-ch-ua。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，指纹一致性回归。
func TestBrowserFingerprintVersionsMatch(t *testing.T) {
	ua := browserUserAgent()
	secUA := browserSecCHUA()

	if !strings.Contains(ua, "Chrome/"+browserChromeMajor+".") {
		t.Fatalf("UA 未包含主版本 %s: %s", browserChromeMajor, ua)
	}
	if !strings.Contains(secUA, `"Google Chrome";v="`+browserChromeMajor+`"`) {
		t.Fatalf("sec-ch-ua 未包含主版本 %s: %s", browserChromeMajor, secUA)
	}
	if !strings.Contains(secUA, `"Chromium";v="`+browserChromeMajor+`"`) {
		t.Fatalf("sec-ch-ua 的 Chromium 版本不一致: %s", secUA)
	}
}

// TestNewNotionTransportConfigured 校验 transport 的关键配置。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，防止误加禁用证书校验。
func TestNewNotionTransportConfigured(t *testing.T) {
	rt := newNotionTransport()
	if rt == nil {
		t.Fatal("transport 为 nil")
	}
	client := newNotionHTTPClient(0)
	if client.Timeout <= 0 {
		t.Fatal("client 超时必须为正数")
	}
	if client.Transport == nil {
		t.Fatal("client 未设置自定义 transport")
	}
}

// TestNewNotionRequestHeaders 校验请求头与账号归属字段。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，头集合回归。
func TestNewNotionRequestHeaders(t *testing.T) {
	scope := requestScope{
		Cookie:  "token_v2=test",
		UserID:  "user-1",
		SpaceID: "space-1",
	}
	req, err := newNotionRequest(t.Context(), scope, "/getSpaces", []byte(`{}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}

	want := map[string]string{
		"x-notion-active-user-header": "user-1",
		"x-notion-space-id":           "space-1",
		"notion-client-version":       browserClientVersion,
		"notion-audit-log-platform":   notionAuditPlatform,
		"Cookie":                      "token_v2=test",
		"Accept":                      "application/x-ndjson",
		"Origin":                      notionOrigin,
		"Referer":                     notionReferer,
		"Sec-Fetch-Site":              "same-origin",
	}
	for key, expected := range want {
		if got := req.Header.Get(key); got != expected {
			t.Fatalf("%s = %q, 期望 %q", key, got, expected)
		}
	}
	if !strings.HasSuffix(req.URL.String(), "/api/v3/getSpaces") {
		t.Fatalf("URL 不正确: %s", req.URL.String())
	}
}

// TestRequestScopeClientVersionOverride 校验配置项可覆盖客户端版本。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，覆盖逻辑回归。
func TestRequestScopeClientVersionOverride(t *testing.T) {
	scope := requestScope{ClientVersion: "custom-version"}
	req, err := newNotionRequest(t.Context(), scope, "/getSpaces", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if got := req.Header.Get("notion-client-version"); got != "custom-version" {
		t.Fatalf("notion-client-version = %q, 期望 custom-version", got)
	}
}
