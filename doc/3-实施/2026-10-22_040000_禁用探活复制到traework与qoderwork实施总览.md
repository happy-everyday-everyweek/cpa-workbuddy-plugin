# 禁用探活复制到 traework 与 qoderwork 实施总览

> 周期：2026-10-22
> 范围：CYCLE-1（traework）+ CYCLE-2（qoderwork）；workbuddy 样板见
> `2026-10-22_023000_账号故障禁用探活取代冷却探活_CYCLE0样板实施总览.md`
> 状态：**两个插件均已完成 build+vet+test 全绿与 mutation 反证；未发布**

## 1. 当前计划最终方案的简要说明

把 workbuddy 已验证的「失败双写 + 串行禁用探活」复制到 `traework` 与 `qoderwork`：
失败时同时写内存阶梯冷却与盘上 `disabled: true`，探活成功时同时解除两侧。
探活周期初版为 60 秒，2026-10-22 按「账号规模将到 200 个」改判为 **10 分钟**。
traework 属同构移植（删掉原有 `cooldown_probe*.go`）；qoderwork 属逐函数适配
（它的冷却只有账号单层、原本完全不做自动探活），并额外落地用户裁定的
「单账号当日 10 次探活失败即停到本地 0 点」。

本轮范围只有这 3 个插件（用户 2026-10-22 明确收窄），其余插件不在本次范围。

## 2. 两插件的结构差异（决定移植方式）

| 维度 | workbuddy（样板） | traework | qoderwork |
|---|---|---|---|
| 冷却维度 | 账号 + 账号×模型双层 | 双层（同样板） | **仅账号单层** |
| 记账落点 | `recordFailureScoped` | `recordFailureScoped`（同构） | `recordAccountFailure` |
| 冷却探活 | 已删除 | `cooldown_probe*.go` 720 行，**已删除** | 原本就没有 |
| 物理直写 | `persistAuthDirect` | 同名同签名 | 同名同签名 |
| `waitAuthDisabledState` | 已有 | **缺失，本轮新增** | **缺失，本轮新增** |
| refresh 回传 | 会重建文档，需回传标记 | 空实现（`{ok:true}`），**无重建** | 会重建文档，需回传标记 |
| 模型枚举 | `orderedAllWorkbuddyModels` | `orderedAllTraeModels` | **缺失，本轮新增 `orderedAllQoderModels`** |
| 账号结构 | `storedAuth{Auth,Account}` | `traeAuth`（扁平，`UserID`） | `storedAuth{Auth,Account}` |
| 独立持久化冷却 | 无 | 无 | **有**：110 日限额 24h `daily_cooling` |

qoderwork 的 `daily_cooling` 是与本机制并列的第三方禁用来源。它带
`daily_cooling_until` 标记，本机制的认领判据把它排除在外——既不覆盖也不解除，
避免探活把日限额冷却「治」成可用。

## 3. 逐插件改动

### 3.1 traework（CYCLE-1，同构移植）

新增：
- `traework/failover_disable.go`：禁用/恢复/探活引擎，与样板同构，类型换成 `traeAuth`、
  模型枚举换成 `orderedAllTraeModels`。因 traework 无白名单重建作用于既有账号
  （三处 `buildAuthFileJSON` 全是导入新凭据），未带 `putFailoverMarkersIntoExtra`。
- `traework/failover_disable_test.go`：23 个用例（禁用幂等、所有权边界、恢复让位
  manual、盘上重建候选、只回放失败模型、串行账号非重叠调度、
  并发写盘一次、标记经 parse 回传并活过宿主重建、重建落点审计）。

修改：
- `accountFailover.go`：`recordFailureScoped` 拆为「冷却写入 `recordCooldownScoped` +
  锁外禁用写盘」；`isAccountCoolingDown` 增加本地禁用第一层判据；
  `resetAccountFailover` 增加解除盘上禁用；抽出 `resetCooldownState`。
