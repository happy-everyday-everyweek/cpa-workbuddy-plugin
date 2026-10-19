---
name: project-cpa-workbuddy-plugin-sse-and-ping-optimization-rules
description: 当开发、优化或排查 cpa-workbuddy-plugin 仓库下任意服务商插件（workbuddy、workbuddy-ai、traework、qoderwork、qoder-ai、cursor、zcode、gemini）的「模型推理慢 / 流式尾部挂死 / 长超时」、「面板测试按钮（/test-active）或自动探活（active_ping）响应慢 / HTTP 200 假成功」、或「OAuth / 设备授权未打开链接即自动认证成功」时自动触发。负责强制执行 SSE 终止帧即刻退出契约、探活流式首包即停 + Keep-Alive 预热、HTTP 200 业务错误拦截，以及 OAuth 轮询防 CreatedAt 漂移假成功规范。
---

# cpa-workbuddy-plugin 流式终止、测试探活提速与 OAuth 防假成功规范

## 1. 适用范围与触发信号

- 用户反馈某插件「模型推理速度很慢」「流式生成结束后一直转圈等待」「偶发长超时」
- 用户反馈面板卡片「测试」按钮（`POST /test-active`）响应慢（尤其是 `glm-5.3-flash`、`qwen3.8-max`、`deepseek-v4.1-flash` 等思考型模型耗时 5~15 秒）
- 排查「配额耗尽或无效模型在测试/探活时仍返回活跃成功（HTTP 200 假成功）」
- 排查「点击添加账号生成 OAuth 链接后，尚未在浏览器打开链接就自动认证成功」
- 新增服务商插件或横向同步既有插件的 `stream.go`、`upstream.go`、`active_ping.go`、`host_bridge.go`、`oauth.go`

**职责边界**：本 skill 约束插件数据面 SSE 终止契约、探活首包即停、连接池预热与 OAuth 轮询真值校验；本地无 CGO 编译单测归 `cgo-plugin-isolated-test`（`scripts/cgo-shim-build.py`），发布与生产热重载归 `project-cpa-workbuddy-plugin-release-rules`。

---

## 2. 核心规则一：SSE 流式终止契约（防 Keep-Alive 尾部挂死）

### 2.1 根因机理
上游网关（OpenAI 兼容 SSE、Qoder COSY 信封 SSE、Trae `llm_utils_chat` SSE）普遍启用 HTTP/1.1 或 HTTP/2 `Keep-Alive` 长连接复用。当大模型生成完毕并下发终止信号（`data: [DONE]` 或 `event: done`）后，**服务端并不会立即主动关闭底层 TCP 连接**。
- 若插件 SSE 扫描器在解析到终止帧时使用 `continue`（而非 `break` / 返回哨兵错误），或未能识别变体终止帧，读取循环将继续调用 `r.Read()`；
- 此时读取器会死等在闲置的 Keep-Alive 连接上，直到上游空闲超时或网关超时（数十秒至数分钟），表现为「模型早已输出完毕，但客户端一直转圈或报超时」。

