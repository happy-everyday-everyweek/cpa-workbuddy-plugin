package main

// 本文件是 AC-103 的账号落盘入口：用真实 Cookie 走插件的官方落盘链路。
//
// 不手工拼宿主 auth 文件格式：宿主用 coreauth.Auth 直接解析 auth 目录，
// 要求 created_at / updated_at / last_refreshed_at / next_refresh_after /
// next_retry_after 全是合法 RFC3339；手拼必然失败。
// 插件自身的 writeAuthFileDirect + host.auth.save 才是正确通道。

import (
	"os"
	"testing"
	"time"
)

// TestLiveSeedAccount 用真实 Cookie 落盘一个可用账号。
//
// 落盘位置是插件默认账号目录（~/.antigravity_cockpit/coze_accounts/），
// 宿主与插件都能读到。需要 COZE_LIVE_COOKIE。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，AC-103 账号接入。
func TestLiveSeedAccount(t *testing.T) {
	cookie := os.Getenv("COZE_LIVE_COOKIE")
	if cookie == "" {
		t.Skip("未设置 COZE_LIVE_COOKIE，跳过账号落盘")
	}

	info, errProbe := probeAccountInfo(cookie)
	if errProbe != nil {
		t.Fatalf("判据失败：%v", errProbe)
	}
	if !info.isSuccess() {
		t.Fatalf("判据未通过：message=%q", info.Message)
	}

	authID := authIDForSecUserID(info.secUserID())
	if authID == "" {
		t.Fatal("无法派生账号标识")
	}

	now := time.Now().Format(time.RFC3339)
	sa := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       authID,
		CookieHeader: cookie,
		SecUserID:    info.secUserID(),
		UserID:       info.userID(),
		ScreenName:   info.screenName(),
		Mobile:       info.mobile(),
		LoginAt:      now,
		LastRenewAt:  now,
		Label:        "AC103-Real",
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	fullPath := getAccountsDir() + "/" + authID + ".json"
	if errWrite := writeAuthFileDirect(fullPath, sa); errWrite != nil {
		t.Fatalf("落盘失败：%v", errWrite)
	}
	t.Logf("账号已落盘：auth_id=%s 文件=%s", authID, authID+".json")
	t.Logf("判据通过：sec_user_id 非空=%t screen_name 非空=%t",
		info.secUserID() != "", info.screenName() != "")
}
