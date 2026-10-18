# qwenwork 执行器漏接 oauth-model-alias 反解致别名请求稳定 400 根因诊断

- 日期：2026-10-18
- 状态：已修复（qwenwork-provider 0.2.3）
- 影响版本：qwenwork-provider 0.1.0 ~ 0.2.2（全生命周期）
- 发现渠道：用户经 new-api 渠道测试 `qwen-3.8-max` 失败，但 CPA 面板「测试」同模型成功

## 1. 现象

new-api 渠道测试与生产请求明细均显示 `qwenwork-provider` + `qwen3.8-max-preview` 失败：

| 来源 | 模型名 | 结果 | 耗时 | 输入 token |
|---|---|---|---|---|
| new-api 请求明细 | `qwen3.8-max-preview` | 失败 `HTTP 400` | 2.14s | 0 |
| new-api 渠道测试 | `qwen-3.8-max` | 失败 `bad response status code 500` | — | — |
| CPA 面板「测试」 | `qwen3.8-max-preview` | **成功** | 2.586s | — |

上游错误体：

```json
{"error":{"code":"invalid_request","message":"invalid request","param":"None","type":"invalid_request_error"}}
```

关键矛盾：**同一账号、同一上游模型，面板成功而 new-api 失败**。

## 2. 根因

### 2.1 直接原因：别名被原样透传给上游

生产 `oauth-model-alias` 配置（管理 API 实测）：

```json
qwenwork-provider: [
  {"name": "qwen3.8-max-preview", "alias": "qwen-3.8-max", "fork": true},
  {"name": "qwen3.8-max-preview", "alias": "qwen-3.8",     "fork": true},
  {"name": "qwen3.8-max-preview", "alias": "qwen3.8-max",  "fork": true}
]
```

宿主对 `oauth-model-alias` **只做路由匹配**：`buildExecutorRequest` 直接把 `req.Model` 原样填进 `ExecutorRequest.Model`（`internal/pluginhost/adapters.go:2018`），不做反解。

插件执行器两条路径此前只做 `stripProviderPrefix`：

```go
upstreamModel := cpaToUpstreamKey(stripProviderPrefix(req.Model))
```

而 `cpaToUpstreamKey` 对 QwenWork 只做 `TrimSpace`（模型 ID 与上游一致，不做重写）。于是 `qwen-3.8-max` 被原样发给上游，上游只认真实 ID `qwen3.8-max-preview` ⇒ `invalid_request`。

### 2.2 为什么面板测试成功

`handleTestActiveWithAuth` → `sendActivePingQoder` 构造最小请求体：

```go
body, _ := json.Marshal(map[string]any{
    "model":    cpaToUpstreamKey(chosenModel),
    "messages": []map[string]string{{"role": "user", "content": "hi"}},
    "stream":   true,
})
```

面板弹窗的模型列表来自 `authModelsForIndex`（上游动态发现），返回的是**真实模型 ID**，不经过宿主别名层 ⇒ 直接命中 `qwen3.8-max-preview` ⇒ 成功。

**两条路径的模型名来源不同，掩盖了执行器侧的缺陷。**

### 2.3 为什么代码里已有反解函数却没用上

`models.go` 早已实现 `resolveUpstreamModel(model, attributes)`（auth 属性 + 宿主别名缓存双通道）与 `cacheModelAliases(host)`，但 `grep` 显示它**只被测试调用**：

```
qwenwork/models.go:430: func resolveUpstreamModel(...)
qwenwork/models_config_test.go:253,258: 测试调用
```

对照 workbuddy 的正确写法（`workbuddy/main.go:690/840`）：

```go
upstreamModel := resolveUpstreamModel(req.Model, req.AuthAttributes)
```

结论：qwenwork 从 workbuddy 克隆骨架时**漏接了执行器这一环**，属于克隆遗留缺陷。

## 3. 复现与取证

### 3.1 对照实验（生产真实入口）

固定 `messages=[{role:user,content:hi}]`、`stream=false`，仅改变模型名：

| 模型名 | HTTP | 结论 |
|---|---|---|
| `qwen3.8-max-preview`（真实 ID） | 200 | 正常 |
| `qwen-3.8-max`（别名） | **500**（上游 400） | 失败 |
| `qwen-3.8`（别名） | 503 `auth_unavailable` | 失败 |
| `qwen3.8-max`（别名） | 503 `auth_unavailable` | 失败 |

### 3.2 排除账号与网络因素

用面板 `test-active` 直发两条模型（同一 auth_index `20ab89208f9ce928`）：