### 2.2 强制实现契约
1. **收到终止信号必须立即退出读取循环并主动关闭流**：
   - 标准 OpenAI / Qoder 流：统一通过 `parseQoderSSEChunk` 识别顶层裸 `data: [DONE]`、嵌套信封 `{"body":"[DONE]"}` 及扁平 chunk，一旦 `done == true`，无论同步 `collect` 还是异步 `pump` 路径都必须立即 `break` 并调用 `stream.Close()`。
   - Trae `llm_utils_chat` 流：在 `scanSSE` 中定义哨兵错误 `errTraeStreamDone`；三条路径（`collectTraeStream`、`aggregateTraeCompletion`、`pumpTraeStreamAttempt`）在 `case "done":` 中记录终态后立即 `return errTraeStreamDone`，`scanSSE` 捕获该哨兵后立即返回 `nil` 退出 `r.Read` 循环。
   - **顶层 `code` 业务错误帧必须被解析器识别，不得当作无关行丢弃（2026-10-12 新增，生产实证）**：阿里 Qoder 对超上下文/排队请求在 HTTP 200 下返回 `data: {"code":"403","message":"{...10605...isQueued:true...}"}`，随后保持 HTTP/2 Keep-Alive 心跳约 60 秒才由对端复位。若解析器（`parseQoderSSEChunk`）只认 `[DONE]` / `choices` / `id` / `error` 四类形态，该帧会落入兜底 `return "", false` 被 `continue` 跳过 → **读不到终止信号而死等连接**，客户端 60s 内零字节（new-api 侧记 `client_gone`）。修复契约：解析器必须识别「顶层存在非空 `code`」为错误帧并返回 payload；错误帧提取器（`sseErrorFrame`）必须识别顶层 `code` + `message` 组合，且 `message` 为内嵌 JSON 字符串时需逐层展开（保留首层业务码如 10605）产出可读文本；**异步 `pump`、同步 `collect`、非流式 `aggregate` 三条路径都必须在读取循环内命中错误帧即刻收尾**，非流式聚合尤其注意：先物化 inner stream 再聚合的实现必须在物化循环里就提前退出，否则仍会死等 EOF。
   - **判定「立即报错」而非换号**：排队帧文本不含 credit / rate-limit marker，`shouldRotateOnUpstreamErr` 天然为 false，请求应立即以错误收尾，不做号池内换号消耗（排队是请求级 + 时段性，非账号级；同账号 ok/bad 混合、成功请求 input 可达 289K token）。
2. **`hostHTTPDoStreamDirect` 必须使用实时流（`liveBody: resp.Body`），严禁 `io.ReadAll` 全量预缓冲**：
   - 直连流降级或无 callback 流必须返回包裹 `liveBody: resp.Body` 的 `hostHTTPStream`（配合无整体 `Timeout`、仅设 `Dial`/`TLSHandshake`/`ResponseHeaderTimeout` 的 `streamHTTPClient`）；
   - **严禁**在 `hostHTTPDoStreamDirect` 内部调用 `io.ReadAll(resp.Body)` 读完再包装成内存切片——否则 `scanSSE` 的提前退出完全失效，且在思考模型或 Keep-Alive 连接上会退化为全量阻塞等待。
3. **全链路 Context 继承与换号会话驱逐**：
   - 换号重建请求（如 `rebuildRequestWith*`）必须通过 `httpReq.Clone(httpReq.Context())` 继承原请求 Context，严禁硬编码 `context.Background()` 导致客户端断开后后台 goroutine 泄漏；
   - 换号重试前必须调用 `evictSessionBindingsForAuth(curAuthID)` 驱逐故障账号的会话粘性绑定。

---

## 3. 核心规则二：测试模型（`/test-active`）与定时探活（`active_ping`）首包即停 + Keep-Alive 预热

### 3.1 根因机理（为什么修复了主聊天流后「测试模型」仍需 5~8 秒？）
1. **思考型模型忽略 `max_tokens` 对思维链的限制**：
   - 在 Trae（`llm_utils_chat`）、智谱（`GLM-5.3` / `GLM-5.3-Flash`）、通义（`Qwen3.8-Max`）、DeepSeek（`deepseek-v4.1-flash`）等上游中，即使探活请求传入 `max_tokens: 5`，上游也只限制最终正文 token，**不限制前置思维链（`reasoning_content`）**；
   - 生产抓包实证：`glm-5.3-flash` 对 `"hi"` 在 `<1s` 内即返回第 1 个 `event: output`（`"Let"`），但随后会继续生成 **264 个思考 token（共 269 个 SSE 事件）** 才发送 `event: done`，耗时达 **5.6~8.1 秒**。
2. **`hostHTTPDo` 全量读取放大延迟**：
   - 若 `sendActivePing*` 调用非流式 `hostHTTPDo(req)`（宿主内部执行 `io.ReadAll(resp.Body)`），则必须等待全部 269 个思考事件生成完毕才能返回。
