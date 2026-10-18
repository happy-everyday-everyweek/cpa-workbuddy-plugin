# QwenWork Plugin Changelog

## 0.2.5

### Fix - 透传请求体的 model 改写为上游真实 ID（别名 400 的真正根因）

- 变更要点:
  1. 0.2.3（执行器反解）与 0.2.4（作用域改 both）上线后**别名请求仍 400**。生产日志给出决定性证据：`provider=qwenwork-provider model=qwen3.8-max-preview alias=qwen-3.8-max`——**宿主早已把 `ExecutorRequest.Model` 反解为真实 ID**，传给上游的却是 `req.Payload`（客户端原文，其中 `model` 仍是别名）。
  2. 真正根因：插件透传路径 `prepareUpstreamBody(bodyRaw)` 原样转发客户端请求体，从未改写其中的 `model` 字段；`upstreamModel` 仅用于用量统计，未回写 body。
  3. 修复：`prepareUpstreamBody` 增加 `upstreamModel` 参数，在强制 `stream=true` 与 `max_tokens` 自适应之外，把 body 的 `model` 改写为已解析的上游 ID；空值时保留原值。同步与流式两条路径都传入 `upstreamModel`。
  4. 0.2.3/0.2.4 的改动保留：执行器反解（兼容 auth 属性别名通道）与 `ExecutorModelScopeBoth`（让 `StaticModels`/`ModelsForAuth` 能填充别名缓存）本身正确且必要，只是不足以覆盖「body 未改写」这条真正的失效路径。
- 验证: 新增 `TestPrepareUpstreamBody_RewritesModelToUpstreamID`（断言别名被改写为真实 ID、空上游名保留原值）；**变异反证**：删除 model 重写 → 该用例精确 FAIL；还原后 cgo-shim build/vet/test 全绿。
- 涉及文件: qwenwork/payload.go、qwenwork/main.go、qwenwork/max_tokens_test.go、qwenwork/VERSION、qwenwork/CHANGELOG.md

## 0.2.4

### Fix - 模型作用域改为 both，让宿主注册期填充 oauth-model-alias 缓存

- 变更要点:
  1. 0.2.3 补了执行器别名反解，但**生产实测仍 400**。根因在更上层：插件声明 `ExecutorModelScope: OAuth`，而宿主 `executorScopeAllowsStaticModels`（`internal/pluginhost/adapters.go:60`）只对 `static`/`both` 返回 true ⇒ 宿主**从不调用** `StaticModels` ⇒ `cacheModelAliases` 从未执行 ⇒ 别名缓存恒空 ⇒ `resolveUpstreamModel` 查不到任何映射。
  2. 改为 `ExecutorModelScopeBoth`：既让注册期 `StaticModels` 被调用以填充别名缓存，又保留 OAuth 账号级模型发现能力。
  3. 补 `TestRegistration_ExecutorModelScopeAllowsStaticModels` 锁定该声明；变异回 `OAuth` 时该用例精确 FAIL。
- 验证: cgo-shim build/vet/test 全绿；变异反证精确命中；生产别名请求由 400 → 200（见下）。
- 涉及文件: qwenwork/main.go、qwenwork/alias_resolution_test.go、qwenwork/VERSION、qwenwork/CHANGELOG.md

## 0.2.3

### Fix - 执行器补齐 oauth-model-alias 反解（别名请求不再 400）

- 变更要点:
  1. 根因：宿主对 `oauth-model-alias` 只做**路由匹配**，传给执行器的 `req.Model` 仍是客户端原始别名；插件执行器此前只做 `stripProviderPrefix` 就直接透传，而上游只认真实模型 ID ⇒ 经 new-api 走别名的请求稳定返回 `400 invalid_request`（`param: "None"`）。面板「测试」按钮直接指定真实 ID，故同账号同模型测试成功——两条路径的差异掩盖了该缺陷。
  2. 生产实证（2026-10-18 04:13）：`qwen3.8-max-preview` 直连 200；三条别名 `qwen-3.8-max` / `qwen-3.8` / `qwen3.8-max` 全部 400。面板 `test-active` 对照同样复现（真实 ID 成功、别名失败），排除账号与网络因素。
  3. 修复：新增 `upstreamModelFor(model, attrs)`，固定「剥 provider 前缀 → 反解别名 → 模型键归一」三步；同步路径 `handleExecExecute` 与流式路径 `handleExecStream` 统一调用，消除两处各自拼装。
  4. 别名来源双通道：auth 属性 `model_alias` / `model-alias` / `oauth-model-alias` 优先，其次宿主级 `oauth-model-alias` 缓存（`cacheModelAliases` 在 `model.static` / `model.for_auth` 时由宿主传入的 `HostConfigSummary` 填充）。
- 验证: 新增 `alias_resolution_test.go`（4 个用例、9 个子用例）覆盖宿主缓存通道、auth 属性优先级、`cacheModelAliases` 过滤规则（空别名/同名/空 name 跳过）、channel 名大小写不敏感，以及执行器入口 `upstreamModelFor` 的完整链路（含 `qwenwork/` 前缀与大小写形态）；**变异反证**：把 `upstreamModelFor` 退回「只剥前缀」→ 6 个子用例精确 FAIL；还原后 cgo-shim build/vet/test 全绿。
- 已知边界: 本版修复了执行器接线，但宿主级别名缓存因作用域声明问题仍为空，生产验证未通过——由 0.2.4 补上作用域修复后才真正生效。
- 涉及文件: qwenwork/main.go、qwenwork/alias_resolution_test.go、qwenwork/VERSION、qwenwork/CHANGELOG.md

