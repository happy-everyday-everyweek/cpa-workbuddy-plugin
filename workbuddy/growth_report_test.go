// growth_report_test.go 覆盖行为上报的字段形状与前置校验。
package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
)

// TestReportChatActivitySendsFullEvent 验证上报事件的字段完整性与 userId 必填。
// 缺 userId 时上游返回 200 但静默丢弃；用最小 3 字段则可能被后续加严拒绝。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增行为上报测试（TASK-02.2）
func TestReportChatActivitySendsFullEvent(t *testing.T) {
	var captured []map[string]any
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != reportPath {
			t.Errorf("unexpected report path: %s", r.URL.Path)
		}
		var events []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Errorf("decode report body: %v", err)
		}
		captured = events
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
	})
	defer cleanup()

	sa := &storedAuth{}
	sa.Account.UID = "uid-abc"
	if err := reportChatActivity(sa, "conv-1", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("body must be a JSON array of 1 event, got %d", len(captured))
	}
	event := captured[0]
	if event["userId"] != "uid-abc" {
		t.Fatalf("userId = %v, want uid-abc", event["userId"])
	}
	if event["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode = %v", event["eventCode"])
	}
	if len(event) < 20 {
		t.Fatalf("event must carry the full field set, got only %d keys", len(event))
	}
}

// TestReportChatActivityRejectsEmptyUID 验证缺 uid 时在发送前拦截。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增行为上报测试（TASK-02.2）
func TestReportChatActivityRejectsEmptyUID(t *testing.T) {
	var called int32
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
	})
	defer cleanup()

	if err := reportChatActivity(&storedAuth{}, "conv-1", ""); err == nil {
		t.Fatal("expected error when uid is empty")
	}
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("must not send request without uid, got %d calls", got)
	}
}
