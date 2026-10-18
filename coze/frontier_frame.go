package main

import (
	"fmt"
)

// 本文件实现扣子 frontier WebSocket 的帧编解码。
//
// 帧是 protobuf 二进制，但没有 .proto 文件可用——协议只存在于客户端
// 压缩后的 JS 里。这里的字段号来自 SDK 的 encodeFrame/decodedFrame
// 反编译，并用真实帧逐字段验证过（2026-10-18，100 帧样本）。
//
// 为什么不引入 protobuf 运行时：整份 schema 只有 12 个标量字段，
// 其中 10 个是 varint 或 length-delimited，手写编解码约 150 行即可覆盖，
// 引入 protoc 与运行时反而会把构建链复杂度抬高（c-shared 交叉编译）。
//
// 为什么 decode 不依赖「字段顺序」：protobuf 允许字段任意顺序、允许未知字段，
// 上游未来加字段时，按序读取会直接错位。这里按 tag 分派并跳过未知字段。

const (
	// frontierServiceClaw 是 claw 对话业务的 service 编号。
	//
	// 实测值：100 帧业务帧全部为 33555657。注意不要与
	// /api/fullstack_chux/v1/frontier/connection-params 返回的
	// message_service_id=33555616 混淆——那是另一条通道（aid=578287），
	// 用错只能收到心跳，收不到任何业务帧。
	frontierServiceClaw uint64 = 33555657

	// frontierServiceSubscribe 是订阅帧使用的 service 编号（通用 fws 约定）。
	frontierServiceSubscribe uint64 = 9000

	// frontierMethodSubscribe 是订阅帧使用的 method 编号。
	frontierMethodSubscribe uint64 = 5

	// frontierFrameTypeStream 是流数据帧的 frameType（携带游标状态）。
	frontierFrameTypeStream uint64 = 16
)

// protobuf wire type 常量。
//
// 只实现实际用到的三种：varint、length-delimited、以及跳过用的定长类型。
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// frontierHeader 是帧头里的一个键值对。
type frontierHeader struct {
	Key   string
	Value string
}

// frontierFrame 是 frontier WebSocket 的一帧。
//
// 字段顺序与字段号严格对应 SDK 的 encodeFrame：
// 1=SeqID, 2=LogID, 3=service, 4=method, 5=headers, 6=payloadEncoding,
// 7=payloadType, 8=payload, 9=LogIDNew, 10=serverTiming, 11=msgID, 12=frameType。
type frontierFrame struct {
	SeqID           uint64
	LogID           uint64
	Service         uint64
	Method          uint64
	Headers         []frontierHeader
	PayloadEncoding string
	// PayloadType 是事件名（实测取值为 session / message），不是 MIME。
	PayloadType  string
	Payload      []byte
	LogIDNew     string
	ServerTiming string
	MsgID        string
	FrameType    uint64
}

// headerValue 读取指定 key 的头值。
// [参数] key: 头名。
// [返回] 头值；不存在时返回空串。
// 最近修改时间：2026-10-18；改动原因：新增，游标与 ACK 头读取入口。
func (f *frontierFrame) headerValue(key string) string {
	if f == nil {
		return ""
	}
	for _, h := range f.Headers {
		if h.Key == key {
			return h.Value
		}
	}
	return ""
}