3. **HTTP 200 下的 `event: error` 假成功**：
   - 部分上游（如 Trae `llm_utils_chat`）在账号配额耗尽（`code: 4008`）、限流（`4011`）或传入内部非对话配置（`2001`）时，HTTP 状态码仍返回 `200 OK`，错误放在 SSE `event: error` 帧中；若仅检查 `resp.StatusCode == 200` 会把死号误判为活跃成功。

### 3.2 强制实现契约
1. **流式首包即停（First-Output Early Exit）**：
   - 所有插件的 `sendActivePing*` / `handleTestActiveWithAuth` 必须发起**流式请求**（`stream: true, max_tokens: 1`）；
   - 逐帧读取响应流：一旦收到**首个有效业务输出帧**（正文片段、思考片段 `reasoning_content` 或有效 chunk），立即判定模型与账号可用，退出扫描并在后台 goroutine 中异步关闭底层流与 Context（`go func() { stream.Close(); cancel() }()`），实现 **TTFT 级（0.3s~1.5s）即时返回**。
2. **拦截 HTTP 200 下的业务错误帧（严禁盲判首包成功）**：
   - **严禁仅凭 `reader.Read(buf) > 0` 盲判流建立成功**！
   - 生产实证（2026-10-11）：阿里 Qoder 面对受限或排队模型（如 `qfmodel` / Qwen3.8-Flash、`qmodel_38max`）在 HTTP 200 下首包吐出 `data: {"code":"403","message":"...10605...isQueued:true..."}`，若只读 512 字节不验内容，测试探活会误报 `ok: true, 219ms` 假成功，而真实推理会因上游长连接死等 60 秒超时崩溃。
   - 在收到首个有效输出前，若首包或帧内容包含 `event: error`、`"code":"403"`、`"code":"10605"`、`"isQueued":true` 或 JSON `error` 字段，必须如实解析错误信息并返回失败，严禁凭首包有字节流出或 HTTP 200 盲判成功。
3. **TLS Keep-Alive 连接池后台预热（消除跨境 1s 握手）**：
   - 当面板点击「测试」按钮打开模型选择弹窗（触发 `GET /models?auth_index=`）以及每次测试请求结束关闭流后，在生产宿主环境下（`hostBridgeAvailable() == true`）通过后台 goroutine 向对应上游域名发送一次轻量 `HEAD /` 请求并读尽关闭 `resp.Body`，将热连接放入 `streamHTTPClient` 的 `idleConn` 池；
   - 单测环境下（`hostBridgeAvailable() == false`）直接跳过预热，避免单测发起真实外网请求。
4. **过滤内部非对话子代理占位项**：
   - 动态模型发现接口（如 Trae `get_detail_param`）返回的内部子代理配置（`custom_model_placeholder`、`browser_use_subagent`、`computer_use_subagent`、`file_search_agent`、`explore_sub_agent_v2`、`summary`）在 `/models` 测试弹窗与随机探活候选池中必须过滤剔除，防止调用对话接口必报 `2001` 错误。

---

## 4. 核心规则三：全链路流式不缓冲（Proxy Buffering）与推理延迟判读规范

### 4.1 根因机理
全链路拓扑：`Agent/客户端 -> Nginx (443) -> New-API (5566) -> CPA (8317) -> 上游插件`。
1. **Nginx 反代缓冲盲区**：
   - Nginx 默认开启 `proxy_buffering on`，会把数据积攒到 4KB/8KB 缓冲区满才一次性推给下游；大模型每吐一个字仅几十字节，导致客户端出现长达数秒甚至几十秒的“假死转圈”；
   - 误区：在外层写 `proxy_set_header X-Accel-Buffering no;` 不会关闭 Nginx 自身缓冲（该头属于上游下发给 Nginx 的响应头，且会被子级 `location` 的 `proxy_set_header` 全部覆盖失效）。
