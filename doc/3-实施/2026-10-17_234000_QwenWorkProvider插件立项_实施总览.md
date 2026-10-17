# QwenWork Provider 插件立项实施总览

- 文档时间：2026-10-17 23:40
- 目标仓库：`luode0320/cpa-workbuddy-plugin`（物理目录 `F:\cpa-plugin`）
- 参照基线：`workbuddy/`（46 个非测试文件 / 21043 行，功能最完整）
- 上游契约：`doc/3-实施/` 同级；实测记录见项目记忆 `reference-qwenwork-upstream-contract.md`

---

## 1. 当前计划最终方案说明

以 `workbuddy` 为功能蓝本、以 `qoderwork` 为代码骨架，新建 `qwenwork-provider`：

- **骨架**取 `qoderwork`（同源技术底座、同样 c-shared 形态、同样 Device Flow 登录形态），而不是 `workbuddy`——workbuddy 的浏览器 OAuth 与 QwenWork 的 Device Flow 不兼容。
- **功能面**对齐 `workbuddy`：多账号池、调度换号、冷却阶梯、会话粘性、动态模型、用量统计、管理面板、导入导出、保活、探活。
- **裁剪** workbuddy 的 CN/Global 双区域、签到、成长中心、试用包——QwenWork 无对应上游能力（签到端点实测 404，见 §4）。

推荐理由：这样既避免从零实现 2 万行，又不会把 workbuddy 的区域化逻辑硬塞进单区域产品。

---

## 2. 问题与目标

### 2.1 问题

CPA 插件生态无 QwenWork 覆盖（`CLIProxyAPI-Plugins-Store` 122 条 registry 无相关条目），但 QwenWork 已具备完整的可接入上游：Device Flow 登录、MaaS 会话签发、OpenAI 兼容推理。缺口真实存在且技术路径已实测打通。

### 2.2 目标

交付一个可发布的 `qwenwork-provider`，达到 workbuddy 同级运维能力：多账号、自动换号、面板可视、用量可统计、可热重载部署。

### 2.3 本轮范围

- 新建 `qwenwork/` 目录与全部 Go 源码、测试、`panel.html`
- 接入 CI（`.github/workflows/build.yml` 8 处）与 `registry.json`
- 本地 `cgo-shim` 全绿验证

### 2.4 非范围

- 不改动 `qoderwork` / `workbuddy` 等既有插件
- 不做 Anthropic messages 协议输出（上游支持，但首版只做 OpenAI，见 §5.4）
- 不做签到 / 成长中心 / 试用包
- 不做企业版（`is_personal_version=false`）专属额度模型

### 2.5 当前优先闭环

**CYCLE-01：单账号从登录到推理全链路跑通**，其余能力在后续周期叠加。

### 2.6 关键假设与待确认点

| 编号 | 假设 | 状态 |
|---|---|---|
| A1 | MaaS key 2 小时 TTL，需在过期前轮换 | 已实测（`ttl_ms=7200000`） |
| A2 | 宿主 token 7 天有效，refresh 可续期 | 已实测（200 返回新 device_token） |
| A3 | 推理只需 Bearer，不需要 Cosy 签名 | 已实测（200） |
| A4 | `X-QwenWork-*` 头非必需 | 已实测（不带也 200） |
| A5 | 模型目录来自 `/api/desktop/chat-modes` | 已实测（3 档位） |
| A6 | 签到能力当前账号不可用 | 已实测（404），**需换账号复验** |
| A7 | 多账号并发时 MaaS 签发是否限流 | **未验证**，CYCLE-03 前需补 |

---

## 3. 上游能力事实（实测）

### 3.1 三条链路

```
① 登录    浏览器 → gateway/device/selectAccounts → qwenwork.cn/oauth2/auth
          → 轮询 gateway/api/v1/deviceToken/poll → 宿主 token(7d) + refresh
② 签发    POST gateway/api/desktop-sessions (Bearer 宿主 token)
          → 201 + sk-mm-v1.* (TTL 2h) + base_url
③ 推理    POST {base_url}/v1/chat/completions (Bearer sk-mm-v1.*)
```

### 3.2 关键端点

