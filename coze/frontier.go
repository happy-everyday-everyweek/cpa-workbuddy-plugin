package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 本文件实现扣子 frontier WebSocket 客户端。
//
// 为什么必须走 WS：message/chat 是异步提交，只返回用户消息 ID；
// message/list 只返回已落库的完整消息（实测约 4 秒延迟），拿不到增量。
// 实时 token 流只从这条 WS 来（2026-10-18 实测 44 条 reply 增量帧）。
//
// 连接参数必须用 claw 这套：aid=751148、fpid=1289、xsack=0。
// /api/fullstack_chux/v1/frontier/connection-params 返回的 aid=578287 /
// message_service_id=33555616 是另一条通道，连上去只能收到 hi 心跳，
// 收不到任何业务帧（本轮踩坑实证）。

const (
	// frontierWSHost 是 frontier WebSocket 的服务地址。
	frontierWSHost = "wss://frontier.coze.cn"

	// frontierWSPath 是 WebSocket 升级路径。
	frontierWSPath = "/ws/v2"

	// frontierWSProtocol 是握手子协议。
	//
	// 缺失该子协议时服务端仍会 101 升级，但后续帧不走 protobuf，
	// 表现为「连上了但什么都收不到」。
	frontierWSProtocol = "pbbp2"

	// frontierAppID 是 claw 通道的 app_id（页面硬编码常量）。
	frontierAppID = "751148"

	// frontierProductID 是 claw 通道的 product_id（页面硬编码常量）。
	frontierProductID = "1289"

	// frontierHeartbeatInterval 是心跳间隔。
	//
	// 页面实测 10005ms，这里对齐 10 秒；间隔过长会被网关判为空闲连接，
	// 过短则是无谓的上行流量。
	frontierHeartbeatInterval = 10 * time.Second

	// frontierHeartbeatTimeout 是等待心跳应答的上限。
	frontierHeartbeatTimeout = 4 * time.Second

	// frontierHeartbeatMessage 是心跳报文（纯文本，不是 protobuf 帧）。
	frontierHeartbeatMessage = "hi"

	// frontierWriteTimeout 是单次写超时。
	frontierWriteTimeout = 5 * time.Second

	// frontierHandshakeTimeout 是握手超时。
	frontierHandshakeTimeout = 10 * time.Second

	// frontierMaxReconnectDelay 是重连退避上限。
	frontierMaxReconnectDelay = 30 * time.Second

	// frontierInitialReconnectDelay 是首次重连延迟。
	frontierInitialReconnectDelay = time.Second

	// frontierPingLossLimit 是连续心跳丢失多少次判定连接已死。
	//
	// 取 2：单次丢失可能是抖动，连续两次基本可判死；
	// 再高会让断线发现变慢，用户侧表现为「卡住不动」。
	frontierPingLossLimit = 2

	// frontierReadTimeout 是单次读操作的超时。
	//
	// 必须显著短于「心跳间隔 × 丢失上限」：服务端异常关闭时
	// 客户端可能收不到 close 帧（实测拿到 1006 unexpected EOF），
	// 只能靠读超时兜底发现。25 秒会让用户对着卡住的界面等太久。
	frontierReadTimeout = 20 * time.Second
)

// frontierEvent 是一条已解析的业务事件。
type frontierEvent struct {
	// PayloadType 是事件名（实测 session / message）。
	PayloadType string
	// Payload 是原始 JSON 载荷。
	Payload []byte
}

// frontierMessage 是 reply 消息的解析结果。
//
// 同时承载两种形态：
//   - Delta 是增量片段（来自 content.message_data.reply.content）；
//   - Snapshot 是累积全文（来自 content_v2.session.final）；
//   - Phase 区分 event_start / event_delta / event_final。
type frontierMessage struct {
	// ID 是消息 ID（同一轮内 start 与 delta 相同）。
	ID string
	// Phase 是 content_v2.session.phase。
	Phase string
	// SeqNum 是轮内序号，用于排序与去重。
	SeqNum string
	// TurnID 标识一轮对话。
	TurnID string
	// Delta 是本帧新增的文本片段。
	Delta string
	// Snapshot 是 event_final 携带的累积全文。
	Snapshot string
	// ReplyToMessageID 是配对用户消息的键。
	ReplyToMessageID string
	// Stream 表示该帧是否仍处于流式过程中。
	Stream bool
}

