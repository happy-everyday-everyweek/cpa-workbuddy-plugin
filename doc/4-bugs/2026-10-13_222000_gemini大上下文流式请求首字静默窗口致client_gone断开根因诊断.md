# gemini-3.8-flash 大上下文流式请求 client_gone 断开根因诊断

- 记录时间：2026-10-13 22:20 (GMT+8)
- 生产入口：`https://api.luode.vip`（new-api）→ `https://cpa.luode.vip`（CPA，容器 `cli-proxy-api`）
- 故障样本：2026-10-13 21:53:04 / 21:53:06 两条 `gemini-3.8-flash` 流式请求
- 关联文档：`doc/4-bugs/2026-10-11_212700_deepseek多服务商同名认领导致推理零首字节断开根因诊断.md`、`doc/4-bugs/2026-10-12_001641_Qoder上游200排队帧未被识别致new-api调用qwen3.8挂死根因诊断.md`

---

## 一、现象与判据

| 观测点 | 本次实测 | 说明 |
|---|---|---|
| new-api 侧 | `end_reason=client_gone`、`end_error="context canceled"`、`received=0`、`frt=-1000` | 客户端在收到任何字节前断开 |
| new-api 耗时 | `use_time_seconds=72 / 74` | 断连发生在 72~74 秒 |
| CPA 侧同两条请求 | **HTTP 200**、耗时 `1m8s / 1m9s`（`1b3d881b`→`6009806c`、`4ab8186a`） | CPA 认为是正常结束 |
| 失败请求的 prompt | ≈ 450,000 tokens（其中约 450,000 命中 cache） | 典型大上下文 |
| 同时段成功同类请求 | `prompt_tokens=453166`、`frt=46374`（**首字 46 秒**）、`end_reason=eof` | 首字确实极慢，但最终成功 |

**核心矛盾**：CPA 记录 200 且耗时 68~69 秒，说明上游**最终产出了数据**；但 new-api 侧在 72~74 秒判定 `client_gone`。二者相差约 4~6 秒——正是「CPA 首字产出 → 客户端已放弃」的竞态窗口。

## 二、失败规律（12 小时全量统计）

```
=== client_gone 失败: 35 条 ===
模型分布: gemini-3.8-flash = 32,  gpt-5.6-luna = 3
耗时分布: <15s = 3,  15-45s = 1,  >45s = 31   ← 88.6% 集中在 >45 秒

=== eof 成功: 2276 条 ===
prompt > 300k 的成功请求: 481 条，首字中位数 17.6s
prompt ≤ 300k 的成功请求: 522 条，首字中位数  4.7s   ← 大上下文首字慢 3.7 倍
```

**两条铁证**：
1. 失败几乎完全集中在 `gemini-3.8-flash`（32/35），且 **31/35 耗时超过 45 秒**；
2. 首字延迟与上下文规模强相关：`prompt>300k` 中位 **17.6s**，`prompt≤300k` 仅 **4.7s**，极端值达 **48.8s**。

## 三、根因（三层叠加）

### 3.1 上游首字天然极慢（触发条件）

`antigravity` provider 承载的 `gemini-3.8-flash` 在 `prompt ≈ 450k`（含 45 万 cache token）时，上游生成首 token 需 **40~50 秒**。这是模型侧 prefill 大上下文的固有耗时，非故障。

### 3.2 CPA 在首字静默期不向下游发送任何字节（核心缺口）

取证（`F:/CLIProxyAPI`，只读）：

- **`/v1/responses` 路径**（本次实际路径）：`sdk/api/handlers/openai/openai_responses_handlers.go:759` 起的主循环 `case chunk, ok := <-dataChan` —— **必须等到首个完整 SSE data frame 才 `setSSEHeaders()` + `c.Writer.Write(initialOutput.Bytes())`**。此前响应头未提交、零字节下发。
- **`/v1beta` Gemini 原生路径**同构：`sdk/api/handlers/gemini/gemini_handlers.go:222` 起 `case chunk, ok := <-dataChan`，同样是「拿到首 chunk 才设头 + 写首帧」。
- **心跳仅在首帧之后才启动**：`ForwardStream` 内部的 keep-alive ticker（`sdk/api/handlers/stream_forwarder.go:83-89`）只在 peek 到首 chunk 后被调用；首字前无 ticker、无 SSE 注释行、无空 chunk。
- **生产 `keepalive-seconds` 未配置** ⇒ 默认 `0` = 关闭（`sdk/api/handlers/handlers.go:58` `defaultStreamingKeepAliveSeconds = 0`，生产 config 的 `requests: {}` 为空）。