| 用途 | 方法 | 路径 | 鉴权 |
|---|---|---|---|
| 授权页 | GET | `gateway/device/selectAccounts` | 无 |
| 轮询登录 | GET | `gateway/api/v1/deviceToken/poll` | 无 |
| 刷新宿主 token | POST | `gateway/api/v1/deviceToken/refresh` | refresh_token |
| 签发 MaaS key | POST | `gateway/api/desktop-sessions` | 宿主 Bearer |
| 轮换 MaaS key | POST | `{base_url}/v1/access-tokens/exchange` | MaaS Bearer |
| 模型目录 | GET | `gateway/api/desktop/chat-modes` | 宿主 Bearer |
| 账号额度 | GET | `gateway/api/v1/adapter/user/account-context` | 宿主 Bearer |
| 钱包余额 | GET | `gateway/api/v1/adapter/user/wallets` | 宿主 Bearer |
| 推理 | POST | `{base_url}/v1/chat/completions` | MaaS Bearer |

### 3.3 额度模型（实测值）

```json
quota: { "total": null, "used": null, "remaining": 2099.8385, "exceeded": false }
daily_credits:    { "total_balance": 99.8385 }
monthly_credits:  { "total_balance": 0 }
longterm_credits: { "total_balance": 2000 }
```

注意：`total`/`used` 可能为 null，**只有 `remaining` 与钱包余额可靠**。

### 3.4 模型目录（实测）

| key | format | harness | is_default | max_input |
|---|---|---|---|---|
| `flash` | openai/chat-completions | oc | true | 1,000,000 |
| `pro` | openai/chat-completions | oc | false | 1,000,000 |
| `qwen3.8-max-preview` | openai/chat-completions | oc | false | 1,000,000 |

`subtask_routing` 内含 `lite` 等内部档位（`reasoning_effort=none`、`max_output_tokens=128`），不对外暴露。

---

## 4. workbuddy 功能对照矩阵

| # | workbuddy 能力 | 源文件 | qwenwork 处置 | 依据 |
|---|---|---|---|---|
| 1 | C ABI 入口 / RPC 分派 | `main.go:244` | **保留**（改名） | 同形态 |
| 2 | 能力声明 `wbRegistration` | `main.go:340` | **保留**（改名） | 同形态 |
| 3 | 浏览器 OAuth 登录 | `oauth.go:68` | **替换**为 Device Flow | 上游不同 |
| 4 | PAT 登录 | — | **不适用** | QwenWork 无 PAT |
| 5 | token 刷新 | `oauth.go:172` | **替换**为 `deviceToken/refresh` | 上游不同 |
| 6 | 保活 4 小时 | `keepalive.go:37` | **保留**（改端点） | 宿主 token 7d，4h 保活合理 |
| 7 | 账号文件命名 `workbuddy-<uid>.json` | `authfile.go:34` | **保留**改前缀 `qwenwork-` | 同构 |
| 8 | 直写物理文件绕过 host.auth.save | `authfile.go:240` | **保留** | 同一宿主缺陷 |
| 9 | 账号状态机（耗尽禁用/删除） | `lifecycle.go:416` | **改造**：单区域，只禁用不删除 | QwenWork 无 Global |
| 10 | 调度三模式 off/credits/session | `scheduler.go:78` | **保留** | 通用 |
| 11 | 冷却阶梯 30s×N 封顶 10min | `accountFailover.go:28` | **保留** | 通用 |
| 12 | 同请求换号预算 0-10 | `retry_config.go:28` | **保留** | 通用 |
| 13 | 会话粘性 1h | `session_auth.go:36` | **保留** | 通用 |
| 14 | 请求改写（stream 强制、模板词清洗） | `payload.go:20` | **裁剪**：仅保留 stream/max_tokens | QwenWork 无 Claude 模板限制 |
| 15 | SSE 泵 + 终止契约 | `stream.go:99` | **保留** | 通用 |
| 16 | 动态模型发现 | `models.go:213` | **替换**为 `chat-modes` | 上游不同 |
| 17 | 官方规格表 + max_tokens 自适应 | `model_specs.go:30` | **保留**（填 QwenWork 规格） | 通用 |
| 18 | 用量统计 + NDJSON feed | `usage.go:81` / `usage_feed.go:157` | **保留** | 通用 |
| 19 | 额度查询 | `billing.go:269` | **替换**为 `account-context` + `wallets` | 上游不同 |
| 20 | 管理面板 16 路由 | `management.go:146` | **保留 14 条**（删签到/成长/试用/领取 Pro） | 见 §5.5 |
| 21 | 签到 | `checkin.go:91` | **删除** | 实测 404 |
| 22 | 成长中心 7 段 | `growth.go:642` | **裁剪** | 无上游能力 |
| 23 | 试用包 | `credits_handler.go:301` | **裁剪** | 无上游能力 |
| 24 | 探活 4h 节流 | `active_ping.go:191` | **保留** | 通用 |
| 25 | 异常键清理 | `anomaly_purge.go:56` | **保留** | 通用 |
| 26 | 测试失败标签 | `test_failed_tag.go:128` | **保留** | 通用 |
| 27 | CN/Global 双区域 | `main.go` 多处 | **裁剪** | 单区域产品 |
| 28 | 导入/导出 | `credits_handler.go:18/109` | **保留**（仅备份恢复，无 PAT 导入） | 通用 |

