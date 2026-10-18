# qwenwork 流式 usage 终止帧被丢弃致 new-api 全量零计费根因诊断

- 报告时间：2026-10-18 18:30:00（GMT+8）
- 报告对象：qwenwork-provider **0.2.7**（生产 active_version，修复前）
- 触发场景：`new-api → cpa → qwenwork-provider`，模型 `qwen-3.8-max`，会话 `76113f53-a6d8-4916-ac36-87ad0378a3fd`
- 关联文档：
  - `doc/4-bugs/2026-10-18_030000_qwen38flash已泄漏分片断流致new-api零计费复发根因诊断.md`（qoder 系断流零计费，**根因不同，不可并案**）
  - `doc/4-bugs/2026-10-18_173000_qwenwork额度耗尽未识别致面板永久加载中根因诊断.md`（同批次的耗尽识别缺陷）

## 1. 现象

用户提供 new-api 使用日志截图，两点异常：

1. **大量 `$0` 计费记录**：`qwen-3.8-max` 请求的 Tokens 列为空、费用为 `$0`，日志文案「上游没有返回计费信息，无法扣费」。
2. **首字延迟 104.93s**：同批次另有一条 `1m 29s` 的请求。

## 2. 取证

### 2.1 上游确实返回 usage 帧（直连实测）

用有余额账号直连 MaaS `POST {base_url}/v1/chat/completions`（`stream:true`），末帧形态：

```json
data: {"choices":[{"delta":{},"index":0}],"created":1792316744,"id":"chatcmpl-...",
 "model":"qwen3.8-max-preview","object":"chat.completion.chunk",
 "usage":{"completion_tokens":24,
          "completion_tokens_details":{"reasoning_tokens":20,"text_tokens":24},
          "prompt_tokens":63,
          "prompt_tokens_details":{"cached_tokens":0,"text_tokens":63},
          "total_tokens":87}}
```

关键结构：`choices[0].delta` 是**空对象**，且**没有 `finish_reason`**，usage 挂在**顶层**。直连统计 `usage_frames=1`。

### 2.2 CPA 输出流里没有 usage（抓包实测）

对生产 CPA 发同形态请求（`stream_options.include_usage:true`），响应 2648 字节，末尾三帧：

```
data: {... "finish_reason":"stop" ...}
data: [DONE]
```

**无任何 usage 帧**。上游给了、CPA 没转发 ⇒ 丢弃点在插件侧。

### 2.3 定位到 `cleanChunkJSON`

