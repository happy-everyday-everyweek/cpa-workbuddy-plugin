# Coze Provider 0.2.0 模型发现与面板重做 · 6-review

- 时间：2026-10-18 05:00
- 范围：`coze/models.go`、`coze/active_ping.go`、`coze/management.go`、`coze/main.go`、`coze/protocol.go`、`coze/panel.html` 及配套测试
- 结论：**STYLE: PASS**

## 1. 代码风格一致性

| 检查项 | 结论 | 说明 |
|---|---|---|
| 命名与既有文件一致 | PASS | 新增 `models.go` / `active_ping.go` 沿用 `credentials.go` 的「小写单文件单职责」划分；函数名动词开头（`discoverModels` / `cachedDiscoveredModels` / `runActiveTest`） |
| 函数注释四要素 | PASS | 每个新增函数均含作用说明、`[参数]`、`[返回]`、`最近修改时间：2026-10-18；改动原因：...` |
| 关键判定有 WHY 注释 | PASS | 缓存按账号分桶、`model.static` 不打网络、`model_type` 必须是字符串、探活只做认证层——均写明原因与实测依据 |
| 注释语言 | PASS | 全中文；仅保留 `model_type` / `sec_user_id` 等协议字段原文 |
| 文件规模 | PASS | `models.go` 约 630 行、`active_ping.go` 约 235 行；均围绕单一职责（模型发现 / 面板探活），未混入管理路由或凭据逻辑 |
| 错误处理 | PASS | 全部 `fmt.Errorf("...：%w", err)` 中文前缀 + 包装；失败路径不伪造成功（全端点失败返回错误而非空目录） |
| 并发安全 | PASS | 发现链路并发拉取后统一 `wg.Wait()`；缓存读写持锁；测试桩 `calls` 加锁避免 `-race` 竞争 |
| 测试文件镜像 | PASS | `models_test.go` / `active_ping_test.go` 与源文件同名同目录，命名 `Test<Func>_<Case>` |
| 面板风格 | PASS | 令牌、卡片、徽章、弹窗、toast 与 `workbuddy/panel.html` 同源；主题三态（light/white/dark）跟随父页 |
| 目录归位 | PASS | 未新增顶层目录；临时 UI 验证桩 `.tmp-ui-check/` 已删除 |

## 2. 发现与处置

| # | 位置 | 问题 | 处置 |
|---|---|---|---|
| 1 | `models.go:discoverModels` | 原实现串行打 7 个端点，宿主模型注册在认证加载路径同步执行，最坏拖 7×30s | 已改并发 + 单请求 8s 超时 |
| 2 | `models.go:modelStatic` | 原实现会在宿主启动期发起网络探测 | 已改为只返回内置种子，真实目录交给 `model.for_auth` |
| 3 | `models.go:catalogCache` | 原为单条缓存，多账号互相覆盖导致重复打上游 | 已改为按 `AuthID` 分桶，并补隔离测试 |
| 4 | `active_ping.go:runActiveTest` | 只校验会话有效性会把「会话活着但模型被收回」误报为可用 | 已改为双条件（判据端点 + 模型目录） |
| 5 | `models_test.go:modelListStub` | 并发化后 `calls` 无锁写入 | 已加 `sync.Mutex` |

无遗留缺陷；无未使用符号；无本轮产生的孤儿文件。

## 3. 与项目风格记忆的一致性

- 沿用 `workbuddy/models.go` 的 `pluginapi.ModelInfo` 构造口径（`Object` / `Created` 固定值 / `OwnedBy`）。
- 沿用 `coze/credentials.go` 的端点覆盖测试模式（`*Override` 变量 + `swap*` 恢复函数）。
- 沿用 `copilot/panel.html` 的「添加账号」由宿主弹窗承担的边界，未在插件面板重复实现授权。

## 4. 结论

`STYLE: PASS`，可进入发布流程。