**统计**：保留 17 项、替换 4 项、改造 1 项、裁剪 6 项、删除 1 项。

### 4.1 与初始计划的偏差（2026-10-18 实现后回填）

| 计划项 | 实际落点 | 原因 |
|---|---|---|
| 保留 `baseprompt.json` 模板链路 | **删除** `baseprompt.json`、`encoding.go`、`buildQoderBody`、`alignToolsWithClient` | QwenWork 走 OpenAI 兼容协议，请求体整体透传；Qoder 的 agent_chat_generation 模板与自定义 base64 编码在上游不存在对应端点 |
| `payload.go` 裁剪为 stream/max_tokens | 新建 `payload.go`：`prepareUpstreamBody` 强制 `stream=true` + `relaxMaxTokensInPlace` | 透传路径仍需兜底这两项，否则客户端 `stream=false` 会打穿 SSE 泵 |
| 模型映射沿用 qoderwork | `cpaToUpstreamKey` 改为 trim 直通，`defaultModelAliasMap` 清空 | 上游模型 ID 与客户端一致，`qmodel_*` 重写会把合法 ID 改坏 |
| 管理路由保留 12 条 | 实际 14 条（含 `/import-cred`、`/clear-cooldown`） | 备份恢复与手动解冷是面板必需能力 |
| 签到调度循环 | 新建 `scheduler_loop.go`，只驱动保活 + 生命周期对账 | 原 `schedulerLoop` 随 `checkin.go` 一并删除，保活不能失去定时器 |

---

## 5. 关键设计决策

### 5.1 为什么骨架用 qoderwork 而不是 workbuddy

| 维度 | qoderwork | workbuddy |
|---|---|---|
| 登录形态 | Device Flow + PKCE | 浏览器 OAuth + state |
| 与 QwenWork 匹配度 | **一致** | 不一致 |
| 代码量 | 17147 行 | 21043 行 |
| 已有 MaaS 类逻辑 | 无 | 无 |

Device Flow 是 QwenWork 唯一登录方式，用 qoderwork 骨架可省去重写登录态机；workbuddy 的 OAuth state 机、cookie jar 全部要删。

### 5.2 MaaS key 生命周期

```
宿主 token (7d)
   └─ POST /api/desktop-sessions ─→ MaaS key (2h)
          └─ POST /v1/access-tokens/exchange ─→ MaaS key (2h, 续期)
```

策略：内存缓存 + 到期前 30 分钟主动轮换；轮换失败按账号冷却处理，不回退宿主 token。

### 5.3 额度与禁用阈值

`quota.remaining` 与钱包余额双口径。禁用阈值默认 `min_routable_credits = 20`（沿用 workbuddy 语义），比较对象是 `daily + monthly + longterm` 合计。

### 5.4 首版只做 OpenAI 协议

上游两种协议都支持（实测），但 CPA 宿主对 Anthropic 输出有额外转换成本。首版统一 OpenAI，Anthropic 作为 CYCLE-06 可选增强。

