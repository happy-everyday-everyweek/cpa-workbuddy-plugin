package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeCookieHeader 校验 Cookie 合并的覆盖、追加与不变三种情形。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，合并回归。
func TestMergeCookieHeader(t *testing.T) {
	header := "token_v2=old; notion_user_id=u1; analytics=keep"

	merged, changed := mergeCookieHeader(header, map[string]string{"token_v2": "new"})
	if !changed {
		t.Fatal("值变化时应报告已变更")
	}
	if !strings.Contains(merged, "token_v2=new") {
		t.Fatalf("未覆盖 token_v2：%s", merged)
	}
	if !strings.Contains(merged, "notion_user_id=u1") {
		t.Fatalf("未更新的 Cookie 应保留：%s", merged)
	}
	if !strings.Contains(merged, "analytics=keep") {
		t.Fatalf("非身份类 Cookie 应保留：%s", merged)
	}

	if _, changed := mergeCookieHeader(header, map[string]string{"token_v2": "old"}); changed {
		t.Fatal("值相同时不应报告变更")
	}

	appended, changed := mergeCookieHeader(header, map[string]string{"device_id": "d1"})
	if !changed || !strings.Contains(appended, "device_id=d1") {
		t.Fatalf("新 Cookie 应追加到末尾：%s", appended)
	}
}

// TestApplySetCookiesWritesBack 校验上游 Set-Cookie 被写回账号文件。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，滚动写回回归。
func TestApplySetCookiesWritesBack(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	auth := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-roll",
		CookieHeader: "token_v2=old; notion_user_id=u1",
		TokenV2:      "old",
		UserID:       "u1",
		SpaceID:      "space-1",
		Label:        "滚动测试",
	}
	path := filepath.Join(dir, auth.AuthID+notionAuthFileSuffix)
	if errWrite := writeAuthFileDirect(path, auth); errWrite != nil {
		t.Fatalf("初始落盘失败：%v", errWrite)
	}

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "token_v2=new; Path=/; HttpOnly")
	resp.Header.Add("Set-Cookie", "analytics=x; Path=/")
	applySetCookies(auth, resp)

	if auth.TokenV2 != "new" {
		t.Fatalf("内存中的 token_v2 未更新：%s", auth.TokenV2)
	}
	loaded, errRead := readAuthFile(path)
	if errRead != nil {
		t.Fatalf("回读失败：%v", errRead)
	}
	if loaded.TokenV2 != "new" || !strings.Contains(loaded.CookieHeader, "token_v2=new") {
		t.Fatalf("滚动 Cookie 未落盘：%+v", loaded)
	}
	if strings.Contains(loaded.CookieHeader, "analytics=x") {
		t.Fatalf("非身份类 Cookie 不应写回：%s", loaded.CookieHeader)
	}
}

// TestApplySetCookiesSkipsExternalRewrite 校验外部改写后放弃滚动。
//
// 用户重新导入 Cookie 后，磁盘内容与内存快照不一致；此时若继续滚动写回，
// 旧会话值会覆盖刚导入的新凭据。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，防覆盖回归。
func TestApplySetCookiesSkipsExternalRewrite(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	auth := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-guard",
		CookieHeader: "token_v2=old; notion_user_id=u1",
		TokenV2:      "old",
		UserID:       "u1",
		SpaceID:      "space-1",
		Label:        "防覆盖测试",
	}
	path := filepath.Join(dir, auth.AuthID+notionAuthFileSuffix)
	if errWrite := writeAuthFileDirect(path, auth); errWrite != nil {
		t.Fatalf("初始落盘失败：%v", errWrite)
	}

	// 模拟用户重新导入：磁盘内容被另一份凭据替换。
	fresh := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-guard",
		CookieHeader: "token_v2=fresh; notion_user_id=u1",
		TokenV2:      "fresh",
		UserID:       "u1",
		SpaceID:      "space-1",
		Label:        "重新导入",
	}
	if errWrite := writeAuthFileDirect(path, fresh); errWrite != nil {
		t.Fatalf("重新导入失败：%v", errWrite)
	}

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "token_v2=stale; Path=/")
	applySetCookies(auth, resp)

	loaded, errRead := readAuthFile(path)
	if errRead != nil {
		t.Fatalf("回读失败：%v", errRead)
	}
	if loaded.TokenV2 != "fresh" {
		t.Fatalf("外部改写被覆盖：%s", loaded.TokenV2)
	}
}

// TestApplySetCookiesIgnoresDeletion 校验上游删除指令不被当成新值。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，删除语义回归。
func TestApplySetCookiesIgnoresDeletion(t *testing.T) {
	auth := &StoredAuth{CookieHeader: "token_v2=keep; notion_user_id=u1", TokenV2: "keep"}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "token_v2=; Max-Age=0; Path=/")
	applySetCookies(auth, resp)

	if auth.TokenV2 != "keep" {
		t.Fatalf("删除指令不应改写 token：%s", auth.TokenV2)
	}
}