// encodeFrontierFrame 把帧序列化成 protobuf 字节。
//
// 零值字段一律省略：protobuf 的零值等价于「未设置」，上游解码端
// 对缺失字段有默认值语义；把零值写进报文会让某些严格实现误判为显式设值。
// [参数] f: 待编码帧。
// [返回] 编码后的字节。
// 最近修改时间：2026-10-18；改动原因：新增，订阅帧与测试回环需要编码能力。
func encodeFrontierFrame(f *frontierFrame) []byte {
	if f == nil {
		return nil
	}
	out := make([]byte, 0, 256)

	if f.SeqID != 0 {
		out = appendVarintField(out, 1, f.SeqID)
	}
	if f.LogID != 0 {
		out = appendVarintField(out, 2, f.LogID)
	}
	if f.Service != 0 {
		out = appendVarintField(out, 3, f.Service)
	}
	if f.Method != 0 {
		out = appendVarintField(out, 4, f.Method)
	}
	for _, h := range f.Headers {
		inner := make([]byte, 0, len(h.Key)+len(h.Value)+8)
		inner = appendStringField(inner, 1, h.Key)
		inner = appendStringField(inner, 2, h.Value)
		out = appendBytesField(out, 5, inner)
	}
	if f.PayloadEncoding != "" {
		out = appendStringField(out, 6, f.PayloadEncoding)
	}
	if f.PayloadType != "" {
		out = appendStringField(out, 7, f.PayloadType)
	}
	if len(f.Payload) > 0 {
		out = appendBytesField(out, 8, f.Payload)
	}
	if f.LogIDNew != "" {
		out = appendStringField(out, 9, f.LogIDNew)
	}
	if f.ServerTiming != "" {
		out = appendStringField(out, 10, f.ServerTiming)
	}
	if f.MsgID != "" {
		out = appendStringField(out, 11, f.MsgID)
	}
	if f.FrameType != 0 {
		out = appendVarintField(out, 12, f.FrameType)
	}
	return out
}

// decodeFrontierFrame 把 protobuf 字节解析成帧。
//
// 未知字段按 wire type 跳过而不是报错：上游随时可能加字段，
// 报错会让整条链路在新版本上线当天就断掉。
// [参数] raw: 原始帧字节。
// [返回] 解析结果与错误。
// 最近修改时间：2026-10-18；改动原因：新增，入站帧解析入口。
func decodeFrontierFrame(raw []byte) (*frontierFrame, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("空帧")
	}
	frame := &frontierFrame{}
	pos := 0
	for pos < len(raw) {
		tag, next, err := readVarint(raw, pos)
		if err != nil {
			return nil, fmt.Errorf("读取字段标记失败: %w", err)
		}
		pos = next
		fieldNum := tag >> 3
		wireType := tag & 0x7

		switch wireType {
		case wireVarint:
			value, after, errRead := readVarint(raw, pos)
			if errRead != nil {
				return nil, fmt.Errorf("读取字段 %d 的 varint 失败: %w", fieldNum, errRead)
			}
			pos = after
			switch fieldNum {
			case 1:
				frame.SeqID = value
			case 2:
				frame.LogID = value
			case 3:
				frame.Service = value
			case 4:
				frame.Method = value
			case 12:
				frame.FrameType = value
			}

		case wireBytes:
			length, afterLen, errLen := readVarint(raw, pos)
			if errLen != nil {
				return nil, fmt.Errorf("读取字段 %d 的长度失败: %w", fieldNum, errLen)
			}
			end := afterLen + int(length)
			if end < afterLen || end > len(raw) {
				return nil, fmt.Errorf("字段 %d 长度越界: %d > %d", fieldNum, end, len(raw))
			}
			chunk := raw[afterLen:end]
			pos = end

			switch fieldNum {
			case 5:
				header, errHeader := decodeFrontierHeader(chunk)
				if errHeader != nil {
					return nil, errHeader
				}
				frame.Headers = append(frame.Headers, header)
			case 6:
				frame.PayloadEncoding = string(chunk)
			case 7:
				frame.PayloadType = string(chunk)
			case 8:
				// 复制一份：调用方可能长期持有 payload，而原始帧缓冲会被复用。
				frame.Payload = append([]byte(nil), chunk...)
			case 9:
				frame.LogIDNew = string(chunk)
			case 10:
				frame.ServerTiming = string(chunk)
			case 11:
				frame.MsgID = string(chunk)
			}

		case wireFixed64:
			if pos+8 > len(raw) {
				return nil, fmt.Errorf("字段 %d 的 fixed64 越界", fieldNum)
			}
			pos += 8

		case wireFixed32:
			if pos+4 > len(raw) {
				return nil, fmt.Errorf("字段 %d 的 fixed32 越界", fieldNum)
			}
			pos += 4

		default:
			return nil, fmt.Errorf("不支持的 wire type %d（字段 %d）", wireType, fieldNum)
		}
	}
	return frame, nil
}

