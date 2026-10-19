# Source Notes

## 2026-10-11 创建：project-cpa-workbuddy-plugin-sse-and-ping-optimization-rules

- **创建原因**：用户要求「将本次的经验吸收到项目 skill 中。看看其他的插件根据当前的优化经验有没有需要优化的地方」。
- **来源**：2026-10-11 会话对 `qoderwork-provider`（0.9.28/0.9.29）、`qoder-ai-provider`（0.1.7/0.1.8）、`traework-provider`（0.2.6/0.2.8）三插件的完整排障、修复、发布与生产实测闭环：
  1. `qoderwork` / `qoder-ai` 0.9.28 & 0.1.7：定位并修复 `collectUpstreamStreamQoder` 遇到 `[DONE]` 误用 `continue` 死等 Keep-Alive、多种 `[DONE]` 形态漏检、换号 Context 丢失与 UTF-8 截断；
  2. `qoderwork` / `qoder-ai` 0.9.29 & 0.1.8：定位并移除 `handlePollLogin` 中基于 `hostAuthList()` `CreatedAt > startedAt` 判定成功的 `Panel-import` 假成功漏洞（`writeAuthFileDirect` 原子重命名刷新重置 `CreatedAt` 导致未点链接自动认证成功）；
  3. `traework` 0.2.6：定位并修复 `scanSSE` 收到 `event: done` 未跳出循环死等 Keep-Alive 的问题（引入 `errTraeStreamDone`）；
  4. `traework` 0.2.8：定位并修复面板「测试」按钮（`sendActivePingTrae`）使用非流式 `hostHTTPDo` 被迫等待思考模型（如 `glm-5.3-flash`）生成全部 264 个 `reasoning_content` token（耗时 5.6~8.1s）及未拦截 HTTP 200 `event: error` 的问题；改为流式首包即停（`verifyActivePingTraeStream`）+ `prewarmStreamHTTPClient` 后台预热 TLS Keep-Alive 连接 + 过滤内部非对话子代理占位项，生产同账号实测 `glm-5.3-flash` 从 `5.615s` 降至 `1.543s`、`deepseek-v4.1-flash` 降至 `943ms`、`qwen3.8-max` 降至 `1.388s`。
- **同域冗余扫描**：与 `project-cpa-workbuddy-plugin-trae-local-verify-rules`（只管本地解密 storage.json 直连验证脚本）、`project-cpa-workbuddy-plugin-release-rules`（只管 CI 发布与生产部署链路）职责正交，无逐字重复与门控冲突。

## 2026-10-11 补充：HTTP 200 首包业务报错帧假成功与全链路流式不缓冲规范

- **触发原因**：用户排查 New-API 接入 Qwen-3.8-Flash 慢卡死但 CPA 面板测试很快（219ms）、Token 用量插件显示响应不慢（334ms）的故障。
- **根因沉淀**：
  1. 阿里 Qoder 上游对受限/排队模型返回 HTTP 200 下的 `data: {"code":"403",... "10605"}`，`active_ping` 盲读 512 字节未验内容致 219ms 假成功，而真实推理因上游长连接挂起 60s 超时；
  2. 用量插件 `ttft_ns` 记录到首包 334ms，但实际 `failed: true`、`latency_ms: 60100` 且 `total_tokens: 0`；
  3. New-API 渠道测试强制 `stream: false`，不能用于流式验证；
  4. Nginx 必须显式配置 `proxy_buffering off; proxy_cache off;` 并通过 `docker exec nginx nginx -t && nginx -s reload` 重载。
- **落点**：更新 `project-cpa-workbuddy-plugin-sse-and-ping-optimization-rules/SKILL.md` 的核心规则二与核心规则三。

## 2026-10-12 补充：顶层 code 排队帧必须在解析器侧识别（数据面根因，非探活侧）

- **触发原因**：用户报告「还是不能从 new-api 请求 cpa 的 qwen-3.8-flash、qwen-3.8 模型，但是 cpa 本身测试是可以的」。上一轮（2026-10-11）已把该现象沉淀为「探活侧首包盲判」，但**数据面解析器侧的根因当时未收敛**——本轮补齐。
- **根因（数据面）**：`qoder-ai/stream.go` 的 `parseQoderSSEChunk` 只认 `[DONE]` / 嵌套 `body` / 扁平 `choices`·`id` / `error` 四类形态，顶层 `code` 帧落入兜底 `return "", false` 被 `continue` 跳过。上游对超上下文请求返回该帧后保持 Keep-Alive 约 60 秒，插件因此读不到终止信号而死等——**探活侧的「首包盲判」只是同一致命帧在另一条路径上的表现，解析器丢弃才是根因**。
- **生产实测判据（可复用）**：
  1. 同一模型名仅 prompt 长度不同，行为完全相反：67KB 通过（3.49s）、112KB **3/3 稳定挂死 60.5s**；
  2. 挂死响应体首行即 `data: {"code":"403","message":"{...10605...isQueued:true...retryAfterSeconds:30...}"}`，随后 3 条 `: keep-alive`，60s 后对端 `INTERNAL_ERROR`；
  3. 排队是**请求级 + 时段性**，非账号级：feed 全量 259 条中同账号 ok/bad 混合（`01a003c5` 25:39、`01a125f7` 18:21），成功请求 `input_tokens` 高达 289,424，97 条挂死集中在 23:36–00:13 两账号交替；
  4. `strings <so> | grep isQueued` 无命中 = 线上二进制确无该识别逻辑（排除部署漂移）。
- **修复落点**：`parseQoderSSEChunk` 增第 4 类形态；`sseErrorFrame` 增顶层 `code`+`message` 识别 + `unwrapNestedMessage` 逐层展开（保留首层业务码）；`pumpUpstreamStream` / `collectUpstreamStreamQoder` / `aggregateQoderSSE` 三路径循环内提前收尾；`sendActivePingQoder` 首帧内容校验。行为统一「立即报错」（`shouldRotateOnUpstreamErr` 对排队帧天然 false，不换号）。
- **方法论教训**：**「探活假成功」与「数据面挂死」是同一错误帧的两条路径，修一侧不等于修根因**。今后排查「面板测试通过但真实不可用」时，必须同时检查探活路径与数据面解析器对该帧的处理，并以「解析器是否识别该帧形态」为第一判据。
- **落点**：更新 `SKILL.md` 核心规则一（2.2 契约 1）新增顶层 code 帧识别条目；两插件版本 `qoder-ai 0.1.9` / `qoderwork 0.9.30`。