⇒ **40~50 秒内，下游（new-api/nginx/客户端）在 TCP 层看不到任何字节**。

### 3.3 中间层/客户端在静默窗口内放弃

- nginx（`api.luode.vip.conf:24` `proxy_read_timeout 600s`）不会在 72 秒断开，可排除。
- new-api 侧：`relay/helper/stream_scanner.go:298-301` 主循环 `case <-c.Request.Context().Done()` → `SetEndReason(ClientGone, context canceled)`；`frt=-1000` 哨兵（`relay/common/relay_info.go:564` 初值 = `startTime - 1s`）证明**从未收到首帧**。
- 即：**是调用方（上游 Agent / SDK 自身的首字节超时）先取消了 HTTP 请求**，new-api 感知为 `context canceled` → `client_gone`；随后 new-api 执行 `cleanup()` 关闭到 CPA 的 `resp.Body`（`stream_scanner.go:126-129`）。

### 3.4 为什么 CPA 记 200（可观测性缺口）

CPA 的 `gin_logger` 取 `c.Writer.Status()`。首字节前断开时 `WriteHeader` 从未被调用，gin 保持默认 `http.StatusOK`（200）；且 `statusCode < 400 && isContextCanceled(c)` 被判定为非可操作错误而不记错误日志。**结果：上游侧表现完全健康，故障只在下游可见**——这解释了「CPA 日志无异常但客户反复断连」的迷惑现象。

## 四、结论

- **不是** max_tokens 问题，**不是** 账号/额度问题，**不是** 上游报错。
- 根因是 **「大上下文 ⇒ 上游首字 40~50s」 × 「链路中间层在静默期零字节下发」×「调用方首字节超时」** 三者叠加的静默窗口竞态。
- `gemini-3.8-flash` 之所以最突出，是因为它在生产中被用于**最大上下文**（≈450k）的会话，首字延迟被放到最大。

### 4.1 关键补充：CPA 侧 keepalive 配置无效（2026-10-13 22:36 实测证伪）

生产于 **22:36 已在面板配置 `requests.streaming.keepalive-seconds: 15`**（spool 已落盘，1382 换行正常未损坏）。但实测**故障未消失**：

```
22:36:50 / 22:38:05 / 22:38:08 / 22:38:11 / 22:41:18 / 22:42:13 / 22:43:10 … 持续 client_gone，frt=-1000
```

**原因（源码确证）**：`keepalive-seconds` 驱动的 ticker **只在 `ForwardStream` 内部启动**，而 `ForwardStream` 是**首帧之后**才被调用的（见 `openai_responses_handlers.go:822`）。**首字静默期根本不在心跳覆盖范围内** —— 该配置项设计用途是「首帧之后维持长连接」，**不是**「首帧之前保温」。因此**这个配置项无法解决本故障**。

### 4.2 真正有效的修复层：new-api 侧 ping 保活（已被显式关闭）

链路是 `调用方 → new-api → CPA → Gemini`。**new-api 具备在等待上游期间向调用方发心跳的完整能力**，但被两处开关拦住：

| 关卡 | 位置 | 现状 |
|---|---|---|
| ① 总开关 `PingIntervalEnabled` | `setting/operation_setting/general_setting.go:28` | **默认 `false`**，且生产 DB `options` 表**无该项** ⇒ 走默认关闭 |
| ② 渠道级 `DisablePing` | `relay/channel/gemini/adaptor.go:157-158` | 仅当 `RelayMode == RelayModeGemini`（原生 Gemini 格式）时置 true |

**本次故障场景恰好不命中关卡②**：日志显示 `request_path: "/v1/responses"`、`request_conversion: ["OpenAI Responses"]`，且渠道 4 为 `type=1`（OpenAI 兼容，指向 `http://127.0.0.1:8317`），走的是 `relay/channel/openai/relay_responses.go:80` 的 `StreamScannerHandler` 路径 —— **`DisablePing` 保持 false**。