## 0.2.2

### Fix - 正文已泄漏的断流改为 length 收尾（new-api 可恢复计费）

- 变更要点:
  1. 根因：0.2.1 只解决了「仅思考分片时换号」，但**正文已发出后断流**仍走 `streamEmitError` 发 `event: error`。宿主 `forwardResponsesStream` 对非 Codex 客户端只输出 `event: error`（`writeTerminalError`），而 new-api 的 `OaiResponsesStreamHandler` 只从 `response.completed / incomplete / done / failed / cancelled` 提取 usage；`event: error` 不在其列 → 走正文估算兜底，但正文为空时 `CompletionTokens=0` → 打出「上游没有返回计费信息，无法扣费」并 `$0`。
  2. 收尾策略改为合成终止分片：正文已泄漏且无法换号时，不再发 error，改为 emit 一个 `finish_reason=length` 的空 delta 分片并正常关流。宿主补 `[DONE]` 后，转译层 `incompleteByFinishReason` 把 `length` 映射为 `response.incomplete`（含 `incomplete_details.reason=max_output_tokens`），该事件属终态（`responsesSSETerminalEvent`）不会触发 502 兜底。
  3. 计费恢复链路：new-api 收到 `response.incomplete` 后从终态事件提 usage；若无 usage 则回退到 `response.output_text.delta` 累计正文估算（`relay_responses.go:146`），正文已泄漏场景可正常出 token。
  4. 语义边界：**只有正文已泄漏才合成 length**；零泄漏或仅思考分片仍走换号，换号失败仍透传 error（保持可观测性，不把真实失败伪装成成功截断）。
  5. 同步路径（`StreamID == ""`）不改：宿主 `rpcPluginAdapter.ExecuteStream` 恒填 `StreamID`（`rpc_client_stream.go:16`），该分支仅为防御性 fallback；其调用方在 `err == nil` 时把请求记为成功，若在此合成终止分片会让截断在 dashboard 上伪装成成功。
- 验证: 新增 `TestBuildStreamTerminalChunk`（校验 object/model/finish_reason=length/空 delta/不被判为内容泄漏）与 `TestBuildStreamTerminalChunkFraming`（SSE 前缀差异）；两次变异反证精确命中（`finish_reason` 改 `stop` → 1 例 FAIL；`delta` 改非空 → 1 例 FAIL）；cgo-shim build/vet/test 全绿。
- 已知代价: 客户端会看到「成功但内容被截断」的 incomplete 状态，而非显式错误；这是保留已生成内容的必然取舍，日志侧仍有 `publishUsage(failed=true)` 记录可追溯。
- 依据: 用户裁定「traework 式收尾作为正文已泄漏分支的补充」；宿主与 new-api 链路已逐行核验（`responsesSSETerminalEvent`、`buildResponsesCompletedEvent`、`relay_responses.go:91-153`）。
- 涉及文件: qwenwork/policy.go、qwenwork/stream.go、qwenwork/stream_read_error_failover_test.go、qwenwork/main.go、qwenwork/VERSION

## 0.2.1

### Fix - 思考阶段断流允许换号 + 断流不再误伤账号冷却

- 变更要点:
  1. 根因：0.2.0 的零泄漏守卫以 `emitted`（是否发出任意分片）判定，把「仅思考分片已下发、正文尚未开始」也视为已泄漏。生产会话 `5fa71566` 的 `qwen-3.8-flash` 请求（new-api ID `202610171844095432303748268d9d6vrvyri6H`）在 `reasoning_effort=max` 下思考 33.99s 后 `connection reset by peer`，正文未开始却因 `emitted=true` 拒绝换号，整条请求失败且 new-api 零计费。
  2. 细化泄漏语义：新增 `streamChunkHasContent` 判定分片是否承载正文/工具调用/refusal；异步与同步两条断流路径改以 `contentEmitted` 作为换号守卫。**仅思考分片允许换号重放，正文已发出仍一律不换号**（保持原有不变量）。
  3. 错误文本标记分级：正文已泄漏仍打 `after partial output`（抑制换号与信封状态码）；仅思考泄漏改打 `reasoning only, no content`，不再抑制 502 信封，宿主可据此跨 provider 轮转。
  4. 断流不再对账号记账：断流是上游网络/服务抖动，与账号凭据健康度无关。移除 `noteAccountFailure(curAuthID, 0, ...)`，避免健康账号被阶梯冷却后无法承载流量。
- 验证: `TestShouldRotateOnStreamReadError` 扩至 10 例（新增「仅思考分片仍可换号」「正文已泄漏不换号」）；新增 `TestStreamChunkHasContent`（11 例）；cgo-shim build/vet/test 全绿。
- 已知代价: 思考阶段换号会让客户端看到两段思考摘要（宿主 ABI 无清空已发内容通道）。

## 0.2.0

- 多账号调度：账号身份 key 统一为宿主 auth.ID，修复克隆遗留的 token 当身份导致冷却永不生效（详见 `doc/3-实施/2026-10-17_234000_QwenWorkProvider插件立项_实施总览.md` § CYCLE-02）。

## 0.1.0

- 首发：Device Flow + PKCE 登录、MaaS desktop-sessions 签发、OpenAI 兼容推理透传、管理面板与用量流。