### 5.5 管理路由（保留 14 条）

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/accounts` | 账号列表 + 额度 + 计划 |
| POST | `/refresh` | 强制刷新额度缓存 |
| GET | `/refresh/status` | 异步刷新进度 |
| GET | `/credits` | 实时额度（单号或全量） |
| POST | `/import-cred` | 从备份恢复凭据（无上游交换） |
| GET | `/export` | 导出全部凭据 |
| POST | `/select` | 切换当前账号 |
| POST | `/delete` | 删除账号 |
| POST | `/keepalive` | 手动保活 |
| GET | `/keepalive/status` | 保活状态 |
| POST | `/clear-cooldown` | 手动解除日限冷却 |
| GET | `/models` | 模型列表（面板用） |
| POST | `/test-active` | 单账号探活推理 |

删除的 5 条：`/checkin`、`/checkin/config`、`/growth`、`/trial`、`/import`（PAT 导入）。
`/claim-pro` 一并删除：QwenWork 无 Pro 升级包领取能力。

---

## 6. 现状与落点

### 6.1 目标目录树

```
qwenwork/
├── main.go              C ABI 入口 + RPC 分派 + 能力声明
├── authfile.go          物理 auth 文件命名/读写/删除
├── host_auth.go         host.auth.* RPC 封装
├── host_bridge.go       host.http.do / do_stream
├── oauth.go             Device Flow 登录 + 轮询 + 刷新
├── maas.go              【新增】desktop-sessions 签发 + 2h 轮换
├── lifecycle.go         账号状态机
├── policy.go            纯决策逻辑
├── scheduler.go         选号
├── accountFailover.go   冷却阶梯
├── failover_retry.go    换号重试
├── retry_config.go      重试配置
├── session_auth.go      会话粘性
├── watchdog.go          定时刷新 + 探活
├── keepalive.go         保活
├── scheduler_loop.go    【新增】定时循环（保活 + 生命周期对账）
├── body.go              模型名透传 + 请求视图
├── payload.go           请求改写（强制 stream + max_tokens 自适应）
├── stream.go            SSE 泵 + 聚合
├── models.go            模型发现（chat-modes）
├── model_specs.go       官方规格表
├── billing.go           额度查询（account-context + wallets）
├── credits_handler.go   面板数据接口（额度/导入/选择/删除）
├── usage.go             用量回调
├── usage_config.go      配置解析
├── usage_feed.go        NDJSON feed
├── counter.go           计数
├── cache.go             缓存
├── active_ping.go       探活
├── anomaly_purge.go     异常清理
├── test_failed_tag.go   测试失败标签
├── management.go        管理路由注册
├── panel.go             面板数据构建
├── panel.html           单页面板
├── redact.go            脱敏
└── *_test.go            测试
```

### 6.2 代码落点要点

- 所有 `qoderwork` 字面量改为 `qwenwork`（107 处，集中在 authfile/main/lifecycle）
- `providerName = "qwenwork-provider"`，`authFilePrefix = "qwenwork-"`
- 管理路由前缀 `/v0/management/plugins/qwenwork-provider`
- `panel.html` 的 `const API` 必须同步

---

## 7. 实施周期

### CYCLE-01：单账号全链路（优先闭环）

**目标**：登录 → 签发 → 推理 → 流式返回，端到端跑通。

最小任务：

| # | 任务 | 文件 | 验证 |
|---|---|---|---|
| 1.1 | 骨架克隆与改名 | 全部 | `cgo-shim-build.py qwenwork` 编译通过 |
| 1.2 | Device Flow 登录替换 | `oauth.go` | 真实账号登录拿到 token |
| 1.3 | MaaS 签发层 | `maas.go`（新） | `desktop-sessions` 返回 sk-mm-v1 |
| 1.4 | 推理转发 | `payload.go` / `stream.go` | 非流式 + 流式各一次 200 |
| 1.5 | 静态模型表 | `models.go` | 3 档位可列出 |

**状态（2026-10-18）**：1.1–1.5 全部完成。`cgo-shim-build.py qwenwork` build + vet + test 全绿；
新增 `qwenwork_contract_test.go` 覆盖 Device Flow URL、MaaS 签发/轮换解析、chat-modes 目录解析
（8 个用例，含 5 个子用例），并用必失败哨兵验证测试真实进编译。
**未完成**：真实账号在 CPA 宿主内跑通 `/v1/chat/completions`（需部署到生产后验证，属 CYCLE-05 验收项）。

**完成条件**：CPA 宿主里配置单账号，`/v1/chat/completions` 流式返回真实内容。

### 7.1 完整验收发现与修复（2026-10-18）

对 qwenwork 做完整验收时发现 6 个真实缺口，已全部修复：

| # | 缺口 | 影响 | 修复 |
|---|---|---|---|
| 1 | `VERSION` 与 `main.go version` 仍是 qoderwork 的 `0.9.44` | 构建产物版本错误，registry 版本冲突 | 改为 `0.1.0` |
| 2 | `oauth.go` 写 `Domain: "qoder.com.cn"` | 账号归属域错误 | 改为 `qwenwork.cn` |
| 3 | `gatewayBaseCN` 常量无引用 | 死代码 | 删除 |
| 4 | MaaS 签发/轮换无独立超时 | 关键路径沿用 120s 兜底，上游挂死会长时间阻塞 | 加 `maasRequestTimeout = 15s` |
| 5 | `model_specs.go` 保留 10 条 Qoder 代号（`qmodel_*` 等）且缺 `flash`/`pro` | 真实档位落到 131072 兜底，1M 上下文被误报 | 删除 Qoder 代号，补三档真实模型 |
| 6 | 未接入 CI 与 registry | 无法构建发布 | CI 全量接入（见 §7.2） |

**验证证据**：新增 `TestOfficialSpecsCoverQwenWorkTiers` 并用变异反证（把 `flash` 的
ContextWindow 改成 999 → 测试精确 FAIL → 还原后全绿）。全部 Go 文件 gofmt 干净。

### 7.2 CI 接入点（CYCLE-05 前置）

`.github/workflows/build.yml` 已按 coze/doubao 同款模板接入 qwenwork：

- `push.tags`：新增 `qwenwork-provider-v*`
- `workflow_dispatch.inputs.plugin.options`：新增 `qwenwork-provider`
- `jobs.test.strategy.matrix.include`：新增 `dir: qwenwork`
- `jobs.build`（native）：5 条（linux amd64/arm64、darwin amd64/arm64、windows amd64）
- `jobs.build-cross`：2 条（windows-arm64、freebsd-amd64）
- `jobs.release.if`：新增 tag 前缀
- `jobs.release` meta 解析：新增 `qwenwork-provider` 分支
- `jobs.release` 显示名映射：`QwenWork Provider`

**registry.json 尚未写入 qwenwork 条目**：`publish-assets.py` 要求 7 平台产物齐全并校验
sha256，必须先跑 CI 产出真实 assets。空 artifacts 会导致 CPA 整体拒绝 registry 源
（历史缺陷，见记忆 `project-registry-empty-artifacts-rejects-whole-source`）。

### CYCLE-02：多账号与调度

- 账号池（authfile / host_auth / lifecycle）
- 调度三模式 + 会话粘性
- 冷却阶梯 + 换号重试

**完成条件**：2 个账号，人为让 1 个失败，请求自动切到另 1 个。

#### CYCLE-02 执行结果（2026-10-18）

**核心缺陷（P0，克隆遗留）**：执行器把 `auth.accessToken` 当作账号身份，而调度候选、
冷却表、会话绑定、积分缓存全部按宿主 `auth.ID`（`qwenwork-<uid>.json`）索引。
宿主 `auth.ID` 由 [authIDForPath](file:///C:/Users/luode/go/pkg/mod/github.com/router-for-me/!c!l!i!proxy!a!p!i/v7@v7.2.129/internal/pluginhost/auth_callbacks.go#L504)
从相对路径派生，与凭据毫无关系；实测 access token 是 552 字符 JWT。后果：失败冷却写到
永远查不到的键上 → 故障账号永不退出轮转，多账号调度实际退化为单账号。

**修复项**：

| 编号 | 位置 | 问题 | 处置 |
|---|---|---|---|
| C2-1 | `main.go` / `stream.go` | 执行器用 accessToken 作账号身份 | 新增 `accountIdentity()`，统一为宿主 `auth.ID`；流式路径补 `initialAuthID` 参数 |
| C2-2 | `usage_config.go` | `scheduler_mode: off` 合法但解析器不识别，被静默当 session | 三值全识别，未知值仍回落 session |
| C2-3 | `keepalive.go` | 令牌刷新走 `buildAuthFileJSON` 重建 payload，丢掉 `disabled` / `manual_disable` / `exhausted_disable` / 计数 / `daily_cooling_until` | 改为 `foldCredentialIntoDoc` 折叠式直写，只替换 auth/account 子树 |
| C2-4 | `keepalive.go` | `markSessionDead` 走 host.auth.save，同样丢顶层字段 | 改 `persistAuthDirect` |
| C2-5 | `daily_cooldown.go` | `hostAuthIndexForPhys` 只按 `f.ID` 精确匹配，裸 UID 反查失败 | 补 Name / 规范文件名两级匹配 |
| C2-6 | `policy.go` | `labelForAuth` 含恒假分支 `"cn" == "global"` | 删除死分支 |

**验证**：

- 单测：`qwenwork/multiaccount_test.go` 新增 9 个用例（身份推导、凭据不泄漏、冷却同键可见、
  accessToken 反证、会话驱逐、off 模式、折叠保留运营字段、冷却排除 + defer）。
- 变异反证（三次，均精确命中）：① `accountIdentity` 退回 accessToken → 2 个用例 FAIL；
  ② 删除 off 解析 → `TestSchedulerMode_OffIsHonored` FAIL；③ 折叠改重建 → 2 个折叠用例 FAIL；
  ④ 同时关闭两处冷却闸门 → 4 个用例 FAIL（含新增的 `TestSchedulerPick_CoolingAccountExcluded`）。
- 真实 `.so` 探针：`test/qwenwork/multiaccount_probe.py` 用 ctypes 驱动 cgo 产物，
  9/9 PASS（off defer / session 粘性与分散 / credits 粘面板 / 禁用候选 defer / clear-cooldown 路由）。
  探针自身判别力经变异验证（删除 off 解析 → `off 模式 Handled=false` FAIL）。
- 编译：`cgo-shim-build.py` build+vet+test 全绿；WSL 真实 cgo 构建 7.85MB ELF + `go test` + `go vet` 全绿。

**未验证（诚实标注）**：真实双账号上游切换。本机只有 1 个真实 QwenWork 账号，
且执行器失败路径需先过真实 MaaS 签发（`sharedHTTPClient` 直连，不经宿主桥），
桩驱动无法在不联网前提下稳定注入 401。故「失败 → 冷却 → 换号」的端到端链路
由同键一致性的单测 + 调度侧冷却排除单测组合覆盖，**未做双真实账号实证**。

**未改动（有意保留）**：`clientUA = "qoderwork/1.3.1"` 是真实登录与推理实测通过的 UA
（探针 `CLIENT_VERSION` 同值），非克隆残留；`isDailyCountExceeded` 的 110 判定对
QwenWork 无对应语义，但实测四种常见 110 形态均不误伤，保留为防御。

**发布结果（2026-10-18）**：`qwenwork-provider` **0.2.0** 已发布。

| 项 | 结果 |
|---|---|
| 源码提交 | `470cf82`（14 文件，+1382/−114） |
| CI run | `39062771266` 全部成功（Test + 6 平台 build/cross + Release） |
| 产物 | 6 平台 zip + `checksums.txt`，逐包 sha256 自校验通过 |
| 资产提交 | `cc7fe59`（先于 registry 推送，确保 URL 可达） |
| registry | `bf36ab1`，`version=0.2.0`、6 artifacts，`validate-registry.py` OK |
| 远端验证 | 6/6 raw URL HTTP 200，`Content-Length` 与 registry `size` 全等；下载复算 sha256 6/6 一致 |
| 旧版清理 | `33d5fb2` prune `qwenwork-provider-0.1.0`（本地与远端目录均移除，旧 URL 现 404 为预期） |
| 产物版本核验 | 6 个二进制内均含 `0.2.0` 版本串 |

**生产部署（2026-10-18 04:04:54）**：首次安装到生产 CPA 宿主，走 `plugin-store install` 热重载，**零容器重启**。

| 项 | 结果 |
|---|---|
| 安装命令 | `POST /v0/management/plugin-store/qwenwork-provider/install?version=0.2.0` → HTTP 200，`restart_required:false` |
| 落盘文件 | `plugins/linux/amd64/qwenwork-provider-v0.2.0.so`，7844832 字节 |
| 哈希比对 | 落盘 sha256 `b75b56282df6e97bf0fc7839f044149a6f68b6b171e67a17b540de1bfd829da6` 与本地 zip 内 `.so` **完全一致** |
| 热重载日志 | `plugin loaded` / `plugin registered`，`plugin_id=qwenwork-provider version=0.2.0`（首装无 retired 版本） |
| 插件列表 | `registered=true enabled=true version=0.2.0` |
| 路由验收 | `/accounts` 200（`scheduler_mode:session`、0 账号）、`/config` 200（2605B）、`/models` 200、面板 `/v0/resource/plugins/qwenwork-provider/panel` 200（62899B） |
| 外网入口 | `https://cpa.luode.vip/v1/models` 200，共 36 个模型 |
| 配置完整性 | `config_store` 真实换行 1502 行（远非 1，未塌缩）、端口稳定 `:8317`、spool mtime 04:04 与安装时刻一致 |
| 容器状态 | `Up 2 days`，**未重启** |
| 运行期错误 | 近 5 分钟无 qwenwork 相关 error/panic |