⇒ **只要打开关卡①（`ping_interval_enabled = true`），new-api 就会在等待上游首帧的 40~50 秒里，每 `ping_interval_seconds` 秒向调用方写 `: PING\n\n`（`relay/helper/common.go:110-123`），静默窗口即被打通。**

该开关在 new-api 管理面板**有可视化入口，但位置隐蔽**：

- **精确路径**：系统设置 → **模型与路由** → **全局模型配置** → **页面最底部**。
- **为什么容易找不到**：① 它不在「站点与品牌 / 系统信息」组，而在「模型与路由」组；② 即使进对页面，它是该页**最后两个控件**（前面是三个大 JSON 编辑器：透传开关 + 思考模型黑名单 + Chat→Responses 策略），中间隔了一条 `Separator`，**不滚到底看不到**。
- **源码锚点**：`web/src/features/system-settings/models/global-settings-card.tsx:319`（`Separator`）→ `:321-342`（`Keep-alive Ping` 开关，字段 `general_setting.ping_interval_enabled`）→ `:344-...`（`Ping Interval (seconds)`，字段 `general_setting.ping_interval_seconds`）；中文界面标题分别为「保持连接心跳」「Ping 间隔（秒）」。
- **导航注册**：`web/src/features/system-settings/models/section-registry.tsx:39-67`（`id: 'global'`，`titleKey: 'Global Model Configuration'`），由 `system-settings.config.ts:69-72` 挂到「Models & Routing」组；导航项**无条件全量渲染**（`utils/section-registry.ts:66-74`），不存在「按版本隐藏该项」的逻辑。

**旁证**：`stream_scanner.go:250` 的 `ticker.Reset(streamingTimeout)` 位于行过滤之前，故 new-api 收到 CPA 的 `: keep-alive` 也能重置其 300s 空闲超时；但 `: PING` 是 new-api **自己**发往调用方的心跳，才是解决「调用方先走」的关键。

## 五、修复方案（按推荐度排序）

> 关键约束：本仓库（`F:/cpa-plugin`）**无权修改** `F:/CLIProxyAPI` 与 `F:/new-api`（兄弟项目，只读红线）。以下方案给出落点，供在对应项目新开会话执行。

### 方案 A（推荐，零代码改动，立即生效）—— 打开 new-api ping 保活

**操作（面板路径）**：new-api 管理面板 → 系统设置 → **模型与路由** → **全局模型配置** → 滚到页面最底部 → 打开「保持连接心跳」（`Keep-alive Ping`），并把「Ping 间隔（秒）」设为 10（保存后即生效，**无需重启**）。

**操作（面板找不到时的等价 API 写法）**：该开关本质是一次 option 写入，面板与 API 完全等价（`controller/option.go:123-146` 的 `PUT /api/option/`，请求体 `{"key":...,"value":...}`，value 会被 `Interface2String` 归一为字符串）：

```bash
# 开总开关
curl -X PUT '<new-api 地址>/api/option/' \
  -H 'Authorization: Bearer <root token>' -H 'Content-Type: application/json' \
  -d '{"key":"general_setting.ping_interval_enabled","value":true}'
# 设间隔（秒）
curl -X PUT '<new-api 地址>/api/option/' \
  -H 'Authorization: Bearer <root token>' -H 'Content-Type: application/json' \
  -d '{"key":"general_setting.ping_interval_seconds","value":10}'
```

> 生产 DB `options` 表当前**无该项** ⇒ 走 `general_setting.go:28` 的默认 `false`，这正是故障未被兜住的原因。若面板确无该项，说明部署的前端产物早于当前源码（本仓库 2026-07-20 `31d70fca3` 已同时引入 7 个 section，本地 `web/dist` 产物中可 grep 到 `Keep-alive Ping` / `保持连接心跳`），此时用上面的 API 写入即可，**后端 `general_setting` 注册与 `stream_scanner.go:108` 的读取逻辑不依赖前端**。

**Ping 间隔取值（关键，决定这个开关有没有用）**：**建议 `10`（可接受 10~15），不要沿用默认的 `60`。**

