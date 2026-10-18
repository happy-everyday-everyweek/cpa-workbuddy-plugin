package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveClawStream 用真实账号跑通 claw 对话链路（需环境变量 COZE_LIVE_COOKIE）。
//
// 该测试默认跳过：会话 Cookie 只能由用户在扣子页面取得，不能自动化。
// 设置 COZE_LIVE_COOKIE 后执行 `go test -run TestLiveClawStream -v` 即真实校验
// 「claw_id → 主会话 → frontier 凭证 → WS 连接 → 提交消息 → 收增量 → 收尾」。
//
// 它会真实消耗一次对话配额（极短提示，约数十字），
// 因此不做成默认用例。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，TASK-004 真实链路验证入口。
func TestLiveClawStream(t *testing.T) {
	cookie := os.Getenv("COZE_LIVE_COOKIE")
	if cookie == "" {
		t.Skip("未设置 COZE_LIVE_COOKIE，跳过真实对话链路校验")
	}

	sa := &StoredAuth{CookieHeader: cookie}
	exec := newCozeExecutor(sa, "coze/glm-5.3-flash", nil)

	// 第一步：握手三步必须都通，否则后续无从谈起。
	sessionID, key, errPrepare := exec.prepare()
	if errPrepare != nil {
		t.Fatalf("握手失败：%v", errPrepare)
	}
	t.Logf("握手成功：session_id=%s access_key_len=%d device_id_len=%d",
		sessionID, len(key.AccessKey), len(key.DeviceID))

	// 第二步：跑一次真实对话，要求至少收到一个增量且最终收尾。
	var deltas []string
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	content, errRun := exec.runStream(ctx, "只回复两个字：收到", func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if errRun != nil {
		t.Fatalf("对话失败：%v", errRun)
	}
	if len(deltas) == 0 {
		t.Fatal("未收到任何增量，说明 WS 链路未打通")
	}
	if strings.TrimSpace(content) == "" {
		t.Fatal("最终正文为空")
	}

	t.Logf("对话成功：增量帧=%d 最终正文长度=%d", len(deltas), len(content))
	t.Logf("正文：%s", content)
}

// TestLiveClawStreamMatchesSnapshot 验证增量拼接与 event_final 快照一致。
//
// 这是 TASK-004 的核心正确性判据：两者必须逐字节相等。
// 若不等，说明去重或排序有缺陷，用户会看到重复或缺字。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，增量正确性判据。
func TestLiveClawStreamMatchesSnapshot(t *testing.T) {
	cookie := os.Getenv("COZE_LIVE_COOKIE")
	if cookie == "" {
		t.Skip("未设置 COZE_LIVE_COOKIE，跳过增量一致性校验")
	}

	sa := &StoredAuth{CookieHeader: cookie}
	exec := newCozeExecutor(sa, "coze/glm-5.3-flash", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var joined strings.Builder
	content, errRun := exec.runStream(ctx, "请从1数到20，逗号分隔，只输出数字。", func(delta string) error {
		joined.WriteString(delta)
		return nil
	})
	if errRun != nil {
		t.Fatalf("对话失败：%v", errRun)
	}

	if joined.String() != content {
		t.Fatalf("增量拼接与最终正文不一致：\n拼接=%q\n正文=%q", joined.String(), content)
	}
	t.Logf("增量一致性验证通过：长度=%d", len(content))
}