// frontierRawMessage 是 WS 载荷的顶层结构。
//
// 用户消息与进度帧没有 message_type，只有 AI 回复有；
// 因此 message_type 为空不代表解析失败。
type frontierRawMessage struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	ClawID      string `json:"claw_id"`
	MessageType string `json:"message_type"`
	Content     string `json:"content"`
	ContentV2   string `json:"content_v2"`
	Reference   struct {
		ID string `json:"id"`
	} `json:"reference_message"`
}

// frontierContentInner 是 content 内层 JSON 的结构。
type frontierContentInner struct {
	ID          string `json:"id"`
	MessageType string `json:"message_type"`
	MessageData struct {
		Reply struct {
			Content          string `json:"content"`
			ReplyToMessageID string `json:"reply_to_message_id"`
			Stream           bool   `json:"stream"`
		} `json:"reply"`
	} `json:"message_data"`
	TurnID string `json:"turn_id"`
}

// frontierContentV2 是 content_v2 内层 JSON 的结构。
type frontierContentV2 struct {
	Session struct {
		Phase   string `json:"phase"`
		SeqNum  string `json:"seq_num"`
		TurnID  string `json:"turn_id"`
		EventID string `json:"event_id"`
		Final   struct {
			Type              string `json:"type"`
			AgentMessageEvent struct {
				Content []struct {
					Type string `json:"type"`
					Text struct {
						Content string `json:"content"`
					} `json:"text"`
				} `json:"content"`
			} `json:"agent_message_event"`
		} `json:"final"`
	} `json:"session"`
}

// frontierClient 是一条 frontier WS 连接。
//
// 生命周期由调用方通过 Run 驱动：Run 会阻塞直到 ctx 取消，
// 期间负责心跳、重连与事件分发。
type frontierClient struct {
	key     *frontierKeyResponse
	dialer  *websocket.Dialer
	onEvent func(frontierEvent)
	logf    func(level, message string, fields map[string]any)
	// cookieHeader 是账号的会话 Cookie。
	//
	// 握手必须带上它：不带时网关照样返回 101，但只推 hi 心跳，
	// 一帧业务数据都不给（2026-10-18 生产对照实验：不带 6 帧全 hi，
	// 带上 25 帧含 21 条业务帧）。这是「连上了却收不到回复」的根因。
	cookieHeader string
	// serverURL 覆盖默认的 frontier 主机，仅供测试指向 httptest 服务端。
	serverURL string
	// readTimeout 覆盖默认读超时，仅供测试缩短等待。
	readTimeout time.Duration
	mu          sync.Mutex
	conn        *websocket.Conn
	closed      bool
}

// newFrontierClient 构造 WS 客户端。
// [参数] key: get_frontier_key 返回的凭证；cookieHeader: 账号会话 Cookie；
// onEvent: 事件回调。
// [返回] 客户端。
// 最近修改时间：2026-10-18；改动原因：握手补 Cookie，修复只收心跳不收业务帧。
func newFrontierClient(key *frontierKeyResponse, cookieHeader string, onEvent func(frontierEvent)) *frontierClient {
	return &frontierClient{
		key:          key,
		dialer:       websocket.DefaultDialer,
		onEvent:      onEvent,
		logf:         hostLogf,
		cookieHeader: cookieHeader,
	}
}

// url 构造带凭证的 WS 地址。
//
// 参数顺序与页面一致：部分网关对参数顺序敏感（签名类网关），
// 保持与实测抓包相同可以减少变量。
// [参数] 无。
// [返回] 完整 WS 地址。
// 最近修改时间：2026-10-18；改动原因：新增，连接地址唯一出口。
func (c *frontierClient) url() string {
	query := url.Values{}
	query.Set("device_platform", "web")
	query.Set("version_code", "fws_1.0.0")
	query.Set("access_key", c.key.AccessKey)
	query.Set("fpid", frontierProductID)
	query.Set("aid", frontierAppID)
	query.Set("device_id", c.key.DeviceID)
	query.Set("xsack", "0")
	query.Set("xaack", "0")
	query.Set("xsqos", "0")
	query.Set("qos_sdk_version", "2")

	host := frontierWSHost
	if c.serverURL != "" {
		// httptest 给的是 http://，WS 需要 ws://。
		host = strings.Replace(c.serverURL, "http://", "ws://", 1)
		host = strings.Replace(host, "https://", "wss://", 1)
	}
	return host + frontierWSPath + "?" + query.Encode()
}

