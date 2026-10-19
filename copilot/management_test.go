package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAccountRoutable 校验账号可调度资格的各个否决条件。
func TestAccountRoutable(t *testing.T) {
	// 1. nil 账号不可调度。
	if accountRoutable(nil, "") {
		t.Fatal("nil account must not be routable")
	}

	// 2. 无凭据不可调度。
	if accountRoutable(&StoredAuth{AuthID: "copilot-a"}, "") {
		t.Fatal("account without credential must not be routable")
	}

	// 3. 已禁用不可调度。
	if accountRoutable(&StoredAuth{AuthID: "copilot-a", GitHubToken: "t", Disabled: true}, "") {
		t.Fatal("disabled account must not be routable")
	}

	// 4. 测试失败标记不可调度。
	if accountRoutable(&StoredAuth{AuthID: "copilot-a", GitHubToken: "t", TestFailed: true}, "") {
		t.Fatal("test-failed account must not be routable")
	}

	// 5. 冷却中不可调度，冷却解除后恢复。
	noteAccountFailure("copilot-a", 429, "rate limit")
	if accountRoutable(&StoredAuth{AuthID: "copilot-a", GitHubToken: "t"}, "") {
		t.Fatal("cooling-down account must not be routable")
	}
	resetAccountFailover("copilot-a")

	// 6. 正常账号可调度。
	if !accountRoutable(&StoredAuth{AuthID: "copilot-a", GitHubToken: "t"}, "") {
		t.Fatal("healthy account must be routable")
	}
}

// TestPickActiveAuthRoundRobin 校验选号在多个可用账号间轮转且跳过不可用账号。
func TestPickActiveAuthRoundRobin(t *testing.T) {
	tmpDir := t.TempDir()
	setAccountsDir(tmpDir)
	defer setAccountsDir("")

	// 落盘 3 个账号：1 个禁用、2 个可用。
	mustWriteAccount(t, tmpDir, &StoredAuth{AuthID: "copilot-a", GitHubToken: "gho_a"})
	mustWriteAccount(t, tmpDir, &StoredAuth{AuthID: "copilot-b", GitHubToken: "gho_b"})
	mustWriteAccount(t, tmpDir, &StoredAuth{AuthID: "copilot-c", GitHubToken: "gho_c", Disabled: true})

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		sa := pickActiveAuth("")
		if sa == nil {
			t.Fatal("pickActiveAuth returned nil with 2 routable accounts")
		}
		if sa.AuthID == "copilot-c" {
			t.Fatal("disabled account must never be picked")
		}
		seen[sa.AuthID]++
	}
	// 两个可用账号都应被轮转到（各 3 次）。
	if seen["copilot-a"] != 3 || seen["copilot-b"] != 3 {
		t.Fatalf("round-robin distribution wrong: %v", seen)
	}
}

// TestPickActiveAuthNoCandidate 校验无可用账号时返回 nil。
func TestPickActiveAuthNoCandidate(t *testing.T) {
	tmpDir := t.TempDir()
	setAccountsDir(tmpDir)
	defer setAccountsDir("")

	mustWriteAccount(t, tmpDir, &StoredAuth{AuthID: "copilot-a", GitHubToken: "gho_a", Disabled: true})
	if sa := pickActiveAuth(""); sa != nil {
		t.Fatalf("expected nil when all accounts disabled, got %s", sa.AuthID)
	}
}

// TestPickNextAuth 校验换号时排除当前账号且跳过不可用账号。
func TestPickNextAuth(t *testing.T) {
	tmpDir := t.TempDir()
	setAccountsDir(tmpDir)
	defer setAccountsDir("")

	mustWriteAccount(t, tmpDir, &StoredAuth{AuthID: "copilot-a", GitHubToken: "gho_a"})
	mustWriteAccount(t, tmpDir, &StoredAuth{AuthID: "copilot-b", GitHubToken: "gho_b"})

	// 1. 从 a 换号应拿到 b。
	next, ok := pickNextAuth("copilot-a", "")
	if !ok || next == nil || next.AuthID != "copilot-b" {
		t.Fatalf("pickNextAuth from a = %v/%v", next, ok)
	}

	// 2. 只有一个账号时无法换号。
	tmpDir2 := t.TempDir()
	setAccountsDir(tmpDir2)
	mustWriteAccount(t, tmpDir2, &StoredAuth{AuthID: "copilot-a", GitHubToken: "gho_a"})
	if _, ok := pickNextAuth("copilot-a", ""); ok {
		t.Fatal("single account must not yield a next candidate")
	}

	// 3. 除当前账号外全部不可用时无法换号。
	tmpDir3 := t.TempDir()
	setAccountsDir(tmpDir3)
	mustWriteAccount(t, tmpDir3, &StoredAuth{AuthID: "copilot-a", GitHubToken: "gho_a"})
	mustWriteAccount(t, tmpDir3, &StoredAuth{AuthID: "copilot-b", GitHubToken: "gho_b", Disabled: true})
	if _, ok := pickNextAuth("copilot-a", ""); ok {
		t.Fatal("disabled-only peer must not yield a next candidate")
	}
}

// TestHandleSchedulerPick 校验无账号时不抢占调度（Handled=false）。
func TestHandleSchedulerPick(t *testing.T) {
	tmpDir := t.TempDir()
	setAccountsDir(tmpDir)
	defer setAccountsDir("")

	out, err := handleSchedulerPick(nil)
	if err != nil {
		t.Fatalf("handleSchedulerPick: %v", err)
	}
	if !strings.Contains(string(out), `"Handled":false`) {
		t.Fatalf("no account should yield Handled=false, got %s", out)
	}
}