- `host_auth.go`：新增 `waitAuthDisabledState`（确认宿主吸收禁用状态）。
- `main.go`：`handleParseAuth` 回传盘上 `disabled` 与三个标记
  （`carryFailoverKeysIntoMetadata`）。
- `watchdog.go`：启动行 `cooldownProbeLoop` → `failoverProbeLoop`。
- `model_circuit.go` / `executor.go`（6 处）/ `failure_class.go`：注释对齐双写口径。
- `model_circuit_test.go`：删掉依赖已删除探活符号的 3 个用例，改为「11128 命中同时
  禁用整号」与「禁用探活回放失败模型」。
- `active_auth.go` / `management.go` / `panel.html`：面板显示「禁用探活中」，
  失败筛选与计数一并纳入。

删除：`cooldown_probe.go`、`cooldown_probe_test.go`、`cooldown_probe_cycle_test.go`。

### 3.2 qoderwork（CYCLE-2，逐函数适配）

新增：
- `qoderwork/failover_disable.go`：同一引擎，额外含 Qoder 专属当日配额
  （`failoverProbeQuota` / `failoverProbeFailLimit = 10` / 本地日期翻篇重置）。
- `qoderwork/failover_disable_test.go`：26 个用例，含两个 Qoder 专属用例
  （当日上限停探至次日 0 点、恢复后配额清零）与 `daily_cooling` 不被认领的反例。

修改：
- `accountFailover.go`：`recordAccountFailure` 拆出 `recordCooldownState` 并追加
  锁外禁用；`isAccountCoolingDown` 增加本地禁用判据；`resetAccountFailover`
  增加解除禁用；新增 `resetCooldownState`。
- `lifecycle.go`：新增 `noteAccountFailureForModel` /
  `reconcileAfterExecutorErrorForModel` / `reconcileByUIDForModel`（把失败模型
  带给禁用侧）；`deleteAuth` 兜底与 `syncAuthNote` 补 `putFailoverMarkersIntoExtra`；
  `clearDeletedAccountState` 清理本机制三张内存表。
- `main.go`：所有记账调用点改带模型；`handleParseAuth` 回传标记与 `disabled`；
  启动序列接入 `startFailoverProbe()`。
- `stream.go`：4 处记账调用改带模型。
- `oauth.go`：`toAuthDataForRefresh(sa, hostMeta)` 保留 `disabled` 并回传标记
  （宿主刷新选号不看 `Disabled`，硬编码 false 会绕开探活直接解封）。
- `active_ping.go`：文件头注释改写——原「不做任何自动探活」的约束只对健康号成立，
  已知坏号的恢复探活不属于该限制。（本会话曾新增 `orderedAllQoderModels` 供全量
  探活，随 2026-10-22 改判「只探失败模型」已删除。）
- `host_auth.go`：新增 `waitAuthDisabledState`。
- `watchdog.go`：新增 `startFailoverProbe`。
- `keepalive.go`：`persistAuthTokens` 补标记搬运（审计用例抓到的真实缺口：
  token 保活每 4 小时重建文档，会把禁用号写成「仍 disabled 但无标记」）。
- `panel.go`：`FailoverDisabled` / `FailoverModel` / `ProbeFails` / `ProbeLimit`
  字段与读盘判定。

## 4. 双写与恢复的语义（两插件一致）

- **失败**：`recordAccountFailure`/`recordFailureScoped` → 内存冷却写完后，锁外调用
  `disableAccountForFailover`。后者按 `auth_index` 取文档锁，逐键改盘上现文档
  （不做白名单重建），写入 `disabled` + `failover_disable` + `failover_disable_model`
  + `failover_disable_at`。
- **认领边界**：盘上已 `disabled` 且带 `failover_disable` → 幂等成功；
  带 `manual_disable` / `exhausted_disable` / `daily_cooling_until` / 无标记 →
  返回 false，不写盘、不接管恢复。
