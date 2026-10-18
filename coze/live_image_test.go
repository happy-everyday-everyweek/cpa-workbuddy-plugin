package main

// 本文件是 AC-103 的真实生图探针，只在设置 COZE_LIVE_COOKIE 时执行。
//
// 它与 live_claw_test.go 的区别：那条用例验证「文本对话链路通」，
// 这条验证「生图回复的图片能否被插件取到并转成 markdown」。
// 生图是否触发由扣子侧模型选择器决定，插件无法指定模型，
// 因此本用例只做观测与取证，不强制断言必须出图。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveImageGeneration 用真实账号跑一次生图对话并打印取到的图片。
//
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，AC-103 真实取证入口。
func TestLiveImageGeneration(t *testing.T) {
	cookie := os.Getenv("COZE_LIVE_COOKIE")
	if cookie == "" {
		t.Skip("未设置 COZE_LIVE_COOKIE，跳过真实生图校验")
	}

	sa := &StoredAuth{CookieHeader: cookie}
	exec := newCozeExecutor(sa, "coze/doubao-seedream-5-0-260128", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	var emitted []string
	content, errRun := exec.runStream(ctx,
		"画一只黑猫趴在书桌上，写实风格", func(delta string) error {
			emitted = append(emitted, delta)
			return nil
		})
	if errRun != nil {
		t.Fatalf("对话失败：%v", errRun)
	}

	joined := strings.Join(emitted, "")
	t.Logf("正文长度=%d 下发片段数=%d", len(content), len(emitted))
	t.Logf("正文：%s", content)

	if strings.Contains(joined, "![image](http") {
		// 只打印 URL 的 host 与路径前缀，不带签名参数。
		for _, part := range strings.Split(joined, "![image](") {
			if idx := strings.Index(part, ")"); idx > 0 {
				url := part[:idx]
				if !strings.HasPrefix(url, "http") {
					continue
				}
				t.Logf("取到图片：%s", redactSignedURL(url))
			}
		}
	} else {
		t.Logf("本轮未取到图片（模型自主性）：下发内容=%q", joined)
	}
}

// redactSignedURL 去掉 URL 上的签名参数，只保留可辨识部分。
// [参数] raw: 原始 URL。
// [返回] 脱敏后的 URL。
// 最近修改时间：2026-10-18；改动原因：新增，证据脱敏。
func redactSignedURL(raw string) string {
	if idx := strings.IndexAny(raw, "?#"); idx > 0 {
		return raw[:idx] + "?<sign-redacted>"
	}
	return raw
}
