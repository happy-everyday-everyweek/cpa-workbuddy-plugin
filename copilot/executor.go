package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleExecExecute 处理非流式推理请求，内置最多 3 次换号重试。
// [参数] raw: 宿主传入的非流式执行请求 JSON。
// [返回] 含上游响应体的信封 JSON 与错误。
// 最近修改时间：2026-10-15；改动原因：新增，Copilot 非流式转发。
func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	cfg := currentConfig()
	upstreamModel := mappedModel(req.Model, cfg)

	sa := resolveTargetAuth(req.AuthID, req.StorageJSON, upstreamModel)
	curPayload := mutateModel(req.Payload, upstreamModel)

	var lastErr error
	var finalRespBytes []byte

	// 模型级拦截不再有独立的全局闸门：被拦截的账号×模型已写入冷却，
	// 路由层会跳过它们；本请求若仍落到这里说明上游刚恢复或该组合尚未被
	// 标记，按正常流程发起即可。

	for attempt := 0; attempt < 3; attempt++ {
		if sa == nil {
			lastErr = fmt.Errorf("no available copilot account")
			break
		}

		respBytes, status, err := executeOnce(curPayload, sa, cfg)
		if err == nil {
			finalRespBytes = respBytes
			lastErr = nil
			// 一次真实成功即证明该模型已恢复，清掉该模型上残留的冷却。
			resetModelBlockedState(upstreamModel)
			break
		}

		lastErr = err
		noteAccountFailureForModel(sa.AuthID, upstreamModel, status, err.Error())

		// 11128：命中即写入账号×模型冷却并停止换号。上游拦截的是这个
		// 组合，其余账号大概率同样命中；冷却探活会在到期前回放验证。
		if isModelBlockedError(status, err.Error()) {
			noteModelBlockedHit(sa.AuthID, upstreamModel)
			lastErr = fmt.Errorf("upstream %d: %s", status, truncateForLog(err.Error(), 200))
			break
		}

		if !shouldRotateOnUpstreamErr(status, err.Error()) {
			break
		}

		nextSA, hasNext := pickNextAuth(sa.AuthID, upstreamModel)
		if !hasNext || nextSA == nil {
			break
		}
		sa = nextSA
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: finalRespBytes})
}

// handleExecStream 处理流式推理请求。
// [参数] raw: 宿主传入的流式执行请求 JSON。
// [返回] 成功返回含响应头与流标识的信封 JSON；序列化失败返回 error。
// 最近修改时间：2026-10-15；改动原因：新增，Copilot 流式转发与换号重试。
func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	cfg := currentConfig()
	upstreamModel := mappedModel(req.Model, cfg)
	startTime := time.Now()

	sa := resolveTargetAuth(req.AuthID, req.StorageJSON, upstreamModel)
	curPayload := mutateModel(req.Payload, upstreamModel)

	var lastStreamErr error
	var finalResp *http.Response
	var finalCancel context.CancelFunc

	// 模型级拦截不再有独立的全局闸门：被拦截的账号×模型已写入冷却，
	// 路由层会跳过它们，本请求正常发起即可。

	for attempt := 0; attempt < 3; attempt++ {
		if sa == nil {
			lastStreamErr = fmt.Errorf("no available copilot account")
			break
		}

		ctx, cancel := context.WithCancel(context.Background())
		httpReq, err := prepareUpstreamRequest(ctx, curPayload, sa, cfg)
		if err != nil {
			cancel()
			lastStreamErr = err
			break
		}

		resp, err := inferenceHTTPClient.Do(httpReq)
		if err != nil {
			cancel()
			lastStreamErr = err
			noteAccountFailureForModel(sa.AuthID, upstreamModel, 0, err.Error())
			nextSA, hasNext := pickNextAuth(sa.AuthID, upstreamModel)
			if !hasNext || nextSA == nil {
				break
			}
			sa = nextSA
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			cancel()
			errMsg := fmt.Sprintf("upstream status %d: %s", resp.StatusCode, string(body))
			lastStreamErr = fmt.Errorf("%s", errMsg)
			noteAccountFailureForModel(sa.AuthID, upstreamModel, resp.StatusCode, errMsg)

			// 11128：写入账号×模型冷却后停止换号，由冷却探活验证恢复。
			if isModelBlockedError(resp.StatusCode, errMsg) {
				noteModelBlockedHit(sa.AuthID, upstreamModel)
				break
			}

			if shouldRotateOnUpstreamErr(resp.StatusCode, errMsg) {
				nextSA, hasNext := pickNextAuth(sa.AuthID, upstreamModel)
				if hasNext && nextSA != nil {
					sa = nextSA
					continue
				}
			}
			break
		}

		finalResp = resp
		finalCancel = cancel
		lastStreamErr = nil
		break
	}

	authID := ""
	accountLabel := ""
	if sa != nil {
		authID = sa.AuthID
		accountLabel = sa.Label
		if accountLabel == "" {
			accountLabel = sa.Username
		}
	}

	if lastStreamErr != nil {
		streamEmitError(req.StreamID, lastStreamErr.Error())
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: streamHeaders()})
	}

	// 异步读取并逐块推送
	go pumpUpstreamStream(context.Background(), finalCancel, req.StreamID, finalResp, authID, accountLabel, upstreamModel, startTime)

	return okEnvelope(streamResponse{Headers: streamHeaders()})
}