| 模型名 | 结果 |
|---|---|
| `qwen3.8-max-preview` | `{"ok":true,"elapsed":"1.467s"}` |
| `qwen-3.8-max` | `{"error":"...upstream 400...invalid_request..."}` |

同账号、同网络、同插件，**唯一变量是模型名** ⇒ 上游不接受别名，与账号健康度无关。

### 3.3 排除 reasoning_effort 干扰

初判曾怀疑 `reasoning_effort=xhigh`（失败行显示 xhigh、输入 0）。实测四档（无 / xhigh / high / max）全部 200，**排除**。`输入 0` 是上游在 token 计数前即拒绝的必然结果，属症状非原因。

## 4. 修复

> **两次误判更正（2026-10-18 14:30）**：
> - 0.2.3 补执行器反解 → 生产仍 400。
> - 0.2.4 改 `ExecutorModelScopeBoth` → 生产仍 400。
> - **真正根因在 §4.6**：宿主早已把 `ExecutorRequest.Model` 反解为真实 ID，但插件透传的是 `req.Payload`（客户端原文），其中 `model` 仍是别名，从未被改写。
>
> §4.1~§4.5 保留为排查过程记录，最终生效版本是 **0.2.5**。

### 4.1 新增统一解析入口

```go
// upstreamModelFor 把执行器收到的客户端模型名解析为上游真实模型 ID。
// 三步不可调换：先剥 provider 前缀，再反解 oauth-model-alias，最后做模型键归一。
func upstreamModelFor(model string, attrs map[string]string) string {
	return cpaToUpstreamKey(resolveUpstreamModel(stripProviderPrefix(model), attrs))
}
```

### 4.2 两条路径统一接线

| 路径 | 位置 | 改动 |
|---|---|---|
| 同步 | `handleExecExecute`（`main.go:638`） | `cpaToUpstreamKey(stripProviderPrefix(req.Model))` → `upstreamModelFor(req.Model, req.AuthAttributes)` |
| 流式 | `handleExecStream`（`main.go:900`） | 同上 |

### 4.3 别名来源双通道

1. **auth 属性**（优先级高）：`model_alias` / `model-alias` / `oauth-model-alias`，支持 JSON 数组、JSON 对象、逗号分隔 `alias=name` 三种形态。
2. **宿主级缓存**：`cacheModelAliases(host)` 在 `model.static` / `model.for_auth` 被调用时，从宿主传入的 `HostConfigSummary.OAuthModelAlias` 填充；插件声明了 `ModelProvider: true`，宿主在**注册期**即调用 `StaticModels`，故缓存必然已填充。

### 4.4 第二层根因（0.2.3 修复无效的原因）

上面 4.3 第 2 条的「故缓存必然已填充」是**未经实证的推断，被生产推翻**。

宿主 `executorScopeAllowsStaticModels`（`internal/pluginhost/adapters.go:60`）的实现：

```go
func executorScopeAllowsStaticModels(caps pluginapi.Capabilities) bool {
	if caps.Executor == nil {
		return true
	}
	scope := normalizedExecutorModelScope(caps)
	return scope == pluginapi.ExecutorModelScopeStatic || scope == pluginapi.ExecutorModelScopeBoth
}
```

而 qwenwork 声明的是 `ExecutorModelScopeOAuth` ⇒ 该谓词返回 false ⇒ 注册期**根本不会调用** `StaticModels` ⇒ `cacheModelAliases` 从未执行 ⇒ 别名缓存恒空 ⇒ `resolveUpstreamModel` 查不到映射，`upstreamModelFor` 原样返回别名。

**判定证据**：0.2.3 已热重载生效（`active_version=0.2.3`、二进制含 `upstreamModelFor` 符号），但别名请求仍 400——证明接线本身没问题，是输入（缓存）为空。

**修复**：改为 `ExecutorModelScopeBoth`，同时满足 static（注册期填充缓存）与 oauth（账号级模型发现）。补 `TestRegistration_ExecutorModelScopeAllowsStaticModels` 锁定，变异回 `OAuth` 时精确 FAIL。

### 4.5 两层根因的关系

| 层 | 位置 | 症状 | 修复版本 |
|---|---|---|---|
| 第一层 | 执行器只剥前缀、不透传反解 | 别名原样发给上游 | 0.2.3 |
| 第二层 | 模型作用域声明为 OAuth，宿主跳过 `StaticModels` | 别名缓存恒空，第一层修复拿不到数据 | 0.2.4 |

**只修第一层无效**——这是本轮最重要的教训：修了「代码路径」，却没验证「数据来源」在生产是否真的可达。

### 4.6 真正根因：透传请求体从未改写 model（0.2.5）

0.2.4 上线后仍 400。生产日志给出决定性一行：

