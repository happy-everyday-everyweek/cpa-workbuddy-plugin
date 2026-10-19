package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestFailoverCooldownLadder 校验阶梯冷却与上限。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，阶梯回归。
func TestFailoverCooldownLadder(t *testing.T) {
	cases := []struct {
		count int
		base  int
		want  time.Duration
	}{
		{count: 0, base: 30, want: 0},
		{count: -1, base: 30, want: 0},
		{count: 1, base: 30, want: 30 * time.Second},
		{count: 2, base: 30, want: 60 * time.Second},
		{count: 19, base: 30, want: 570 * time.Second},
		{count: 20, base: 30, want: 600 * time.Second},
		{count: 100, base: 30, want: 600 * time.Second},
		{count: 1, base: 0, want: 60 * time.Second},
		{count: 1, base: 120, want: 120 * time.Second},
		{count: 10, base: 120, want: 600 * time.Second},
	}
	for _, c := range cases {
		if got := failoverCooldownFor(c.count, c.base); got != c.want {
			t.Errorf("count=%d base=%d：得到 %v，期望 %v", c.count, c.base, got, c.want)
		}
	}
}

// TestIsAccountFailure 校验失败分类口径。
//
// 400 必须排除：把业务错误计入冷却会把健康账号逐个冷掉。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，分类回归。
func TestIsAccountFailure(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{status: 0, want: true},
		{status: http.StatusInternalServerError, want: true},
		{status: http.StatusBadGateway, want: true},
		{status: http.StatusUnauthorized, want: true},
		{status: http.StatusForbidden, want: true},
		{status: http.StatusNotFound, want: true},
		{status: http.StatusMethodNotAllowed, want: true},
		{status: http.StatusPaymentRequired, want: true},
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusBadRequest, want: false},
		{status: http.StatusUnprocessableEntity, want: false},
		{status: http.StatusOK, want: false},
	}
	for _, c := range cases {
		if got := isAccountFailure(c.status); got != c.want {
			t.Errorf("isAccountFailure(%d) = %v，期望 %v", c.status, got, c.want)
		}
	}
}

// TestShouldRotateHonorsBudget 校验 40x 预算对换号的控制。
//
// 预算为 0 时 40x 只冷却不换号，但 429 仍应换号——用户关掉 40x 换号
// 的意图是避免因端点差异频繁跳号，不是放弃限流恢复。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，预算回归。
func TestShouldRotateHonorsBudget(t *testing.T) {
	cases := []struct {
		name   string
		status int
		budget int
		want   bool
	}{
		{name: "429 无预算限制", status: http.StatusTooManyRequests, budget: 0, want: true},
		{name: "402 无预算限制", status: http.StatusPaymentRequired, budget: 0, want: true},
		{name: "401 有预算", status: http.StatusUnauthorized, budget: 3, want: true},
		{name: "401 无预算", status: http.StatusUnauthorized, budget: 0, want: false},
		{name: "403 有预算", status: http.StatusForbidden, budget: 1, want: true},
		{name: "403 无预算", status: http.StatusForbidden, budget: 0, want: false},
		{name: "404 有预算", status: http.StatusNotFound, budget: 2, want: true},
		{name: "404 无预算", status: http.StatusNotFound, budget: 0, want: false},
		{name: "405 无预算", status: http.StatusMethodNotAllowed, budget: 0, want: false},
		{name: "500 不换号", status: http.StatusInternalServerError, budget: 3, want: false},
		{name: "传输错误不换号", status: 0, budget: 3, want: false},
		{name: "400 不换号", status: http.StatusBadRequest, budget: 3, want: false},
	}
	for _, c := range cases {
		if got := shouldRotate(c.status, c.budget); got != c.want {
			t.Errorf("%s：shouldRotate(%d, %d) = %v，期望 %v", c.name, c.status, c.budget, got, c.want)
		}
	}
}