// Run 建立连接并持续收帧，直到 ctx 取消。
//
// 重连采用指数退避（1s 起，上限 30s），连接成功后重置退避；
// 不在此处做「游标续传」——claw 通道不依赖游标，重连后新帧照常推送
// （页面自身连接从未发过订阅帧，实测业务帧正常）。
// [参数] ctx: 生命周期控制。
// [返回] ctx 取消时返回 nil；其它错误在重试耗尽前不会返回。
// 最近修改时间：2026-10-18；改动原因：新增，连接主循环。
func (c *frontierClient) Run(ctx context.Context) error {
	delay := frontierInitialReconnectDelay
	for {
		if ctx.Err() != nil {
			return nil
		}

		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			c.logf("warn", "frontier 连接中断", map[string]any{"error": err.Error(), "retry_in": delay.String()})
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}

		// 成功连上过就重置退避，避免一次长时间稳定后的偶发断线
		// 仍然等满 30 秒。
		if err == nil {
			delay = frontierInitialReconnectDelay
		} else if delay < frontierMaxReconnectDelay {
			delay *= 2
			if delay > frontierMaxReconnectDelay {
				delay = frontierMaxReconnectDelay
			}
		}
	}
}

// runOnce 建立一次连接并跑完它的生命周期。
// [参数] ctx: 生命周期控制。
// [返回] 连接错误；ctx 取消时返回 nil。
// 最近修改时间：2026-10-18；改动原因：新增，单次连接生命周期。
func (c *frontierClient) runOnce(ctx context.Context) error {
	header := http.Header{}
	header.Set("User-Agent", webUserAgent)
	header.Set("Origin", cozeSiteHost)
	if c.cookieHeader != "" {
		header.Set("Cookie", c.cookieHeader)
	}

	dialer := *c.dialer
	dialer.HandshakeTimeout = frontierHandshakeTimeout
	dialer.Subprotocols = []string{frontierWSProtocol}

	conn, resp, errDial := dialer.DialContext(ctx, c.url(), header)
	if errDial != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return fmt.Errorf("WS 握手失败（HTTP %d）：%w", status, errDial)
	}
	defer conn.Close()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.conn = conn
	c.mu.Unlock()

	c.logf("info", "frontier 已连接", map[string]any{"device_id": c.key.DeviceID})

	// 读循环独占 conn：gorilla 不允许并发读，心跳只写不读。
	// 心跳协程通过 ctx 或连接关闭感知退出，不在 defer 里空等 ticker。
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		c.heartbeatLoop(heartbeatCtx, conn)
	}()

	readErr := c.readLoop(ctx, conn)

	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()

	// 先取消心跳，再关连接：心跳协程在 select 里立即返回，
	// 不必等下一个 10 秒 tick。
	stopHeartbeat()
	_ = conn.Close()
	<-heartbeatDone
	return readErr
}

// heartbeatLoop 周期性发送文本心跳。
//
// 只在读超时（说明对端长时间无任何帧）时计入丢失；正常收帧期间
// 即使心跳应答被合并进业务帧，也不应判死。
// [参数] ctx: 生命周期；conn: 连接。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，保活循环。
func (c *frontierClient) heartbeatLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(frontierHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			closed := c.closed || c.conn != conn
			c.mu.Unlock()
			if closed {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(frontierWriteTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, []byte(frontierHeartbeatMessage)); err != nil {
				c.logf("debug", "frontier 心跳发送失败", map[string]any{"error": err.Error()})
				return
			}
		}
	}
}

// readLoop 持续读取并分发帧。
//
// 读超时设得比心跳间隔略长：若对端在多个心跳周期内完全无帧，
// 说明连接已死，主动断开让上层重连，而不是无限等待。
// [参数] ctx: 生命周期；conn: 连接。
// [返回] 连接错误；ctx 取消时返回 nil。
// 最近修改时间：2026-10-18；改动原因：新增，入站帧处理。
func (c *frontierClient) readLoop(ctx context.Context, conn *websocket.Conn) error {
	readTimeout := c.readTimeout
	if readTimeout <= 0 {
		readTimeout = frontierReadTimeout
	}
	// 读超时到点后 gorilla 返回 i/o timeout，需要断开重连；
	// 同时用 ctx 取消打断阻塞读，避免 ctx 到期后仍卡在 ReadMessage 里。
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-stopWatch:
		}
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

		messageType, raw, errRead := conn.ReadMessage()
		if errRead != nil {
			if ctx.Err() != nil {
				return nil
			}
			if websocket.IsCloseError(errRead, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return fmt.Errorf("读取 WS 帧失败：%w", errRead)
		}

		// 文本帧只有心跳应答（"hi"），没有业务内容。
		if messageType == websocket.TextMessage {
			continue
		}
		c.dispatchFrame(raw)
	}
}