```
provider=qwenwork-provider model=qwen3.8-max-preview alias=qwen-3.8-max
```

- `model=qwen3.8-max-preview`：**宿主传给插件的 `ExecutorRequest.Model` 已经是真实 ID**——宿主确实做了反解，§2.1「宿主不做反解」的判断是错的。
- `alias=qwen-3.8-max`：用量上报里记录的仍是客户端别名。

由此推出：插件发给上游的模型名来自 `req.Payload`，而不是 `req.Model`：

```go
bodyRaw := req.Payload          // 客户端原始请求体，model 字段仍是别名
body := prepareUpstreamBody(bodyRaw)   // 只强制 stream、放宽 max_tokens，从不改 model
```

`upstreamModel` 只被用于 `publishUsage` 统计，**从未回写进 body**。

**修复**：`prepareUpstreamBody` 增加 `upstreamModel` 参数，在原有改写之外把 `obj["model"]` 设为真实 ID；空值时保留原值。

```go
func prepareUpstreamBody(bodyRaw []byte, upstreamModel string) []byte {
	...
	obj["stream"] = true
	if m := strings.TrimSpace(upstreamModel); m != "" {
		obj["model"] = m
	}
	relaxMaxTokensInPlace(obj)
	...
}
```

**为什么 0.2.3/0.2.4 的改动仍保留**：执行器反解（兼容 auth 属性别名通道）与 `ExecutorModelScopeBoth`（让模型回调能填充别名缓存）本身正确，只是都不触及真正的失效路径。它们让 `upstreamModel` 的值更可靠，但 `upstreamModel` 此前根本没被用上。

### 4.7 第三层：MaaS session_id 被塞入非 UUID 会话键（0.2.6）

0.2.5 修好 `body.model` 后，执行器路径改报：

```
maas: maas mint: http 422
{"detail":[{"got":"","loc":["body","session_id"],"msg":"value is not a valid UUID"}]}
```

- 面板测试 `handleTestActiveWithAuth` 调用 `ensureMaasSession(sa, "")`——空 sessionID，不发 `session_id` 字段 ⇒ 成功。
- 执行器路径调用 `ensureMaasSession(sa, sessionKey)`，而 `sessionKey` 是**调度侧会话键**（`x-client-request-id:abc` / `derived:conv-123` / `execution:task-9`），被原样塞进 `session_id` ⇒ 上游 UUID 校验失败。

**这是同一类「两条路径输入不同」缺陷的第三次出现**：模型名（§4.6）与 session_id 都是「面板传空/传真实值，网关传派生键」。

**修复**：新增 `maasSessionID`，按会话键做 SHA-256 确定性派生并格式化为 v4 UUID（版本位 `4`、变体位 `8/9/a/b`）；同一会话恒得同一 UUID（保持会话粘性），空键返回空串交由上游分配。`mintPayload` 独立成函数，让测试能断言「实际发往上游的 payload」。

### 4.8 三层根因总览

| 层 | 缺陷 | 症状 | 版本 |
|---|---|---|---|
| 1 | 执行器不做别名反解 | 别名原样透传 | 0.2.3 |
| 2 | `ExecutorModelScope=OAuth` 致别名缓存恒空 | 反解拿不到数据 | 0.2.4 |
| 3 | 透传 body 未改写 `model` | 别名随 Payload 发出 | 0.2.5 |
| 4 | 调度会话键被当作 MaaS `session_id` | 422 not a valid UUID | 0.2.6 |

**关键教训**：四层缺陷**串在同一条请求路径上**，每修好一层才暴露下一层。只做单测或只看「真实 ID 能通」都会误判为已修复——必须用与故障完全同形的请求（别名 + 带会话头）做端到端验收。

## 5. 验证

### 5.4 生产端到端验收（0.2.6，决定性证据）

热重载 `active_version=0.2.6 retired_version=0.2.5` 后，用生产真实入口 `https://cpa.luode.vip/v1/chat/completions` 逐条验证：

| 模型名 | 修复前 | 修复后 |
|---|---|---|
| `qwen3.8-max-preview`（真实 ID） | 200 | **200** |
| `qwen-3.8-max`（别名） | 500（上游 400） | **200** |
| `qwen-3.8`（别名） | 503 auth_unavailable | **200** |
| `qwen3.8-max`（别名） | 503 auth_unavailable | **200** |

流式路径（new-api 实际形态，带 `X-Session-ID` 头）：

| 模型名 | 结果 |
|---|---|
| `qwen-3.8-max` | 200，11 个 SSE 分片，`model` 字段为 `qwen3.8-max-preview`，正常 `[DONE]` |
| `qwen3.8-max-preview` | 200，14 个 SSE 分片，正常 `[DONE]` |