2. **New-API 渠道测试与用量统计感知陷阱**：
   - **陷阱 1（测试机制）**：New-API 渠道列表的「测试」按钮底层强制发送非流式请求（`stream: false`），测的是服务端全量生成与 Blocking Aggregation 聚合的总耗时，绝不能用来评估流式打字机效果；
   - **陷阱 2（统计指标）**：Token 用量插件（`token-usage-feed.ndjson`）的 `ttft_ns` 记录的是收到第一个 SSE 分块的时间。当上游快速返回一个业务报错帧（如 403 10605 耗时 334ms）后进入长连接挂起 60 秒时，用量插件表格中的 TTFT 仅显示 334ms，容易被误以为“响应不慢”，但实际请求 `failed: true`、`latency_ms: 60100` 且 `total_tokens: 0`。

### 4.2 强制实现契约
1. **Nginx 反代配置规范**：
   - 反代 New-API 或 CPA 的 `location` 块必须显式配置：
     ```nginx
     proxy_buffering off;
     proxy_cache off;
     proxy_connect_timeout 60s;
     proxy_read_timeout 600s;
     proxy_send_timeout 600s;
     proxy_http_version 1.1;
     chunked_transfer_encoding on;
     proxy_set_header Connection "";
     ```
   - Docker Nginx 每次变更配置文件后，必须先通过 `docker exec nginx nginx -t` 语法检测，通过后再执行 `docker exec nginx nginx -s reload`。
2. **推理断开与慢响应排查判读法**：
   - 验证流式是否通畅，必须使用终端 `curl -N -i`（禁用终端缓冲）直测，严禁使用 New-API 渠道测试按钮判定；
   - 查断开或卡顿，必须交叉比对 `ttft_ns`、`latency_ms`、`failed` 与 `total_tokens` 四个字段：`total_tokens == 0` 且 `latency_ms >> ttft_ns` 且 `failed: true` 即为典型的“首包假响应、后段死等超时”特征。

---

## 5. 核心规则四：OAuth / Device Flow 登录轮询防“未授权自动成功”

### 4.1 根因机理
宿主文件监控器（`fsnotify`）与插件后台巡检在调用 `writeAuthFileDirect`（写临时文件后原子 rename 覆盖）刷新存量账号的积分与令牌时，会触发文件系统的 `CREATE` 事件，使宿主将存量账号的 `HostAuthFileEntry.CreatedAt` 重置为 `time.Now()`。
- 若 `handlePollLogin` 中包含通过遍历 `hostAuthList()` 检查 `f.CreatedAt.UnixNano() > lc.startedAt` 来推断“是否有新账号导入”的分支，则用户刚点开添加账号弹窗、尚未打开授权链接时，碰上后台刷新存量账号就会立即误判为“登录成功”（严重假成功漏洞）。

### 4.2 强制实现契约
- **严禁在 `handlePollLogin` 中通过扫描 `hostAuthList()` 的文件时间戳推断登录状态**；
- `handlePollLogin` 只能根据当前登录会话 `state` 对应的上游权威 OAuth / Device Token 轮询结果（如 `pollDeviceToken`、上游 token 交换接口）或显式绑定的 callback state 判定是否成功；未完成浏览器授权前必须稳定返回 `AuthLoginStatusPending`（前端显示 `wait`）。

---

## 5. 回归验证标准

1. **流式即时退出单测**：使用 `hangingReader`（提供初始 SSE 帧后在 `EOF` 前 `time.Sleep(10s)` 模拟 Keep-Alive 阻塞）验证：
   - 聊天流在收到 `[DONE]` / `event: done` 后必须在 `< 500ms` 内返回；
   - 探活函数 `sendActivePing*` 在收到第 1 个思考/正文输出帧后必须在 `< 500ms` 内返回。
2. **HTTP 200 业务错误单测**：模拟 `HTTP 200` + `event: error`（如 `4008` 额度耗尽），断言探活函数返回非 nil 错误。
3. **哨兵反证**：新增单测必须先通过临时注入必失败哨兵经 `python scripts/cgo-shim-build.py <plugin>` 证实 FAIL，再还原至全绿。
