---
schema_version: 1
doc_type: style_regression
source_object_id: REQ-DOUBAO-PROVIDER-20261017
task_scope: TASK-003 Cookie Jar 写回与续期
status: PASS
updated_at: 2026-10-17 04:36:00
---

# 豆包 Provider 插件 Cookie Jar 写回与续期 6-review

结论：本轮新增的 `durableJar`（Set-Cookie 捕获、快照落盘、防覆盖）与登录态状态上报，写法与既有插件一致，未引入局部风格跳变；**STYLE: PASS**。

- 来源对象：`REQ-DOUBAO-PROVIDER-20261017`（需求 v1.6）
- 对应实施文档：`doc/3-实施/2026-10-17_033614_豆包Provider插件立项需求_实施总览.md`
- 真实测试证据：`doc/5-tests/2026-10-17_043530_豆包Provider插件TASK-003_CookieJar写回与续期实测.md`
- 本轮范围：`doubao/cookiejar.go`、`doubao/cookiejar_test.go`、`doubao/cookiejar_live_test.go`、`doubao/authfile.go`、`doubao/management.go`
- 检查步骤来源：`static_owner_router.py` 以 `--root C:/Users/luode/.qoder/skills` 运行，命中 **STYLE-01 / 02 / 03 / 04 / 08 / 09** 六步

## 逐步检查结论

| 步骤 | 检查项 | 结论 | 证据 |
|---|---|---|---|
| STYLE-01 | UTF-8 无 BOM、换行、无尾随空白、gofmt | 通过（含一处修正） | 3 个新文件均 `no-BOM / trailing_ws=0`；**修正**：`SetCookies` 注释的列表块与 `[参数]` 之间缺空注释行，gofmt 会插入空行，已补齐后 `gofmt -l` 返回空 |
| STYLE-02 | 命名与同层符号一致 | 通过 | 沿用参考实现（EXT-002 rex-zsd，MIT）的 `durableJar` / `savedCookie` / `snapshot` / `persist` 术语；`sessionCookieNames` / `hasLiveSession` / `isSessionCookieName` 语义直白；私有符号全部小写无前缀（包内） |
| STYLE-03 | 注释分层与定义位置 | 通过 | 全部业务函数具备「说明 + `[参数]` + `[返回]` + 最近修改时间」四要素（脚本核验 0 处缺失）；**关键**：`persist` 的注释说明了「为何要防覆盖」（多进程/热重载场景，计划指定的 6-review 关注点），`SetCookies` 说明了 Path/MaxAge 归一化的理由 |
| STYLE-04 | 函数签名与参数结构 | 通过 | 无函数参数 ≥3 个（脚本核验为空）；`snapshotLocked` 拆分为无锁变体以复用持锁上下文 |
| STYLE-08 | 语言与框架特异写法 | 通过 | 无 `iota`；`sync.Mutex` 保护 `records` 与 `base`，`defer j.mu.Unlock()` 统一释放；实现标准 `http.CookieJar` 接口（`Cookies` / `SetCookies`）以便直接挂到 `http.Client` |
| STYLE-09 | 测试资产与编码前契约 | 通过 | 单测落 `doubao/cookiejar_test.go`（同目录，Go 惯例）；真实上游用例单独落 `doubao/cookiejar_live_test.go` 并以 `DOUBAO_LIVE_TEST=1` 隔离，CI 默认跳过；哨兵测试证实新测试真实进编译 |

## 发现与处理

| 序号 | 发现 | 处理 |
|---|---|---|
| 1 | `persist` 持 `j.mu` 后调用 `snapshot()`，后者再次 `Lock()` 造成**自锁死锁**（`go test` 挂死至 601s 超时） | 抽出无锁变体 `snapshotLocked()`，`persist` 改调它；`snapshot()` 保留加锁版本供外部使用 |
| 2 | `SetCookies` 注释列表块与 `[参数]` 之间缺空注释行，不符合 gofmt 口径 | 已补空注释行，`gofmt -l` 返回空 |
| 3 | 死锁缺陷由**真实测试暴露**（挂死超时），非静态检查发现 | 已在测试文档记录；后续同类持锁调用需先检查是否有无锁变体 |

以上修正后 `python scripts/cgo-shim-build.py doubao`（build / vet / test）与 `node test/doubao/panel_import_repro.mjs` 均全绿。

## 并发安全与防覆盖说明（计划指定关注点）

- **并发安全**：`durableJar` 的 `records` 与 `base` 全部经 `j.mu` 保护；`TestDurableJarConcurrentSetCookies` 以 8 goroutine × 50 次并发写入验证同一 key 收敛为 1 条，无竞态。
- **为何要防覆盖**：provider 是长驻进程，启动后长期持有内存态；若期间用户在面板重新导入同一账号、或另一热重载实例更新了账号文件，本进程随后的一次滚动写回会用旧快照覆盖新凭据，导致用户刚粘贴的登录态凭空失效。故 `persist` 写入前比对文件身份（`os.SameFile` + `ModTime` + `Size`），发现被替换即放弃写入。`TestDurableJarPersistDefersToNewer` 锁定该行为。

## 负向边界声明

本记录只判断写法、位置、格式与既有习惯，不判断业务正确性、不重新验需求覆盖、不替代真实功能测试、不作发布放行或最终验收结论。AC-002 已按实测降级为「长有效期 + 失效重导入」（见需求 v1.6 与测试文档 §4），本记录不构成发布放行结论。
