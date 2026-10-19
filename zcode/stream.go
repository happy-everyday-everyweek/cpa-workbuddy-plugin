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
	"github.com/tidwall/gjson"
)

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
func pumpUpstreamStream(
	ctx context.Context,
	cancel context.CancelFunc,
	streamID string,
	resp *http.Response,
	authID, accountLabel, model, upstreamModel string,
	startTime time.Time,
) {
	defer func() {
		cancel()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		streamClose(streamID)
	}()

	var inTokens, outTokens, cacheReadTokens, cacheCreationTokens int64
	var ttftNS uint64
	firstByteSeen := false
	reader := bufio.NewReader(resp.Body)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !firstByteSeen {
				firstByteSeen = true
				if d := time.Since(startTime); d > 0 {
					ttftNS = uint64(d.Nanoseconds())
				}
			}

			lineStr := strings.TrimSpace(string(line))
			if strings.HasPrefix(lineStr, "data:") {
				dataPayload := strings.TrimSpace(strings.TrimPrefix(lineStr, "data:"))
				if dataPayload != "" && dataPayload != "[DONE]" {
					// 提取 token 用量（Anthropic 协议）
					if it := gjson.Get(dataPayload, "message.usage.input_tokens").Int(); it > 0 {
						inTokens = it
					}
					if ot := gjson.Get(dataPayload, "usage.output_tokens").Int(); ot > 0 {
						outTokens = ot
					}
					if crit := gjson.Get(dataPayload, "message.usage.cache_read_input_tokens").Int(); crit > 0 {
						cacheReadTokens = crit
					}
					if ccit := gjson.Get(dataPayload, "message.usage.cache_creation_input_tokens").Int(); ccit > 0 {
						cacheCreationTokens = ccit
					}
				}
			}

			if errEmit := streamEmit(streamID, line); errEmit != nil {
				// 客户端可能已断开
				return
			}
		}

		if err != nil {
			durationMS := time.Since(startTime).Milliseconds()
			detail := usageTokensDetail{
				InputTokens:         inTokens,
				OutputTokens:        outTokens,
				CacheReadTokens:     cacheReadTokens,
				CacheCreationTokens: cacheCreationTokens,
				TotalTokens:         inTokens + outTokens,
			}
			if err != io.EOF {
				streamEmitError(streamID, err.Error())
				publishUsage(authID, accountLabel, model, upstreamModel, startTime, detail, durationMS, resp.StatusCode, true, err.Error(), ttftNS)
				// 读到此处说明响应头已成功返回（StatusCode 恒为 2xx），失败发生在
				// 流读取阶段，属传输层故障。必须按 status 0 记账：传 200 会被
				// classifyUpstreamFailure 判为成功响应而不计入冷却，导致真实断流漏记。
				noteAccountFailureForModel(authID, upstreamModel, 0, err.Error())
				return
			}
			break
		}
	}

	durationMS := time.Since(startTime).Milliseconds()
	detail := usageTokensDetail{
		InputTokens:         inTokens,
		OutputTokens:        outTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: cacheCreationTokens,
		TotalTokens:         inTokens + outTokens,
	}
	publishUsage(authID, accountLabel, model, upstreamModel, startTime, detail, durationMS, resp.StatusCode, false, "", ttftNS)
	resetAccountFailover(authID)
	// 一次真实成功即证明该模型已恢复，清掉熔断残留。
	resetModelCircuit(providerName, upstreamModel)
}