- 本故障的静默窗口 = 「上游首字 40~50 秒」。若间隔取 `60`，**第一个 ping 要等到第 60 秒才发出，而调用方在 45 秒左右就已经断开** —— 间隔设 60 等于没开。
- 取 `10` 秒时，40~50 秒窗口内可发出 4~5 个 ping（`: PING\n\n`，8 字节），足以持续重置调用方的「首字节超时」。
- `10` 非拍脑袋：它就是源码兜底值 `relay/helper/stream_scanner.go:28` 的 `DefaultPingInterval = 10 * time.Second`（当配置项 ≤0 时回落到该值）。
- 上界约束：必须明显小于「调用方首字节超时」与「上游首字延迟」。实测 31/35 次失败发生在 >45 秒，故 10~15 秒安全。

> ⚠️ **面板上「建议保持此值较高，以避免上游限流」这条提示对本场景是误导**：`PingData` 写入的是 `c.Writer`（`relay/helper/common.go:110-123`），是**发往下游调用方**的 SSE 注释帧，**从不到达上游**（全仓仅两处 ping 调用——`api_request.go:442`、`stream_scanner.go:176`——都经 `PingData` → `c.Writer`；源码中不存在任何周期性写上游连接的路径）。因此 ping 频率不可能造成上游限流。该提示疑似 v1.0 前端重写（`a42b39760`）引入的通用文案，v1.0 之前的老版设置页（`git show a42b39760^:web/src/pages/Setting/Model/SettingGlobalModel.jsx`）并无此提示。

**为什么它是正解**：
- 故障发生在「调用方 ↔ new-api」这一段（调用方先走，new-api 才记 `client_gone`）；
- new-api 的 ping 是**发往调用方**的，正好补上这段静默；
- 无代码改动、无发布、无重启，改完即生效；
- 与 CPA 侧 `keepalive-seconds`（首帧后保温）职责互补，二者不冲突。

**验证判据**：观察 new-api 日志，`end_reason=client_gone AND frt=-1000` 应显著减少/消失；`received` 不再恒为 0。

### 方案 B（根治，需在 CLIProxyAPI 项目执行）—— CPA 首帧前心跳

**在 peek 首帧之前就启动 SSE 心跳**，让 CPA 自身也不留静默窗口（覆盖调用方不经 new-api 的直连场景）。

**精确改动点（三处 + 一处配置）**


> ⚠️ 关键设计约束：一旦在 peek 阶段 `Flush()`，HTTP 响应头即被提交（状态码锁定为 200），**之后无法再走 `WriteErrorResponse` 返回 4xx/5xx JSON**。因此心跳必须满足两个前提：
> ① 只在「错误尚未确定」时发送；
> ② 首个心跳发送后，若上游报错，只能改为写 SSE 错误帧（`event: error` / `event: response.failed`），不能再改状态码。
> 这与现有 `forwardResponsesStream` 的 `writeTerminalError` 语义一致，可复用。

**改动 1** · `sdk/api/handlers/openai/openai_responses_handlers.go`（peek 循环，约 723-767 行）

在 `for { select { ... } }` 中新增心跳 case。改动前（摘要）：

```go
// Peek at the first complete SSE data frame.
for {
    select {
    case <-c.Request.Context().Done():
        cliCancel(c.Request.Context().Err())
        return
    case errMsg, ok := <-errChan:
        ...   // 现有错误处理
    case chunk, ok := <-dataChan:
        ...   // 现有首帧处理
    }
}
```

改动后（新增 `keepAliveC` 分支，`headersFlushed` 用于区分「是否已提交响应头」）：

```go
// Peek at the first complete SSE data frame.
headersFlushed := false
keepAliveInterval := handlers.StreamingKeepAliveInterval(h.Cfg)
var keepAlive *time.Ticker
var keepAliveC <-chan time.Time
if keepAliveInterval > 0 {
    keepAlive = time.NewTicker(keepAliveInterval)
    defer keepAlive.Stop()
    keepAliveC = keepAlive.C
}
for {
    select {
    case <-c.Request.Context().Done():
        cliCancel(c.Request.Context().Err())
        return
    case <-keepAliveC:
        // 首帧静默期心跳：先提交 SSE 头再写注释行，避免下游长时间零字节。
        if !headersFlushed {
            setSSEHeaders()
            handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
            headersFlushed = true
        }
        _, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
        flusher.Flush()
        continue
    case errMsg, ok := <-errChan:
        if headersFlushed {
            // 响应头已提交：降级为 SSE 错误帧，不能再改状态码。
            h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) },
                make(chan []byte), errChan, framer)
            return
        }
        ...   // 原有逻辑保持不变（仍可 WriteErrorResponse 返回 JSON 错误）
    case chunk, ok := <-dataChan:
        ...   // 原有逻辑保持不变（其中 setSSEHeaders 已幂等，重复调用无副作用）
    }
}
```