- **探活**：每 10 分钟一轮，`runFailoverTickNonOverlap` 用 TryLock 实现「上一轮没完
  就跳过本轮」；候选从盘上枚举（`failoverProbeTargets`），串行遍历账号，**每个账号
  只回放失败的那一个模型**，该模型成功即 `reenableAccountForFailover`；没有记录到
  失败模型则跳过该账号（无法验证就不恢复）。
- **恢复**：写回 `disabled:false`、删三个自有键、清内存冷却 + 失败模型 + 本地集合。
  若盘上有 `manual_disable` 一律不动。
- **外部启用对账**：每轮先 `reconcileFailoverLocalSet`，把盘上已 enabled 的号从本地
  排除集合里清掉（插件重启后集合为空，不依赖内存即可继续探活）。

## 5. 验证

### 5.1 cgo-shim（build + vet + test）

| 插件 | 结果 |
|---|---|
| traework | build OK / vet OK / test `ok … 2.700s` / **all green** |
| qoderwork | build OK / vet OK / test `ok … 2.893s` / **all green** |
| workbuddy（回归） | 沿用 CYCLE-0 结论，本轮未再改动 |

### 5.2 race

`-race` 需要 cgo，本机无 C 工具链，改用 WSL Go 1.26.4：

- traework：`CGO_ENABLED=1 go test -race …` → `ok 1.105s`
- qoderwork：`CGO_ENABLED=1 go test -race …` → `ok 1.101s`
- 覆盖范围：禁用/恢复/并发/候选/审计全部本机制用例。

### 5.3 mutation 反证（全部精确捕获）

| 变异 | 命中用例 | 实测差异 |
|---|---|---|
| traework 去掉文档锁 | `TestConcurrentDisableFromAliasIDsWritesOnce` | 写盘 6 次，want 1 |
| traework parse 不回传标记 | `TestParseAuthCarriesFailoverMarkersIntoMetadata` + `TestHostRebuildFromParseAuthDataKeepsMarkers` | `Metadata[failover_disable] = <nil>`；宿主重建后标记丢失 |
| traework 去掉所有权判定 | `TestDisableAccountForFailoverNeverClaimsOthersDisabled` | manual / exhausted / markerless 三类全被误认领 |
| qoderwork 去掉当日上限 | `TestFailoverProbeDailyLimitStopsProbingUntilMidnight` | 达到 10 次后仍探测（probed=1，want 0） |

### 5.4 审计用例抓到的两个真实缺口

`TestWhitelistRebuildSitesCarryFailoverMarkers` / `TestWhitelistRebuildSitesAreImportOnly`
两个扫描用例在本轮各抓到一处我事先没有预判到的落点：

1. qoderwork `keepalive.persistAuthTokens`：作用于既有账号的白名单重建，
   未搬运标记 → token 保活会把禁用号变成「仍 disabled、无标记」，永久卡死。
   **已补搬运**。
2. traework 的两个导入路径（`finishBrowserLogin`、`handleImportCredential`）与
   qoderwork 的 `handleImportPAT`：确属「盘上还没有这个号」，登记为豁免并写明理由。

## 6. 与 CYCLE-0 的口径差异

- **qoderwork 多一条当日探活上限**（用户裁定）：单账号单日 10 次探活失败后停探，
  本地 0 点恢复。这是 Qoder 系「上游按日限调用」的让位，避免探活烧光账号配额。
  traework / workbuddy 无此限制（用户未要求）。
- **traework 的 refresh 是空实现**，因此不存在「刷新解封」这个口子；只有 parse
  回传一条通道。qoderwork 两条通道都在。
- **traework 无白名单重建作用于既有账号**，故不带 `putFailoverMarkersIntoExtra`；
  它用一个「重建落点只允许是导入路径」的审计用例守住这条边界。

## 7. 未覆盖项与风险

