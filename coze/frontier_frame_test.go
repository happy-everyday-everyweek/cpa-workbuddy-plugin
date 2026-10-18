package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

// 本文件验证 frontier 帧编解码。
//
// 关键用例使用 2026-10-18 浏览器实测抓到的真实帧 hex，
// 而不是自己构造的「看起来像」的字节——自造样本无法证明
// 字段号与上游一致，只有真实帧能证明解码器读对了位置。

// realCursorFrameHex 是真实游标帧（frameType=16，带 cursor_file_name）。
//
// 来源：claw 连接首次建立时服务端下发，用于断线续传。
const realCursorFrameHex = "088394ffb1051084cabdccbeace5ef18180020052a2b0a10637572736f725f66696c655f6e616d651217426655796f7637695252583379796269323041727938792a160a123a785f66726f6e746965725f6d73675f696412002a290a193a785f66726f6e746965725f6d73675f73656e645f74696d65120c646d37757462676c327232692a460a0b7472616365706172656e74123730302d31303533353030323138343961623265633063623430373661393466306536622d623731303565663632613539646461322d303132003a0042004a00521a66726f6e746965723b7374613d313739323331353438363133325a4e333236363336363136333337333333323337363136363631363233343339333233303336333136363338363236333632376333333330333033343334376330316131346535336566623430303033601068007200a00100a80100b00100"

// realReplyFrameHex 是真实业务帧（service=33555657，payloadType=message）。
//
// 来源：页面发消息后 claw 通道推送的会话进度帧，是最短的真实业务帧。
const realReplyFrameHex = "08f181abb205109a94aef592b4e5ef1818c989801020002a280a18785f66726f6e746965725f72656369657665645f74696d65120c646d3775767a7a323436776a2a1a0a15785f66726f6e746965725f617272697665726174651201312a3a0a123a785f66726f6e746965725f6d73675f6964122434343437623062302d623039632d343130612d393466622d3538346635316339356534372a290a193a785f66726f6e746965725f6d73675f73656e645f74696d65120c646d3775767a7a326f6b67322a460a0b7472616365706172656e74123730302d32373036663664393430343735633738306261333035383630383730626165622d336636656338656337316463366461372d303132003a0773657373696f6e42637b225f6163636f756e745f6964223a2230222c22636c61775f6964223a2237363937363339353636343436313439383938222c2270726f6772657373223a312c2273657373696f6e5f6964223a2237363937363430363630303134333833343131227d4a2434343437623062302d623039632d343130612d393466622d35383466353163393565343752005a2434343437623062302d623039632d343130612d393466622d353834663531633935653437600068007200a00100a80100b00100"

// mustHex 把 hex 字符串解成字节，失败直接终止测试。
// [参数] t: 测试上下文；s: hex 字符串。
// [返回] 解码后的字节。
// 最近修改时间：2026-10-18；改动原因：新增，真实样本解码辅助。
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("样本 hex 解码失败: %v", err)
	}
	return raw
}

// TestDecodeRealCursorFrame 用真实游标帧验证解码器。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，真实帧字段号校准。
func TestDecodeRealCursorFrame(t *testing.T) {
	frame, err := decodeFrontierFrame(mustHex(t, realCursorFrameHex))
	if err != nil {
		t.Fatalf("解码真实游标帧失败: %v", err)
	}

	if frame.FrameType != frontierFrameTypeStream {
		t.Errorf("frameType = %d，期望 %d", frame.FrameType, frontierFrameTypeStream)
	}
	if frame.Service != 0 {
		t.Errorf("游标帧 service = %d，期望 0", frame.Service)
	}
	if frame.Method != 5 {
		t.Errorf("method = %d，期望 5", frame.Method)
	}

	cursor := frame.headerValue("cursor_file_name")
	if cursor != "BfUyov7iRRX3yybi20Ary8y" {
		t.Errorf("cursor_file_name = %q，期望 BfUyov7iRRX3yybi20Ary8y", cursor)
	}
	if len(frame.Headers) < 2 {
		t.Errorf("headers 数量 = %d，期望至少 2 条", len(frame.Headers))
	}
}

// TestDecodeRealReplyFrame 用真实业务帧验证 service 与 payload。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，claw service 校准。
func TestDecodeRealReplyFrame(t *testing.T) {
	frame, err := decodeFrontierFrame(mustHex(t, realReplyFrameHex))
	if err != nil {
		t.Fatalf("解码真实业务帧失败: %v", err)
	}

	if frame.Service != frontierServiceClaw {
		t.Errorf("service = %d，期望 %d", frame.Service, frontierServiceClaw)
	}
	if frame.PayloadType != "session" {
		t.Errorf("payloadType = %q，期望 session", frame.PayloadType)
	}
	if frame.Method != 0 {
		t.Errorf("method = %d，期望 0", frame.Method)
	}
	if len(frame.Payload) == 0 {
		t.Fatal("payload 为空")
	}
	if got := frame.headerValue(":x_frontier_msg_id"); got == "" {
		t.Error("缺少 :x_frontier_msg_id 头")
	}
	// 内部保留头带冒号前缀，普通头（如 traceparent）不带。
	if got := frame.headerValue("traceparent"); got == "" {
		t.Error("缺少 traceparent 头")
	}

	// payload 必须是可读 JSON：上游是明文 JSON，不是二次编码。
	payload := string(frame.Payload)
	if payload[0] != '{' {
		t.Errorf("payload 首字节 = %q，期望 JSON 对象", payload[0])
	}
	if !strings.Contains(payload, `"claw_id"`) {
		t.Errorf("payload 缺少 claw_id 字段: %s", payload)
	}
}