// dispatchFrame 解析一帧并回调业务事件。
//
// 只处理 service=33555657 的业务帧：同一条连接上还可能有其它
// service 的控制帧，混着处理会把内部状态帧当成回复。
// [参数] raw: 原始帧字节。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，帧分发。
func (c *frontierClient) dispatchFrame(raw []byte) {
	frame, err := decodeFrontierFrame(raw)
	if err != nil {
		c.logf("debug", "frontier 帧解析失败", map[string]any{"error": err.Error()})
		return
	}
	if frame.Service != frontierServiceClaw {
		return
	}
	if len(frame.Payload) == 0 || frame.PayloadType == "" {
		return
	}
	if c.onEvent != nil {
		c.onEvent(frontierEvent{PayloadType: frame.PayloadType, Payload: frame.Payload})
	}
}

// Close 主动关闭连接。
// [参数] 无。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，生命周期收尾。
func (c *frontierClient) Close() {
	c.mu.Lock()
	conn := c.conn
	c.closed = true
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// parseFrontierMessage 把一帧载荷解析成结构化的 reply 消息。
//
// 非 reply 帧（用户消息、进度、状态）返回 found=false，
// 调用方据此跳过——它们不承载正文增量。
// [参数] payload: 帧载荷 JSON。
// [返回] 解析结果与是否命中 reply。
// 最近修改时间：2026-10-18；改动原因：新增，reply 帧解析。
func parseFrontierMessage(payload []byte) (*frontierMessage, bool) {
	var raw frontierRawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, false
	}
	if raw.MessageType != "reply" {
		return nil, false
	}

	msg := &frontierMessage{ID: raw.ID}

	// content 承载增量片段；它本身是 JSON 字符串，需要二次解析。
	if inner := strings.TrimSpace(raw.Content); inner != "" {
		var parsed frontierContentInner
		if err := json.Unmarshal([]byte(inner), &parsed); err == nil {
			msg.Delta = parsed.MessageData.Reply.Content
			msg.ReplyToMessageID = parsed.MessageData.Reply.ReplyToMessageID
			msg.Stream = parsed.MessageData.Reply.Stream
			msg.TurnID = parsed.TurnID
			if msg.ID == "" {
				msg.ID = parsed.ID
			}
		}
	}

	// content_v2 承载阶段与累积快照；同样需要二次解析。
	if innerV2 := strings.TrimSpace(raw.ContentV2); innerV2 != "" {
		var parsed frontierContentV2
		if err := json.Unmarshal([]byte(innerV2), &parsed); err == nil {
			msg.Phase = parsed.Session.Phase
			msg.SeqNum = parsed.Session.SeqNum
			if msg.TurnID == "" {
				msg.TurnID = parsed.Session.TurnID
			}
			for _, item := range parsed.Session.Final.AgentMessageEvent.Content {
				if item.Type == "text" {
					msg.Snapshot += item.Text.Content
				}
			}
		}
	}

	return msg, true
}

// dedupeKey 生成去重键。
//
// 必须含 seq_num：同一轮的 event_start 与 event_delta 共享同一个 id，
// 仅按 id 去重会把真正的首个增量丢掉（实测）。
// [参数] 无。
// [返回] 去重键。
// 最近修改时间：2026-10-18；改动原因：新增，重复帧过滤。
func (m *frontierMessage) dedupeKey() string {
	if m == nil {
		return ""
	}
	return m.TurnID + "|" + m.SeqNum + "|" + m.Phase
}

// isFinal 判断该帧是否标志本轮结束。
// [参数] 无。
// [返回] 是收尾帧时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，收尾判定。
func (m *frontierMessage) isFinal() bool {
	return m != nil && m.Phase == "event_final"
}