// resolveTargetAuth 按请求指定的 auth_id 解析目标账号，缺失时回退调度器选号。
// [参数] reqAuthID: 宿主指定的账号标识；storageJSON: 宿主随请求下发的凭据快照；
// model: 已归一化的上游模型名，用于模型维度冷却判定，可为空。
// [返回] 目标账号；无可选账号时返回 nil。
// 最近修改时间：2026-10-19 02:00:00；改动原因：冷却判定带模型维度。
func resolveTargetAuth(reqAuthID string, storageJSON []byte, model string) *StoredAuth {
	if len(storageJSON) > 0 {
		var sa StoredAuth
		if err := json.Unmarshal(storageJSON, &sa); err == nil && credentialOf(&sa) != "" {
			if sa.AuthID == "" {
				sa.AuthID = authIDFor(sa.GitHubToken)
			}
			if accountRoutable(&sa, model) {
				return &sa
			}
		}
	}

	reqAuthID = strings.TrimSpace(reqAuthID)
	cleanAuthID := strings.TrimSuffix(reqAuthID, ".json")
	if reqAuthID != "" {
		accounts, err := listAllAuthFiles()
		if err == nil {
			for _, acc := range accounts {
				if acc.AuthID == reqAuthID || acc.AuthID == cleanAuthID ||
					authFileNameFor(acc.GitHubToken) == cleanAuthID+".json" {
					if accountRoutable(acc, model) {
						return acc
					}
				}
			}
		}
	}

	// 从调度器选取健康账号
	if sa := pickActiveAuth(model); sa != nil {
		return sa
	}

	// 兜底：若全在冷却中，选用未禁用的有效账号避免死锁
	accounts, err := listAllAuthFiles()
	if err == nil && len(accounts) > 0 {
		for _, acc := range accounts {
			if !acc.Disabled && credentialOf(acc) != "" && !acc.TestFailed {
				return acc
			}
		}
	}

	// 兜底：若配置文件里写死了 api_key，构造单账号
	cfg := currentConfig()
	if cfg.APIKey != "" {
		return &StoredAuth{
			AuthID:      "copilot-config-static",
			APIKey:      cfg.APIKey,
			GitHubToken: cfg.APIKey,
			Label:       "Config GitHub Token",
			Disabled:    false,
		}
	}

	return nil
}

// prepareUpstreamRequest 构造一次 Copilot 上游推理请求。
// [参数] ctx: 上下文；payload: 已改写的请求体；sa: 目标账号；cfg: 当前插件配置。
// [返回] 上游 HTTP 请求与错误。
// 最近修改时间：2026-10-15；改动原因：新增，Copilot 上游请求构造（含令牌保活与指纹注入）。
func prepareUpstreamRequest(ctx context.Context, payload []byte, sa *StoredAuth, cfg Config) (*http.Request, error) {
	token, err := ensureCopilotToken(sa)
	if err != nil {
		return nil, fmt.Errorf("ensure copilot token for %s: %w", sa.AuthID, err)
	}

	reqURL := resolveCopilotBaseURL(sa, cfg) + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	applyCopilotHeaders(httpReq.Header, sa, cfg, payload)
	httpReq.Header.Set("Authorization", "Bearer "+token)
	return httpReq, nil
}

// executeOnce 向上游发起一次非流式推理请求并读取完整响应体。
// [参数] payload: 已改写的请求体；sa: 目标账号凭据；cfg: 当前插件配置。
// [返回] 响应体字节、HTTP 状态码与错误；状态码非 2xx 时错误携带上游响应体。
// 最近修改时间：2026-10-15；改动原因：新增，Copilot 非流式单次执行内核。
func executeOnce(payload []byte, sa *StoredAuth, cfg Config) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	httpReq, err := prepareUpstreamRequest(ctx, payload, sa, cfg)
	if err != nil {
		return nil, 0, err
	}

	resp, err := inferenceHTTPClient.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if errRead != nil {
		return nil, resp.StatusCode, errRead
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(body))
	}

	return body, resp.StatusCode, nil
}

// mutateModel 改写请求体中的模型名。
//
// 特殊口径：目标模型为 auto 时必须**删除** model 字段而不是写入 "auto"。
// 上游对 "model":"auto" 返回 400 model_not_supported，省略该字段才由服务端
// 按账号档位自动选模——这是免费 / 学生档唯一可用的请求形态。
// [参数] raw: 原始请求体；model: 目标上游模型名。
// [返回] 改写后的请求体；解析失败时原样返回副本。
// 最近修改时间：2026-10-17 15:30:00；改动原因：新增 auto 语义，省略 model 字段交由上游自选。
func mutateModel(raw []byte, model string) []byte {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return append([]byte(nil), raw...)
	}
	// 1. auto 走省略字段的自动选模通道；其余情况原样写入目标模型名。
	if isAutoModelID(model) {
		delete(body, "model")
	} else if model != "" {
		body["model"] = model
	}
	out, err := json.Marshal(body)
	if err != nil {
		return append([]byte(nil), raw...)
	}
	return out
}