// TestNormalizeMgmtSubPath 校验三种宿主前缀形态的路径归一化。
func TestNormalizeMgmtSubPath(t *testing.T) {
	cases := []struct{ in, want string }{
		// 完整管理前缀。
		{"/v0/management/plugins/copilot-provider/accounts", "/accounts"},
		// 短前缀。
		{"/plugins/copilot-provider/credits", "/credits"},
		// 资源前缀（面板页）。
		{"/v0/resource/plugins/copilot-provider/panel", "/panel"},
		{"/v0/resource/plugins/copilot-provider", "/panel"},
		// 尾部斜杠需归一化。
		{"/v0/management/plugins/copilot-provider/delete/", "/delete"},
	}
	for _, c := range cases {
		got, _ := normalizeMgmtSubPath(c.in)
		if got != c.want {
			t.Fatalf("normalizeMgmtSubPath(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestCachedQuotaTTL 校验额度缓存按 TTL 过期，避免面板长期展示陈旧额度。
func TestCachedQuotaTTL(t *testing.T) {
	const authID = "copilot-ttl"

	// 1. 新鲜快照可读。
	storeQuota(authID, &copilotQuotaSnapshot{FetchedAt: time.Now().Format(time.RFC3339)})
	if cachedQuota(authID) == nil {
		t.Fatal("fresh snapshot must be readable")
	}

	// 2. 超过 TTL 的快照视为过期。
	storeQuota(authID, &copilotQuotaSnapshot{
		FetchedAt: time.Now().Add(-copilotQuotaTTL - time.Minute).Format(time.RFC3339)})
	if cachedQuota(authID) != nil {
		t.Fatal("expired snapshot must not be readable")
	}

	// 3. invalidateQuota 立即清除。
	storeQuota(authID, &copilotQuotaSnapshot{FetchedAt: time.Now().Format(time.RFC3339)})
	invalidateQuota(authID)
	if cachedQuota(authID) != nil {
		t.Fatal("invalidateQuota must clear the cache")
	}
}

// TestHandleDeleteAccountClearsQuota 校验删除账号后额度缓存同步清理。
func TestHandleDeleteAccountClearsQuota(t *testing.T) {
	tmpDir := t.TempDir()
	setAccountsDir(tmpDir)
	defer setAccountsDir("")

	sa := &StoredAuth{AuthID: "copilot-del", GitHubToken: "gho_del"}
	mustWriteAccount(t, tmpDir, sa)
	storeQuota("copilot-del", &copilotQuotaSnapshot{FetchedAt: time.Now().Format(time.RFC3339)})

	out, err := handleDeleteAccount([]byte(`{"auth_id":"copilot-del"}`))
	if err != nil {
		t.Fatalf("handleDeleteAccount: %v", err)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("delete should succeed, got %s", out)
	}
	if cachedQuota("copilot-del") != nil {
		t.Fatal("deleted account must not leave a stale quota entry")
	}
}

// TestHandleSetAccountStatus 校验启停账号并落盘 disabled 字段。
func TestHandleSetAccountStatus(t *testing.T) {
	tmpDir := t.TempDir()
	setAccountsDir(tmpDir)
	defer setAccountsDir("")

	mustWriteAccount(t, tmpDir, &StoredAuth{AuthID: "copilot-s", GitHubToken: "gho_s"})

	// 1. 禁用。
	if _, err := handleSetAccountStatus([]byte(`{"auth_id":"copilot-s"}`), true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	accounts, _ := listAllAuthFiles()
	if len(accounts) != 1 || !accounts[0].Disabled {
		t.Fatalf("account should be disabled, got %+v", accounts)
	}

	// 2. 重新启用。
	if _, err := handleSetAccountStatus([]byte(`{"auth_id":"copilot-s"}`), false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	accounts, _ = listAllAuthFiles()
	if len(accounts) != 1 || accounts[0].Disabled {
		t.Fatalf("account should be enabled, got %+v", accounts)
	}

	// 3. 缺失 auth_id 返回 400。
	resp, _ := handleSetAccountStatus([]byte(`{}`), true)
	if !strings.Contains(string(resp), "400") {
		t.Fatalf("missing auth_id should yield 400, got %s", resp)
	}

	// 4. 不存在的账号返回 404。
	resp, _ = handleSetAccountStatus([]byte(`{"auth_id":"copilot-none"}`), true)
	if !strings.Contains(string(resp), "404") {
		t.Fatalf("unknown account should yield 404, got %s", resp)
	}
}

// TestExtractAuthID 校验 auth_id / auth_index / authID / id 四种键的取值优先级。
func TestExtractAuthID(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"auth_id":"a1"}`, "a1"},
		{`{"auth_index":"a2"}`, "a2"},
		{`{"authID":"a3"}`, "a3"},
		{`{"id":"a4"}`, "a4"},
		{`{"auth_id":"  a5  "}`, "a5"},
		{`{"other":"x"}`, ""},
		{`{}`, ""},
	}
	for _, c := range cases {
		if got := extractAuthID([]byte(c.body)); got != c.want {
			t.Fatalf("extractAuthID(%s)=%q, want %q", c.body, got, c.want)
		}
	}
}

// mustWriteAccount 把账号写入临时账号目录，供调度与路由测试使用。
func mustWriteAccount(t *testing.T, dir string, sa *StoredAuth) {
	t.Helper()
	if sa.Type == "" {
		sa.Type = copilotProviderID
	}
	if sa.Provider == "" {
		sa.Provider = copilotProviderID
	}
	path := filepath.Join(dir, sa.AuthID+".json")
	if err := writeAuthFileDirect(path, sa); err != nil {
		t.Fatalf("write account %s: %v", sa.AuthID, err)
	}
}