// TestNoteAccountFailureAndCooldown 校验记账、冷却与成功清零。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，冷却生命周期回归。
func TestNoteAccountFailureAndCooldown(t *testing.T) {
	resetFailoverStates()
	const authID = "notion-aaa"

	if isAccountCoolingDown(authID) {
		t.Fatal("初始状态不应冷却")
	}

	if !noteAccountFailure(authID, http.StatusTooManyRequests, 1) {
		t.Fatal("429 应计入冷却")
	}
	if !isAccountCoolingDown(authID) {
		t.Fatal("429 后应处于冷却")
	}
	if got := accountFailureCount(authID); got != 1 {
		t.Fatalf("连续失败次数应为 1，实际 %d", got)
	}

	noteAccountFailure(authID, http.StatusTooManyRequests, 1)
	if got := accountFailureCount(authID); got != 2 {
		t.Fatalf("连续失败次数应为 2，实际 %d", got)
	}

	resetAccountFailover(authID)
	if isAccountCoolingDown(authID) {
		t.Fatal("成功清零后不应再冷却")
	}
	if got := accountFailureCount(authID); got != 0 {
		t.Fatalf("清零后计数应为 0，实际 %d", got)
	}

	if noteAccountFailure(authID, http.StatusBadRequest, 1) {
		t.Fatal("业务 400 不应计入冷却")
	}
	if isAccountCoolingDown(authID) {
		t.Fatal("业务 400 后不应冷却")
	}
}

// TestClassifyInferenceError 校验从错误到状态码的归一。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，错误分类回归。
func TestClassifyInferenceError(t *testing.T) {
	if got := classifyInferenceError(&notionAPIError{StatusCode: http.StatusUnauthorized}); got != http.StatusUnauthorized {
		t.Errorf("上游状态码应原样返回，实际 %d", got)
	}
	if got := classifyInferenceError(errTrustRuleDenied); got != http.StatusForbidden {
		t.Errorf("trust-rule-denied 应归一为 403，实际 %d", got)
	}
	if got := classifyInferenceError(errNoAvailableAccount); got != 0 {
		t.Errorf("非上游错误应返回 0，实际 %d", got)
	}
	if got := classifyInferenceError(nil); got != 0 {
		t.Errorf("nil 应返回 0，实际 %d", got)
	}
}

// TestPickAccountSkipsUnavailable 校验选号跳过停用、冷却与刚失败的账号。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，选号回归。
func TestPickAccountSkipsUnavailable(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")
	resetFailoverStates()

	writeTestAuthFile(t, dir, "notion-aaa", false, "space-a")
	writeTestAuthFile(t, dir, "notion-bbb", true, "space-b")
	writeTestAuthFile(t, dir, "notion-ccc", false, "space-c")

	// 排除 aaa 后应选中 ccc（bbb 已停用）。
	got, errPick := pickAccountExcluding("notion-aaa")
	if errPick != nil {
		t.Fatalf("选号失败：%v", errPick)
	}
	if got.AuthID != "notion-ccc" {
		t.Fatalf("应跳过停用与刚失败账号，实际选中 %s", got.AuthID)
	}

	// ccc 进入冷却后应无号可用。
	noteAccountFailure("notion-ccc", http.StatusTooManyRequests, 30)
	if _, errPick := pickAccountExcluding("notion-aaa"); errPick == nil {
		t.Fatal("全部账号不可用时应报错")
	}
}

// TestTrustRuleDeniedFrameIsRecognized 校验流内拒绝帧被识别为账号级故障。
//
// 该帧以 HTTP 200 下发；若不识别，整轮会被误判成空流且账号不进冷却。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，静默失败识别回归。
func TestTrustRuleDeniedFrameIsRecognized(t *testing.T) {
	line := []byte(`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"x","type":"error","value":{"subType":"trust-rule-denied"}}}]}`)

	parser := newStreamParser()
	if _, errLine := parser.consumeLine(line); errLine != nil {
		t.Fatalf("拒绝帧不应返回解析错误：%v", errLine)
	}
	if parser.failure == "" {
		t.Fatal("trust-rule-denied 帧未被识别")
	}
	if got := classifyInferenceError(errTrustRuleDenied); got != http.StatusForbidden {
		t.Fatalf("拒绝帧应归一为 403，实际 %d", got)
	}
	if !isAccountFailure(http.StatusForbidden) {
		t.Fatal("403 应计入账号冷却")
	}
}

// writeTestAuthFile 在测试目录写入一份账号文件。
// [参数] t: 测试上下文；dir: 目录；authID: 账号标识；disabled: 是否停用；spaceID: 空间。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，测试夹具。
func writeTestAuthFile(t *testing.T, dir, authID string, disabled bool, spaceID string) {
	t.Helper()
	auth := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       authID,
		CookieHeader: "token_v2=t; notion_user_id=u",
		TokenV2:      "t",
		UserID:       "u",
		SpaceID:      spaceID,
		Disabled:     disabled,
		Label:        authID,
	}
	if errWrite := writeAuthFileDirect(filepath.Join(dir, authID+notionAuthFileSuffix), auth); errWrite != nil {
		t.Fatalf("写入测试账号失败：%v", errWrite)
	}
}
