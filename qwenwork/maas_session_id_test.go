// maas_session_id_test.go 覆盖 MaaS session_id 的 UUID 派生。
//
// 缺陷背景（2026-10-18 生产实证）：调度侧会话键形如
// "x-client-request-id:abc"，被直接当作上游 session_id 发送，上游强校验
// UUID 后返回 `422 value is not a valid UUID`，导致执行器路径全部失败，
// 而面板测试（传空 sessionID）正常——两条路径的差异掩盖了该缺陷。
package main

import (
	"regexp"
	"testing"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestMaasSessionID_DerivesValidUUID 验证任意会话键都派生出合法 UUID。
func TestMaasSessionID_DerivesValidUUID(t *testing.T) {
	keys := []string{
		"x-client-request-id:abc",
		"session-id:20261018",
		"derived:conv-123",
		"execution:task-9",
		"中文会话键",
	}
	for _, k := range keys {
		got := maasSessionID(k)
		if !uuidPattern.MatchString(got) {
			t.Errorf("maasSessionID(%q) = %q, not a valid v4 UUID", k, got)
		}
	}
}

// TestMaasSessionID_Deterministic 验证同一会话键恒得同一 UUID（保持会话粘性），
// 不同会话键得到不同 UUID。
func TestMaasSessionID_Deterministic(t *testing.T) {
	a1 := maasSessionID("x-client-request-id:abc")
	a2 := maasSessionID("x-client-request-id:abc")
	if a1 != a2 {
		t.Fatalf("same session key must map to same UUID: %q vs %q", a1, a2)
	}
	b := maasSessionID("x-client-request-id:def")
	if a1 == b {
		t.Fatalf("different session keys must map to different UUIDs, both = %q", a1)
	}
}

// TestMaasSessionID_EmptyReturnsEmpty 验证空键返回空串，交由上游自行分配。
func TestMaasSessionID_EmptyReturnsEmpty(t *testing.T) {
	if got := maasSessionID(""); got != "" {
		t.Errorf("empty session key must return empty, got %q", got)
	}
	if got := maasSessionID("   "); got != "" {
		t.Errorf("blank session key must return empty, got %q", got)
	}
}

// TestMintPayload_SessionIDIsUUID 直接断言发往上游的请求体。
//
// 这是变异能命中的关键：只测 maasSessionID 无法发现调用方漏用派生结果，
// 必须验证「实际组装出来的 payload」里 session_id 是 UUID。
func TestMintPayload_SessionIDIsUUID(t *testing.T) {
	payload := mintPayload("x-client-request-id:abc")
	sid, _ := payload["session_id"].(string)
	if !uuidPattern.MatchString(sid) {
		t.Fatalf("payload session_id = %q, not a valid v4 UUID (upstream rejects with 422)", sid)
	}
	// 空会话键时不得写入该字段，避免空串触发上游校验。
	if p := mintPayload(""); len(p) != 0 {
		t.Fatalf("empty session key must produce empty payload, got %v", p)
	}
}
