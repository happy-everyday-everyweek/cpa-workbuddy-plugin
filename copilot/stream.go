package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// streamHeaders 返回流式响应的标准头。
func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// streamEmit 向宿主 stream 发送一个 chunk。
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

// streamEmitError 向宿主发送流式错误终止信号。
func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "error": message})
	_, _ = hostCall(pluginabi.MethodHostStreamEmit, body)
}

// streamClose 关闭宿主流。
func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

// pumpUpstreamStream 后台读取上游 SSE 响应并将数据逐块推送给宿主。
//
// 关键契约（对齐既有插件成熟策略）：
//  1. 遇到 `[DONE]` 必须立即 break 退出并关闭连接，严禁 continue 死等 Keep-Alive；
//  2. 使用 resp.Body 实时逐行读取（liveBody 语义），不做全量预缓冲；
//  3. HTTP 200 下仍可能出现 `event: error` 业务错误帧，须识别并上报。
//
// [参数] ctx: 上下文；cancel: 取消函数；streamID: 宿主流标识；
// resp: 上游响应；authID/accountLabel: 账号标识；upstreamModel: 已归一化的上游
// 模型名，用于成功时清除模型级熔断；startTime: 请求起始时间。
// [返回] 无。
// 最近修改时间：2026-10-18 23:40:00；改动原因：新增 upstreamModel 参数以支持模型级风控熔断。
func pumpUpstreamStream(
	ctx context.Context,
	cancel context.CancelFunc,
	streamID string,
	resp *http.Response,
	authID, accountLabel string,
	upstreamModel string,
	startTime time.Time,
) {
	defer func() {
		cancel()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		streamClose(streamID)
	}()

	reader := bufio.NewReader(resp.Body)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))

			// 1. 终止哨兵：收到即结束，不再等待后续 Keep-Alive 空闲帧。
			if trimmed == "data: [DONE]" || trimmed == "data:[DONE]" {
				_ = streamEmit(streamID, line)
				// 干净终止即证明该模型已恢复，清掉熔断残留。
				resetModelCircuit(providerName, upstreamModel)
				return
			}

			// 2. 业务错误帧：HTTP 200 下的 event: error 需上报。
			if strings.HasPrefix(trimmed, "event: error") {
				streamEmitError(streamID, "上游返回业务错误帧")
				return
			}

			if errEmit := streamEmit(streamID, line); errEmit != nil {
				// 客户端可能已断开
				return
			}
		}

		if err != nil {
			if err != io.EOF {
				streamEmitError(streamID, err.Error())
				// 读到此处说明响应头已成功返回（StatusCode 恒为 2xx），失败发生在
				// 流读取阶段，属传输层故障。必须按 status 0 记账：传 200 会被
				// classifyUpstreamFailure 判为成功响应而不计入冷却，导致真实断流漏记。
				noteAccountFailureForModel(authID, upstreamModel, 0, err.Error())
				return
			}
			break
		}
	}
	resetAccountFailover(authID)
	// 读到干净 EOF 同样证明该模型已恢复，清掉熔断残留。
	resetModelCircuit(providerName, upstreamModel)
}