[qwenwork/stream.go:656-666](file:///F:/cpa-plugin/qwenwork/stream.go#L656-L666)（修复前）：

```go
if len(delta) == 0 {
    if fr, _ := choice["finish_reason"].(string); fr == "" {
        return ""     // ← usage 帧在此被整帧丢弃
    }
}
```

该分支本意是清理 `{"delta":{"function_call":null}}` 归约成的空壳，但上游的 usage 终止帧同样满足「delta 为空 + 无 finish_reason」，于是被一并吞掉。用 Python 按同一逻辑模拟确认：`delta: {} len: 0 finish_reason: '' => would return '' : True`，`has usage: True`。

两条流式路径共用该函数，故同时受害：

- 异步路径 [stream.go:316](file:///F:/cpa-plugin/qwenwork/stream.go#L316)（`streamEmit`）
- 同步收集路径 [stream.go:541](file:///F:/cpa-plugin/qwenwork/stream.go#L541)（`collectUpstreamStreamOpenAI`）

### 2.4 下游计费链路

new-api 的 OpenAI chat 流式计费依赖流内 usage 帧；拿不到就回落到「上游没有返回计费信息」，`quota=0`。这与 qoder 系那次（断流 + `event: error` 不被识别）**机制完全不同**：本次流是**正常结束**的，只是缺了计费帧。

### 2.5 104.93s 首字延迟的真实成因

从生产 `token-usage-feed.ndjson` 统计 216 条 qwenwork 记录，按 `ttft_ns` 降序：

| TTFT | 总耗时 | 输入 tokens | 输出 tokens | 时间 |
|---|---|---|---|---|
| **103.99s** | 257.26s | **388,677** | 10,592 | 08:55:45 |
| 36.00s | 186.55s | **388,677** | 10,127 | 08:57:05 |
| 30.82s | 34.84s | 255,738 | 111 | 09:46:31 |
| 19.66s | 48.53s | 238,899 | 1,968 | 09:41:32 |
| 16.08s | 106.95s | 291,515 | 5,261 | 09:57:37 |

同一会话的输入 token 逐轮累积（每轮 +300~5,500），从 6.2 万一路涨到 **31.5 万**；输入涨到 31 万后，单轮 TTFT 稳定在 7~12s。

**结论：104.93s 是 38.8 万 token 预填充的上游耗时，不是插件开销，也不是「总结」阶段。** 插件对该请求的附加开销仅 MaaS 签发与请求体改写，均在毫秒级。

### 2.6 24 条零 token 记录的分布

216 条记录中 24 条 tokens 全 0，全部是**失败请求**，不是计费丢失：

| (failed, status) | 条数 |
|---|---|
| (true, 429) | 15 |
| (true, 400) | 6 |
| (true, 0) | 3 |

且不存在「output_tokens ≤ 5 的成功记录」。**用户截图里的 `$0` 与这批失败记录无关，是 §2.3 的 usage 帧丢失所致。**

## 3. 根因

`cleanChunkJSON` 的「全空 delta 整帧丢弃」规则与上游 usage 终止帧形态冲突：

- 上游把 usage 放在 `delta:{}` + 无 `finish_reason` 的 chunk 上；
- 插件把「delta 为空且无 finish_reason」一律视为噪声；
- 于是计费信息在插件出口处被抹掉，下游只能记 `$0`。

## 4. 修复

[qwenwork/stream.go:656-666](file:///F:/cpa-plugin/qwenwork/stream.go#L656-L666)：

```go
if len(delta) == 0 {
    if fr, _ := choice["finish_reason"].(string); fr == "" {
        // 顶层 usage 是终止计费帧的唯一载体：上游把它挂在
        // delta 为空的 chunk 上（无 finish_reason）。整帧丢弃会让
        // 下游（new-api 等）拿不到 token 数，全部记 $0。
        if _, hasUsage := obj["usage"]; hasUsage {
            continue
        }
        return ""
    }
}
```

`continue` 让循环继续处理其余 choice 的清理逻辑，同时**不返回 `""`**，整帧得以保留（空 `tool_calls`/`function_call` 噪声仍被清理）。

## 5. 验证

### 5.1 单测

新增 [qwenwork/cleanchunk_test.go](file:///F:/cpa-plugin/qwenwork/cleanchunk_test.go)，5 个用例：

| 用例 | 断言 |
|---|---|
| `TestCleanChunkJSONKeepsUsageOnlyFrame` | 上游真实形态整帧保留，`usage.total_tokens=87` 与 `model` 不丢 |
| `TestCleanChunkJSONKeepsUsageWithNoiseFields` | usage 帧同时带空 `tool_calls`/`function_call` 时，保留帧且清掉空壳 |
| `TestCleanChunkJSONDropsEmptyNoSignalFrame` | 无 usage、无 finish_reason 的纯空 delta 仍返回 `""` |
| `TestCleanChunkJSONKeepsFinishReason` | 带 `finish_reason` 的空 delta 帧保留 |
| `TestCleanChunkJSONKeepsRoleOnlyChunk` | 角色首帧保留 |

### 5.2 变异反证

把 `hasUsage` 分支改为 `hasUsage && false`（等价于退回旧行为）→ 精确 FAIL：

```
--- FAIL: TestCleanChunkJSONKeepsUsageOnlyFrame (0.00s)
    cleanchunk_test.go:19: usage 终止计费帧被丢弃：下游将拿不到 token 数
--- FAIL: TestCleanChunkJSONKeepsUsageWithNoiseFields (0.00s)
    cleanchunk_test.go:43: 带空壳噪声的 usage 帧被整帧丢弃
```

还原后 `cgo-shim build/vet/test` 全绿。

### 5.3 生产验收（0.2.8 热重载后）

| 项 | 结果 |
|---|---|
| 热重载 | `active_version=0.2.8 retired_version=0.2.7`，容器 Up 2 days 零重启 |
| 落盘 `.so` sha256 | `1d1ab751440efcea1087306a57fd94acfeab5b8841a8d60de17f5de67d45f87c`，与 zip 内 `.so` 逐字节一致 |
| CPA 流式响应 | 含 1 个 usage 帧：`total_tokens: 87`（修复前 0 个） |
| new-api 计费 | 18:15:58 热重载；18:16:29 起 `prompt_tokens=349312 / quota=17475`，**连续 3 条全部正常**；此前同会话连续 8 条均为 0 |

## 6. 同批次附带修复：刷新路径缺生命周期对账

开启 `lifecycle_auto` 后，耗尽账号仍未被停用。两层原因：

1. **配置落错位置**：`lifecycle_auto` 被写进实例的 `config_yaml` 子键（`config_yaml: "lifecycle_auto: true\n"`），而插件逐行解析要求该键在实例**顶层**。用宿主正规接口 `PATCH /v0/management/plugins/<id>/config` 写回顶层后，`/accounts` 立即返回 `lifecycle_auto: True`。
2. **刷新路径不跑对账**：`refresh_runner.doFetchOne` 只刷新积分缓存，不调 `reconcileOneAccount`。全插件仅面板 force 加载（`panel.go:147`）与保活时刻（`scheduler_loop.go:74`）两处触发对账，watchdog 的 10 分钟定时刷新不会。已在 `doFetchOne` 末尾补 `reconcileOneAccount(authIndex, authID, false)`——非 force 复用刚写入的缓存，不产生第二次上游请求。

生产验收：`qwenwork-2b7b9cc0-...` 从 `disabled=False` 变为 `disabled=True exhausted=True`。

## 7. 教训

1. **「清理噪声」的判定条件必须显式排除计费载体**。`delta` 为空不等于 chunk 无意义——顶层 `usage` 才是终止帧的唯一计费载体。任何「整帧丢弃」规则都应先检查是否存在不可丢失的顶层字段。
2. **上游形态要用直连实测，不能靠推理**。0.2.6 的 `session_id` 与本次的 usage 帧都是「面板成功、网关失败」的路径差异；只有直连上游抓原始 SSE 才能看到真实帧结构。
3. **TTFT 异常先看输入规模**。38.8 万 token 的预填充耗时 104s 属上游正常行为；把长上下文首字延迟当插件缺陷会走错方向。判据是「同会话输入 token 曲线 + 同规模下的 TTFT 一致性」。
4. **失败请求的零 token 与计费丢失的零 token 必须分开统计**。按 `(failed, status)` 分组后，24 条零 token 全是失败请求，与 `$0` 计费无关。
5. **生命周期功能有三处触发点，缺一处就是「开关打开了但没生效」**。补功能时必须同时盘点所有会刷新积分的路径（面板 force / 定时 watchdog / 保活），而不是只改对账函数本身。

## 8. 未验证项

- new-api 侧是否还有其它可提取 usage 的兜底路径（未穷举其全部计费分支）。
- 非流式路径（`stream:false` 由宿主转聚合）是否同样受益——插件强制 `stream=true` 后由 `aggregateCompletion` 折叠，该路径的 usage 来自同一批帧，理论上同步修复，但未单独抓包验证。