// TestFrameRoundTrip 验证编码后再解码能还原全部字段。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，订阅帧编码正确性。
func TestFrameRoundTrip(t *testing.T) {
	original := &frontierFrame{
		SeqID:   1424008517,
		LogID:   3672102781,
		Service: frontierServiceSubscribe,
		Method:  frontierMethodSubscribe,
		Headers: []frontierHeader{
			{Key: "cursor_file_name", Value: "abc123"},
			{Key: "traceparent", Value: "00-xyz-01"},
		},
		PayloadType: "session",
		Payload:     []byte(`{"k":"v"}`),
		FrameType:   frontierFrameTypeStream,
	}

	decoded, err := decodeFrontierFrame(encodeFrontierFrame(original))
	if err != nil {
		t.Fatalf("往返解码失败: %v", err)
	}

	if decoded.SeqID != original.SeqID || decoded.LogID != original.LogID {
		t.Errorf("SeqID/LogID 不一致: %+v", decoded)
	}
	if decoded.Service != original.Service || decoded.Method != original.Method {
		t.Errorf("service/method 不一致: %+v", decoded)
	}
	if decoded.FrameType != original.FrameType {
		t.Errorf("frameType = %d，期望 %d", decoded.FrameType, original.FrameType)
	}
	if decoded.PayloadType != original.PayloadType {
		t.Errorf("payloadType = %q，期望 %q", decoded.PayloadType, original.PayloadType)
	}
	if string(decoded.Payload) != string(original.Payload) {
		t.Errorf("payload = %q，期望 %q", decoded.Payload, original.Payload)
	}
	if len(decoded.Headers) != 2 {
		t.Fatalf("headers 数量 = %d，期望 2", len(decoded.Headers))
	}
	if decoded.Headers[0].Key != "cursor_file_name" || decoded.Headers[0].Value != "abc123" {
		t.Errorf("头 0 不一致: %+v", decoded.Headers[0])
	}
}

// TestDecodeSkipsUnknownField 验证未知字段被跳过而不是报错。
//
// 上游加字段是常态；若遇到未知字段就失败，新版本上线当天链路即断。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，向前兼容守卫。
func TestDecodeSkipsUnknownField(t *testing.T) {
	base := encodeFrontierFrame(&frontierFrame{Service: frontierServiceClaw})
	// 追加一个未知的 length-delimited 字段（字段号 99）。
	extra := appendBytesField(nil, 99, []byte("future"))

	decoded, err := decodeFrontierFrame(append(base, extra...))
	if err != nil {
		t.Fatalf("含未知字段时应成功解码，实际失败: %v", err)
	}
	if decoded.Service != frontierServiceClaw {
		t.Errorf("service = %d，期望 %d", decoded.Service, frontierServiceClaw)
	}
}

// TestDecodeRejectsTruncatedPayload 验证长度越界被拒绝。
//
// 越界不能静默截断：截断的 payload 会变成非法 JSON，
// 在更下游才报错，排查成本远高于在此处失败。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，边界守卫。
func TestDecodeRejectsTruncatedPayload(t *testing.T) {
	full := encodeFrontierFrame(&frontierFrame{Payload: []byte("0123456789")})
	if _, err := decodeFrontierFrame(full[:len(full)-3]); err == nil {
		t.Fatal("截断帧应当报错，实际成功")
	}
}

// TestDecodeRejectsEmptyFrame 验证空帧被拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，空输入守卫。
func TestDecodeRejectsEmptyFrame(t *testing.T) {
	if _, err := decodeFrontierFrame(nil); err == nil {
		t.Fatal("空帧应当报错，实际成功")
	}
}

// TestHeaderValueMissing 验证缺失头返回空串。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，头读取边界。
func TestHeaderValueMissing(t *testing.T) {
	frame := &frontierFrame{Headers: []frontierHeader{{Key: "a", Value: "1"}}}
	if got := frame.headerValue("cursor_file_name"); got != "" {
		t.Errorf("缺失头应返回空串，实际 %q", got)
	}
	var nilFrame *frontierFrame
	if got := nilFrame.headerValue("x"); got != "" {
		t.Errorf("nil 帧应返回空串，实际 %q", got)
	}
}
