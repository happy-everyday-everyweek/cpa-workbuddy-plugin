package main

// 本文件是 AC-103 的图片可下载性验证：从真实回复里取 file_url 并检查。
//
// 判据来自实施总览 AC-103：URL 可下载（HTTP 200 + Content-Type 为图片）。
// 水印无法程序化判定，只记录尺寸供人工目检。

import (
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestLiveFileURLDownloadable 验证生图回复里的 file_url 可下载且是图片。
//
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，AC-103 可下载判据。
func TestLiveFileURLDownloadable(t *testing.T) {
	cookie := os.Getenv("COZE_LIVE_COOKIE")
	if cookie == "" {
		t.Skip("未设置 COZE_LIVE_COOKIE，跳过可下载校验")
	}

	exec := newCozeExecutor(&StoredAuth{CookieHeader: cookie}, "coze/probe", nil)
	sessionID, _, errPrepare := exec.prepare()
	if errPrepare != nil {
		t.Fatalf("握手失败：%v", errPrepare)
	}
	messages, errList := exec.client.listMessages(sessionID)
	if errList != nil {
		t.Fatalf("拉取消息失败：%v", errList)
	}

	target := ""
	for _, msg := range messages {
		if msg.MessageType != "reply" || len(msg.Files) == 0 {
			continue
		}
		if urls := clawFileURLs(msg.Files); len(urls) > 0 {
			target = urls[0]
			break
		}
	}
	if target == "" {
		t.Skip("最近消息里没有带附件的回复，跳过")
	}

	t.Logf("目标 host=%s 路径后缀=%s", hostOfURL(target), pathSuffix(target))

	resp, errGet := http.Get(target)
	if errGet != nil {
		t.Fatalf("下载失败：%v", errGet)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d，期望 200", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	t.Logf("HTTP=%d Content-Type=%s Content-Length=%d",
		resp.StatusCode, contentType, resp.ContentLength)
	if !strings.HasPrefix(contentType, "image/") {
		t.Errorf("Content-Type=%q，期望 image/*", contentType)
	}

	cfg, format, errDecode := image.DecodeConfig(resp.Body)
	if errDecode != nil {
		t.Fatalf("图片解码失败：%v", errDecode)
	}
	t.Logf("图片格式=%s 尺寸=%dx%d", format, cfg.Width, cfg.Height)
	if cfg.Width < 64 || cfg.Height < 64 {
		t.Errorf("尺寸异常：%dx%d", cfg.Width, cfg.Height)
	}
}

// hostOfURL 取 URL 的 host。
// [参数] raw: 原始 URL。
// [返回] host，失败时为空。
// 最近修改时间：2026-10-18；改动原因：新增，脱敏打印。
func hostOfURL(raw string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil {
		return ""
	}
	return parsed.Host
}

// pathSuffix 取 URL 路径的最后一段（不含签名参数）。
// [参数] raw: 原始 URL。
// [返回] 路径末段。
// 最近修改时间：2026-10-18；改动原因：新增，脱敏打印。
func pathSuffix(raw string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil {
		return ""
	}
	path := parsed.Path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}
