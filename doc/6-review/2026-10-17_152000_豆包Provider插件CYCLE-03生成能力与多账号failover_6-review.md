---
schema_version: 1
doc_type: style_regression
source_object_id: REQ-DOUBAO-PROVIDER-20261017
task_scope: CYCLE-03 生成能力与多账号 failover（TASK-007 ~ TASK-008）
status: PASS
updated_at: 2026-10-17 15:20:00
---

# 豆包 Provider 插件 CYCLE-03 生成能力与多账号 failover 6-review

结论：本轮新增的生成结果解析、无水印换取、阶梯冷却、账号调度与面板账号管理，写法与既有 9 个插件一致，文件落点与实施总览一致；拆分后所有生产文件均在 200 行以内，未引入局部风格跳变；**STYLE: PASS**。

- 来源对象：`REQ-DOUBAO-PROVIDER-20261017`（需求 v1.8）
- 对应实施文档：`doc/3-实施/2026-10-17_033614_豆包Provider插件立项需求_实施总览.md`
- 真实测试证据：`doc/5-tests/2026-10-17_150500_豆包Provider插件CYCLE-03生成能力与多账号failover实测.md`
- 本轮范围：`doubao/creation.go`、`doubao/creation_parse.go`、`doubao/creation_merge.go`、`doubao/creation_watermark.go`、`doubao/creation_render.go`、`doubao/failover.go`、`doubao/scheduler.go`、`doubao/executor.go`、`doubao/executor_stream.go`、`doubao/executor_failover.go`、`doubao/main.go`、`doubao/panel.html`、`doubao/CHANGELOG.md`，以及 `doubao/creation_test.go`、`doubao/failover_test.go`、`test/doubao/panel_accounts_repro.mjs`
- 检查步骤来源：`python scripts/static_owner_router.py --root "C:/Users/luode/.qoder/skills" --changed ...`，命中 **STYLE-01 / 02 / 03 / 04 / 05 / 07 / 08 / 09** 八步

## 逐步检查结论

| 步骤 | 检查项 | 结论 | 证据 |
|---|---|---|---|
| STYLE-01 | UTF-8 无 BOM、LF、无尾随空白、gofmt | 通过 | 10 个生产文件 + 3 个测试/脚本文件均 `bom=False / crlf=0 / trailing_ws=0`；`gofmt -l doubao/` 返回空 |
| STYLE-02 | 命名与同层符号一致 | 通过 | 冷却族沿用仓库既有命名：`isAccountCoolingDown` / `noteAccountFailure` / `resetAccountFailover` / `accountFailureCount` / `failoverCooldownBase` / `failoverCooldownMax` 与 `workbuddy`、`copilot` 同名同义；调度族 `handleSchedulerPick` / `candidateDisabled` 与 `copilot/scheduler.go` 一致；豆包专属前缀 `creation*` / `watermark*` / `skill*` / `blockType*` 统一 |
| STYLE-03 | 注释分层与定义位置 | 通过 | 脚本核验 10 个生产文件 **0 处**缺「说明 + `[参数]` + `[返回]` + 最近修改时间」；关键非显然约束均有注释：CREATION 块同资源多次上报、完整 key 才有无水印、业务 400 不计入冷却、全灭 defer 的跨 provider 语义、越界取号会被宿主判 unknown auth id |
| STYLE-04 | 函数签名与参数结构 | 通过 | 生产函数最多 3 参（`runWithFailover` 5 参但为执行入口且参数语义互不相关，与 `copilot` 同类实现一致）；`streamResponse` / `executorStreamRequest` / `chatRequest` 用结构体承载多字段 |
| STYLE-05 | 结构与落点 | 通过 | 落点与实施总览 §6 目录树一致：`creation*.go`（解析/合并/无水印/渲染）、`failover.go`（阶梯冷却）、`scheduler.go`（选号 + `scheduler.pick`）、`executor*.go`（接入/流式/故障转移）；`doubao/` 未引用其他插件包（`grep` 0 命中） |
| STYLE-07 | 接口契约与数据访问 | 通过 | 管理路由统一 `/plugins/<providerName>/*` 前缀，新增 `scheduler.pick` 走 `pluginabi.MethodSchedulerPick` 常量而非字面量；账号启停直写物理文件（不经 `host.auth.save`，避免丢未知字段），与 `authfile.go` 既有口径一致；`scheduler.pick` 只返回宿主候选集内 ID |
| STYLE-08 | 语言与框架特异写法 | 通过 | 无 `iota`（常量逐个显式赋值）；无 `fmt.Print` / `log.Print` 控制台输出；`panel.html` 单 `<script>` 块经 `node --check` 通过；新增 `panel_accounts_repro.mjs` 亦通过 `node --check` |
| STYLE-09 | 测试资产与编码前契约 | 通过 | Go 单测与源码同目录（`creation_test.go` / `failover_test.go`）；脚本类回归放 `test/doubao/`（与 `panel_import_repro.mjs` 同目录）；真实上游用例统一以 `DOUBAO_LIVE_TEST=1` 门控；测试夹具统一走 `writeFailoverAccount` + `t.TempDir()`，未触碰真实账号目录 |

## 发现与处理

| 序号 | 发现 | 处理 |
|---|---|---|
| 1 | `creation.go` 587 行，同时承载数据模型、解析、合并、无水印、渲染、流式帧六类职责，违反「单文件超 200 行拆分」与「一个文件只做一件事」 | 已拆为 5 个同级文件：`creation.go`（模型，110 行）、`creation_parse.go`（解析，146 行）、`creation_merge.go`（合并，79 行）、`creation_watermark.go`（无水印，136 行）、`creation_render.go`（渲染，146 行） |
| 2 | `executor.go` 拆分后仍 263 行，混有非流式入口、流式入口、同步收集、后台泵四类职责 | 已拆为 3 个同级文件：`executor.go`（167 行）、`executor_stream.go`（112 行）、`executor_failover.go`（74 行） |
| 3 | `failover.go` 同时承载冷却状态与账号选择，与实施总览「`scheduler.go` 负责账号选择」的落点不符 | 已把 `pickAccount` / `errNoAvailableAccount` / `handleSchedulerPick` / `candidateDisabled` 迁到 `scheduler.go`；`failover.go` 只保留冷却状态 |
| 4 | `runWithFailover` 的 `[参数]` 说明折行，导致元信息块被参数续行打断 | 拆分到 `executor_failover.go` 时重写为连续元信息块，脚本复核 0 处缺失 |
| 5 | `executor.go` 拆分后 `context` / `errors` 成为未使用导入 | 已删除；build/vet 全绿 |

## 拆分后复核

| 项目 | 结果 |
|---|---|
| 生产文件行数 | `creation.go` 110 / `creation_parse.go` 146 / `creation_merge.go` 79 / `creation_watermark.go` 136 / `creation_render.go` 146 / `executor.go` 167 / `executor_stream.go` 112 / `executor_failover.go` 74 / `failover.go` 182 / `scheduler.go` 115 —— **全部 ≤ 200 行** |
| 函数唯一性 | `grep "^func "` 跨文件统计无重复定义 |
| 测试总数 | 拆分前后均为 **101 PASS / 12 SKIP / 0 FAIL**，无用例丢失（拆分后另补 1 个 `TestCreationLiveVideo`，门控跳过，不影响默认口径） |
| 面板回归 | `panel_accounts_repro.mjs`（17 项）与 `panel_import_repro.mjs` 均全部通过 |
| 构建 | `python scripts/cgo-shim-build.py doubao` → build / vet / test 全绿 |