// decodeFrontierHeader 解析帧头内层的 {1:key, 2:value} message。
// [参数] raw: 内层字节。
// [返回] 头键值对与错误。
// 最近修改时间：2026-10-18；改动原因：新增，头字段解析。
func decodeFrontierHeader(raw []byte) (frontierHeader, error) {
	header := frontierHeader{}
	pos := 0
	for pos < len(raw) {
		tag, next, err := readVarint(raw, pos)
		if err != nil {
			return header, fmt.Errorf("读取头字段标记失败: %w", err)
		}
		pos = next
		fieldNum := tag >> 3
		wireType := tag & 0x7

		switch wireType {
		case wireVarint:
			_, after, errSkip := readVarint(raw, pos)
			if errSkip != nil {
				return header, errSkip
			}
			pos = after

		case wireBytes:
			length, afterLen, errLen := readVarint(raw, pos)
			if errLen != nil {
				return header, fmt.Errorf("读取头字段 %d 长度失败: %w", fieldNum, errLen)
			}
			end := afterLen + int(length)
			if end < afterLen || end > len(raw) {
				return header, fmt.Errorf("头字段 %d 长度越界", fieldNum)
			}
			value := string(raw[afterLen:end])
			pos = end
			switch fieldNum {
			case 1:
				header.Key = value
			case 2:
				header.Value = value
			}

		case wireFixed64:
			pos += 8
		case wireFixed32:
			pos += 4
		default:
			return header, fmt.Errorf("头字段不支持的 wire type %d", wireType)
		}
	}
	return header, nil
}

// readVarint 读取一个 protobuf varint。
//
// 最多 10 字节（64 位），超出说明数据已损坏——继续读会把后续字段
// 当成 varint 的一部分，产生完全错乱的解析结果。
// [参数] buf: 缓冲；pos: 起始偏移。
// [返回] 值、新偏移与错误。
// 最近修改时间：2026-10-18；改动原因：新增，varint 读取。
func readVarint(buf []byte, pos int) (uint64, int, error) {
	var value uint64
	var shift uint
	for i := 0; i < 10; i++ {
		if pos+i >= len(buf) {
			return 0, pos, fmt.Errorf("varint 被截断")
		}
		b := buf[pos+i]
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, pos + i + 1, nil
		}
		shift += 7
	}
	return 0, pos, fmt.Errorf("varint 超过 10 字节")
}

// appendVarintField 追加一个 varint 字段（tag + 值）。
// [参数] buf: 目标缓冲；fieldNum: 字段号；value: 字段值。
// [返回] 追加后的缓冲。
// 最近修改时间：2026-10-18；改动原因：新增，编码辅助。
func appendVarintField(buf []byte, fieldNum int, value uint64) []byte {
	buf = appendVarint(buf, uint64(fieldNum)<<3|wireVarint)
	return appendVarint(buf, value)
}

// appendStringField 追加一个字符串字段。
// [参数] buf: 目标缓冲；fieldNum: 字段号；value: 字段值。
// [返回] 追加后的缓冲。
// 最近修改时间：2026-10-18；改动原因：新增，编码辅助。
func appendStringField(buf []byte, fieldNum int, value string) []byte {
	return appendBytesField(buf, fieldNum, []byte(value))
}

// appendBytesField 追加一个 length-delimited 字段。
// [参数] buf: 目标缓冲；fieldNum: 字段号；value: 字段字节。
// [返回] 追加后的缓冲。
// 最近修改时间：2026-10-18；改动原因：新增，编码辅助。
func appendBytesField(buf []byte, fieldNum int, value []byte) []byte {
	buf = appendVarint(buf, uint64(fieldNum)<<3|wireBytes)
	buf = appendVarint(buf, uint64(len(value)))
	return append(buf, value...)
}

// appendVarint 追加一个 varint 编码。
// [参数] buf: 目标缓冲；value: 待编码值。
// [返回] 追加后的缓冲。
// 最近修改时间：2026-10-18；改动原因：新增，编码辅助。
func appendVarint(buf []byte, value uint64) []byte {
	for value >= 0x80 {
		buf = append(buf, byte(value)|0x80)
		value >>= 7
	}
	return append(buf, byte(value))
}
