# new-api 使用日志流式请求 client_gone 取消致零计费被判「失败」根因诊断

- 发现时间：2026-10-15 22:36（用户报告 request_id `202610151435151689416798268d9d6kJzCtHT1`）
- 报告人：luode
- 影响对象：new-api 使用日志中标记为 `streaming → 200` 但结论列为「✗ / 失败」的流式请求
- 严重级别：低（非插件/上游故障，属客户端主动取消 + 计费信息缺失的可见判定，不影响服务健康）
- 承载层：new-api 使用日志展示与计费口径（非 CPA 插件、非上游 provider）

---

## 一、现象

用户在 new-api 管理台「使用日志」看到一条记录：HTTP 状态 `streaming → 200`（流式响应头已正常发出、连接成功建立），但**结论列判定为失败（✗）**，且 Tokens 显示 `0/0`、耗时 `1m1s`。用户给的 request_id 为 `202610151435151689416798268d9d6kJzCtHT1`。

> 注意：截图顶部高亮的 `glm-5.1`（`56.4K→219`、`21.90s`）与正文所给 request_id **不是同一条**。按该 request_id 在生产库精确唯一命中的是 `deepseek-v4.1-flash`、`61s`、`0/0 token` 的那条（即红框圈住的底部行）。生产 new-api 库中 `model_name='glm-5.1'` 记录数为 0，`glm` 系只有 `glm-5.3-flash`，因此截图的 glm-5.1 行来自另一实例/视图，本次只诊断正文 request_id 命中的这条。

## 二、证据链（生产只读取证）

### 2.1 new-api 日志行（`oneapi.logs` 表，按 request_id 精确唯一命中）

```
id                     = 326072
created_at             = 1792074976  -> 北京时间 2026-10-15 22:36:16
model_name             = deepseek-v4.1-flash
type                   = 2 (流式消费日志)
is_stream              = 1
prompt_tokens          = 0
completion_tokens      = 0
quota                  = 0
use_time               = 61 (秒)
request_path           = /v1/responses
request_conversion     = ["OpenAI Responses"]
frt                    = 4861 (ms，首字节用时，正常值 => 非零首字节静默)
stream_status.status   = "error"
stream_status.end_reason = "client_gone"
stream_status.end_error  = "context canceled"
```

时间戳自洽：`request_id` 内嵌 `20261015143515`（UTC 14:35:15 = 北京 22:35:15）+ `use_time 61s` = `22:36:16` = `created_at` = 用户报告时间，三者一致，确认这条就是所查记录。

### 2.2 失败判定来源（new-api 前端 + 后端）

- 后端流式收口判据：[stream_status.go:88](file:///F:/new-api/relay/common/stream_status.go#L88) `IsNormalEnd()` 只认 `done / eof / handler_stop`；本条 `end_reason=client_gone` 不属正常收口 → `stream_status.status="error"`。
- 前端结论渲染：[timing-metrics-cell.tsx:163](file:///F:/new-api/web/src/features/usage-logs/components/timing-metrics-cell.tsx#L163) `showStreamError = isStream && streamStatus && streamStatus.status !== 'ok'`，命中即显示红色告警图标与「Stream Status: Error / client_gone」。
- 结论列「失败」直接来自 `stream_status.status="error"`，而非 HTTP 码：HTTP 是 200（响应头在流式开始即发出，之后客户端取消不会改写已发送的状态行），所以呈现「`streaming → 200` 但判失败」。

### 2.3 计费信息缺失（quota=0 的成因）

- `/v1/responses` 的 usage 在流末 `response.completed`（或 `response.incomplete`）终态帧才携带。
- 客户端在 `use_time=61s` 时取消（`context canceled` → `client_gone`），new-api 在收到终态 usage 帧之前就断开，故 `prompt_tokens/completion_tokens=0`、`quota=0`，无法扣费。
- 与 [[reference-zero-body-length-truncation-fingerprint]] 区分：该指纹是 `end_reason=eof`/`status:ok` 的「零正文截断」；本条是 `end_reason=client_gone`/`status:error` 的「客户端取消」，属连接层取消，不是生成层截断，两类严禁并案。

## 三、根因

**客户端在本次流式对话进行到约 61 秒时主动取消连接（`context canceled`），new-api 将 `client_gone` 判定为非正常收口（`stream_status.status="error"`），据此在结论列标记为「✗ 失败」；由于取消发生在 `/v1/responses` 终态 usage 帧到达之前，计费 token 与 quota 全部记为 0。**

这是「成功建立并出流（HTTP 200）→ 用户侧提前中断 → 无终态 usage → 判失败且零计费」的正常组合，不是插件、上游或网络故障。`frt=4861ms`（首字节正常）排除了零首字节静默窗口（`frt=-1000`）那一类故障。

依据项目记忆 [[reference-inference-chain-observability]]：`client_gone` 的取消方需单独核实，本实例 nginx `proxy_read_timeout 300s`、new-api 无流式超时项，61s 远未到网关超时 → 取消来自**终端客户端自身**（用户停掉/切走该轮对话）。

## 四、结论与处置

- **定性**：非 Bug。是客户端主动取消 + Responses 流式「usage 仅在终态帧」的计费口径共同导致的可见「失败/零计费」标记。
- **是否需要修复**：
  - 若产品口径认为「用户取消」不应展示为「失败」，可在 new-api 侧把 `client_gone` 与真正的上游错误（`scanner_error`/`timeout`/`panic`/上游 4xx5xx）在结论列分流展示（例如 client_gone 标「已取消」而非「失败」）。这是展示口径优化，非必须。
  - 计费丢失（取消即 0 扣费）是 OpenAI Responses 协议固有行为：客户端取消发生在 usage 帧之前，本就无法补计；与 [[reference-zero-body-length-truncation-fingerprint]] 里方案 B（补 `response.incomplete` 透传 usage）不同源——那条针对「上游已产出终态帧但被丢弃」，本条根本没有终态帧。
- **回归影响**：无。不改动任何生产代码。

## 五、待确认

- 截图顶部 `glm-5.1`（`56.4K→219`、`21.90s`）那行的失败原因未取证（生产库无该模型记录，属另一实例）。如需诊断该条，请提供它所在实例/面板地址或其自身 request_id，再单独立案。