- **未做真实宿主四态验收**：workbuddy 样板做过（四态 + 标记 20 秒零丢失），
  本轮两插件只做了离线验证与 race，**未在真实 CPA 宿主上跑四态**。
  要启动本地宿主需重新搭 MITM 假上游与伪造凭据，本轮未重复搭建。
- **未发布**：按用户会话约束，未 commit / push / dispatch CI / 改 registry / 部署。
- **`resetAuthModelFailover` 现在只剩单测在使用**（workbuddy / traework 一致）：
  成功路径统一走 `resetAccountFailover` 整账号清空。函数本身保留，因为它是
  「只清模型维度、保留账号级」这一语义的唯一实现，删掉会让该语义无处可依；
  `model_circuit.go` 的注释已同步改成实际调用路径，不再声称成功路径用它。
- **qoderwork 的探活会与真实业务共享日调用配额**：这是用户明确接受的代价，
  由 10 次上限兜底；若生产观察到配额仍被吃掉，应下调 `failoverProbeFailLimit`。
  改判「只探失败模型」后单轮请求数从 1~N 降为恒 1 次，配额压力同步下降。

## 8. 变更记录：探活范围收窄为「仅失败模型」（2026-10-22）

用户改判：**每个账号的每个模型额度彼此独立**，别的模型探通不能证明失败模型已恢复。
两个插件与 workbuddy 同步落地：

- 删除 `failoverProbeModelList` 与 `failoverModelsFn` 注入点；`runFailoverProbeTick`
  直接回放候选里的 `model`，单账号单轮只发一次探活。
- 无失败模型时跳过该账号并打显眼日志（原来是退化为全部候选模型）。
- qoderwork 另外删除了 `orderedAllQoderModels`（本会话新增，随改判失去唯一消费者）；
  `orderedAllWorkbuddyModels` / `orderedAllTraeModels` 仍被 4 小时健康探活使用，保留。
- 当日配额语义不变：只有**真的发出探活且失败**才计一次；跳过不消耗配额。

测试改写：`TestRunFailoverProbeAnyModelSuccessRecovers` →
`TestRunFailoverProbeOtherModelSuccessDoesNotRecover`；`…StopsAfterFirstSuccess` →
`TestRunFailoverProbeStopsAfterOnePing`；`…NoCandidateModelsSkips` /
`TestFailoverProbeModelListWithoutFailedModel` → `TestRunFailoverProbeWithoutFailedModelSkips`；
新增 `TestRunFailoverProbeSyncsLatestFailedModel`。traework 的
`TestModelBlockedDisableProbeReplaysFailedModel` 增加「实际发出的探活只有失败模型一个」的断言。

验证（本轮实跑）：

| 插件 | cgo-shim | WSL `-race`（本机制用例） |
|---|---|---|
| workbuddy | all green 1.383s | ok |
| traework | all green 2.692s | ok |
| qoderwork | all green 2.807s | ok |

`-race` 全量跑另有两处**与本轮无关的既有**竞态失败，涉及文件相对 HEAD 无改动：
traework `host_bridge.go` ↔ `host_bridge_read_timeout_test.go`（bridgedRead 后台 goroutine
与 degradeToDirect 的字段写竞争）、qoderwork `checkin.go` ↔ `scheduler_jitter_test.go`
（后台 schedulerLoop 读 `schedulerJitterFn` 与测试改写它竞争）。本轮未修。

## 9. 变更记录：探活周期 60 秒 → 10 分钟（2026-10-22）

与 workbuddy 同步改判（账号规模将到 200 个）。两插件 `failoverProbeInterval`
改为 `10 * time.Minute`，相关注释、面板文案与周期断言一并更新。

qoderwork 的当日探活上限语义不变：**只有真的发出探活且失败才计一次**，跳过不消耗
配额。周期拉长后单日可达探活次数从最多 1440 次降到 144 次，10 次上限更容易触发，
但也更不容易把 Qoder 的日调用配额烧在探活上——两个方向都更保守。

验证：两插件 cgo-shim 全绿（traework 3.402s / qoderwork 2.834s）。