**生产账号**：安装后 `accounts:[]`（0 账号），需用户在面板走 Device Flow 登录后才会出现账号；本轮不代登录。

### CYCLE-03：额度与生命周期

- `account-context` + `wallets` 额度查询
- 低于阈值禁用 / 恢复启用
- MaaS key 2h 轮换

**完成条件**：额度正确显示；耗尽账号被禁用；2h 后 key 自动轮换成功。

### CYCLE-04：管理面板

- 12 条路由 + `panel.html`
- 导入 / 导出 / 删除 / 测试

**完成条件**：面板可完成全部运维操作。

### CYCLE-05：用量与发布

- NDJSON feed 接入
- CI 8 处 + registry
- 生产热重载验收

**完成条件**：registry 可安装，生产环境热重载生效。

### CYCLE-06（可选）：Anthropic 协议输出

---

## 8. 真实测试入口

| 层级 | 命令 | 通过标准 |
|---|---|---|
| 编译 | `python scripts/cgo-shim-build.py qwenwork` | build + vet + test 全绿 |
| 单测 | 同上（内含 `go test`） | 0 FAIL |
| 上游闭环 | `python scripts/qwenwork_login_probe.py probe` | 模型清单 + 双协议 200 |
| 宿主集成 | CPA 本地实例 + 单账号 | 流式返回真实内容 |
| 生产验收 | `plugin-store install` + 面板四路由 | 200 + 热重载日志 |