### 5.5 安装时的额外坑（可复用）

`plugin-store install` **不带 `source` 参数会失败**：

```
POST /v0/management/plugin-store/qwenwork-provider/install?version=0.2.6
→ 502 {"error":"plugin_manifest_invalid","message":"direct plugin version \"0.2.6\" not found"}
```

带上 `&source=source-636fa01121af` 后成功。原因：无 `source` 时宿主走 `fetchSourcedPlugins` 合并多源结果，解析出的 plugin 对象版本与请求版本对不上；显式指定 source 走 `FetchRegistry` 单源直取。

### 5.6 单测

新增 `qwenwork/alias_resolution_test.go`（4 个用例、9 个子用例）：

- `TestResolveUpstreamModel_HostAliasCache`：宿主缓存通道、大小写不敏感、真实 ID 原样通过、未知模型不改写
- `TestResolveUpstreamModel_AuthAttributeWins`：auth 属性优先级高于宿主缓存
- `TestCacheModelAliases_ReadsHostSummary`：空别名 / 同名 / 空 name 必须跳过，只保留 1 条有效项
- `TestCacheModelAliases_CaseInsensitiveChannel`：channel 名大小写不敏感
- `TestUpstreamModelFor_ExecutorPath`：**直接覆盖执行器入口**，9 个子用例含生产三条别名、`qwenwork/` 前缀形态、大小写形态、auth 属性、未知模型

### 5.7 变异反证（关键）

第一次变异（把两条路径的 `upstreamModelFor(...)` 还原为旧写法）后测试**仍全绿**——因为测试只覆盖 `resolveUpstreamModel` 本身，没有覆盖接线。据此把接线抽成 `upstreamModelFor` 并补 `TestUpstreamModelFor_ExecutorPath`。

第二次变异（把 `upstreamModelFor` 函数体退回 `cpaToUpstreamKey(stripProviderPrefix(model))`）：

```
--- FAIL: TestUpstreamModelFor_ExecutorPath/bare_alias_qwen-3.8-max
--- FAIL: TestUpstreamModelFor_ExecutorPath/bare_alias_qwen-3.8
--- FAIL: TestUpstreamModelFor_ExecutorPath/bare_alias_qwen3.8-max
--- FAIL: TestUpstreamModelFor_ExecutorPath/prefixed_alias
--- FAIL: TestUpstreamModelFor_ExecutorPath/uppercase_alias
--- FAIL: TestUpstreamModelFor_ExecutorPath/auth_attribute_alias
```

6 个子用例精确 FAIL；还原后 cgo-shim build/vet/test 全绿。

### 5.8 门禁

- `python scripts/cgo-shim-build.py qwenwork` → build / vet / test 全绿
- `gofmt -l`（CRLF 归一后）→ 空

## 6. 教训

1. **「面板成功、网关失败」必须作为差异信号**：两条路径的模型名来源不同（面板走动态发现真实 ID，网关走宿主别名透传），差异本身就是定位线索。
2. **克隆骨架时容易漏接的环节**：`resolveUpstreamModel` 这类「已实现但只被测试调用」的函数，是克隆遗留的高危信号。判定方法：`grep -n "funcName" *.go` 后看调用点是否只在 `_test.go`。
3. **变异反证必须能覆盖接线**：只测纯函数不测接线时，还原接线后测试仍绿，属于假阳性保护。抽取可测的接线函数（`upstreamModelFor`）才能让变异精确命中。
4. **`输入 0` 是上游在计数前拒绝的症状**，不能据此推断请求体内容问题；`reasoning_effort` 初判即被四档实测排除。
5. **「代码路径修好」≠「功能生效」**：必须验证数据来源在生产可达。本轮 0.2.3 接线正确、符号已进二进制、日志显示热重载成功，但缓存为空导致修复无效——**只有生产端到端请求能证伪**。修复后必须用与故障完全同形的请求（此处是别名）验收，不能只看「真实 ID 能通」。
6. **能力声明会改变宿主行为**：`ExecutorModelScope` 不只是元数据，它决定宿主是否调用 `StaticModels`，进而决定插件能否拿到 `HostConfigSummary`（别名表、排除列表都从这里来）。改作用域前要读宿主的谓词实现。

## 7. 关联

- 修复提交：`f9e094d`（0.2.3，含并行会话的 0.2.1/0.2.2 断流修复）
- 相关缺陷：`doc/4-bugs/2026-10-18_030000_qwen38flash已泄漏分片断流致new-api零计费复发根因诊断.md`