**改动 2** · `sdk/api/handlers/gemini/gemini_handlers.go`（peek 循环，约 200-255 行）

同构改造：新增 `keepAliveC` case，写 `: keep-alive\n\n` + `flusher.Flush()`；同样引入 `headersFlushed`，已提交头时错误降级为 SSE 错误帧。

**改动 3** · `sdk/api/handlers/stream_forwarder.go`（可选，一致性）

`ForwardStream` 内部已有 keep-alive ticker（83-89 行）。首帧后由它接管，无需改动；但需确认 `handlers.StreamingKeepAliveInterval(h.Cfg)` 在两处取到同一值，避免心跳节奏跳变。

**改动 4** · 生产配置 `config.yaml`（**必须显式开启，默认 0 = 关闭**）

```yaml
requests:
  # 首帧静默期心跳：AI 大上下文首字可达 40~50s，需持续向链路保活。
  # 建议 15 秒，小于各层 proxy_read_timeout 与调用方首字节超时。
  streaming:
    keepalive-seconds: 15
```

> 注意：生产 `/usr/local/src/cli-proxy-api/config/config.yaml` 当前**没有 `requests` 段**（`requests: {}`）。按项目既有红线，配置变更必须走管理 API `PUT /v8/management/config`，**严禁直接读写 `config_store` 表**（2026-10-11 两次生产事故成因，详见 `doc/4-bugs/2026-10-11_1812_生产CPA配置转义损坏二次复发与热重载修复.md`）。

**验证方式（改后必做）**

1. 编译门禁：`CGO_ENABLED=1 go build ./...` 全绿；针对新增心跳分支补单测（模拟「上游 30s 不产首帧」，断言这段时间内客户端已收到 ≥1 个 `: keep-alive`）。
2. 生产验收：对 `gemini-3.8-flash` 发起 `prompt>300k` 的流式请求，new-api 日志 `end_reason` 应为 `eof`、`frt` 为真实毫秒值、`prompt_tokens>0`。
3. 反向验证：把 `keepalive-seconds` 设回 0，确认 `client_gone` 复现——证明改动确为有效因子。


### 方案 B（本仓库可做，降级缓解）

**降低单请求上下文规模**，把首字拉回可接受区间：

- 生产实测：`prompt≤300k` 时首字中位仅 4.7s，远低于调用方超时窗口。
- 措施：对 `gemini-3.8-flash` 启用更积极的会话压缩 / 历史截断；或将超大上下文的会话路由到首字更快的小上下文模型。
- 局限：治标不治本，且依赖调用方上下文策略。

### 方案 C（调用方侧，最快见效）

调高调用方（Agent/SDK）的**首字节超时**至 90~120 秒，覆盖最坏 48.8s 的首字延迟。

## 六、验证与防复发

1. **修复后验收判据**：`gemini-3.8-flash` 大上下文请求的 new-api 日志中，`end_reason` 应从 `client_gone` 转为 `eof`，`frt` 从 `-1000` 转为真实毫秒值，且 `prompt_tokens > 0`（可正常计费扣款）。
2. **监控**：对 `end_reason=client_gone AND frt=-1000` 建立告警——该组合等价于「首字未产出即断连」，是本次故障的指纹。
3. **回归样本**：`202610131351521588589728268d9d64M1jjvFF`（21:53:04）与 `202610131351522636497188268d9d6eedz2wYA`（21:53:06）。

## 七、当前状态

- 诊断：已完成，证据链闭合（两侧日志 + 源码 + 12 小时全量统计）。
- 修复：**未实施**（受跨项目只读红线约束，本仓库不能改 `F:/CLIProxyAPI`）。
  - 方案 A 的精确 patch 已写入本文档第五节「精确改动点」，可在 `F:/CLIProxyAPI` 目录新开会话直接执行。
  - 方案 B / C 待用户选定。
- 用户已在本轮表达「授权当前会话更新 F:/CLIProxyAPI」，但本仓库 `AGENTS.md`「跨项目写入红线」明确规定**不存在「取得用户授权后即可执行」的例外**，故仍以 patch 交付，未越界写入。