依赖环境：Python 3.14、Go（WSL 或本机）、CPA 本地实例。

样本数据：真实 QwenWork 账号（个人免费版）。

---

## 9. 阻断项与风险

| 编号 | 风险 | 影响 | 处置 |
|---|---|---|---|
| R1 | 多账号 MaaS 签发限流未知 | CYCLE-02 可能受阻 | CYCLE-02 前先做 5 账号并发探测 |
| R2 | 签到能力当前账号 404 | 少一项运维能力 | 换账号复验后再决定是否补 |
| R3 | `total`/`used` 恒 null | 面板无法显示总量 | 改用 `remaining` + 钱包三档 |
| R4 | MaaS key 2h 轮换失败 | 请求中断 | 轮换失败即换号，不重试原号 |
| R5 | 上游随时可能变更 | 长期维护成本 | 探针脚本纳入回归 |

---

## 10. 任务停止与最大推进边界

- **本轮停止条件**：CYCLE-01 完成（单账号端到端跑通）即停，不自动进入 CYCLE-02。
- **最大推进边界**：仅新建 `qwenwork/` 目录与 CI/registry 接入，不改动任何既有插件源码。
- 若 CYCLE-01 期间发现上游契约与本文档不符，立即停止并回到本文档修订，不带着错误假设继续实现。
