# 6-review：WorkBuddy 三档积分分级（落盘 / 每小时串行扫描 / 面板读盘 / 签到与手动刷新后复检）

STYLE: PASS

## 检查范围

- 测试证据：`python scripts/cgo-shim-build.py workbuddy` → `go build` OK、`go vet` OK、`go test` OK（`ok github.com/luode0320/cpa-workbuddy-plugin/workbuddy 16.387s`），于注释补齐后重跑。
- 改动文件：
  - 新增：`workbuddy/credits_snapshot.go`、`workbuddy/credits_sweep.go`、`workbuddy/credit_disable.go`、`workbuddy/credits_sweep_test.go`
  - 修改：`workbuddy/policy.go`、`workbuddy/scheduler.go`、`workbuddy/active_auth.go`、`workbuddy/failover_retry.go`、`workbuddy/lifecycle.go`、`workbuddy/refresh_runner.go`、`workbuddy/watchdog.go`、`workbuddy/checkin.go`、`workbuddy/main.go`、`workbuddy/usage_config.go`、`workbuddy/panel.go`、`workbuddy/panel.html`、`workbuddy/min_routable_credits_test.go`、`workbuddy/scheduler_test.go`
- 规则来源：`PROJECT_STYLE.md`、`code-style-consistency-rules` references、`comment-rules` references。
- 检查步骤由 `static_owner_router.py`（以 `--root C:/Users/luode/.qoder/skills`）按变更路径推导，九步全部命中。

## 风格证据

- `STYLE-01` 静态格式与编码：18 个改动文件全部 UTF-8 无 BOM、无尾随空白（按真实行尾切分精确检测，0 命中）。新增 4 文件为 LF，仓库主体为 CRLF（`workbuddy/*.go` CRLF=71 / LF=21 / 混合=0）；`.gitattributes` 为 `* text=auto`，`git diff --stat` 未出现整文件伪变更（最大改动 `scheduler.go` 104 行，其余 4~85 行），无 CRLF 漂移。新增 4 文件经 `gofmt -l` 检查输出为空。
- `STYLE-02` 命名与符号引用：新增符号统一使用 `credit*` / `credits*` / `creditTier*` 前缀，与同目录既有 `lifecycle*`、`failover*` 前缀族写法一致；`creditTierDisabled/Preserve/Normal` 显式赋值，未用 `iota`（`policy.go:20` 的既有 `iota` 属历史代码，本轮未触碰）。
- `STYLE-03` 注释分层与定义位置：新增/修改函数均为「整体说明 + `[参数]` + `[返回]` + 最近修改时间（含一句话说明）」的完整层；字段、行内、补丁位点只写单句作用说明，未平摊完整格式。批量复检三个新增文件函数头元信息缺失数 = 0。包级 `var (` 分组沿用 `policy.go:37` 等既有写法；函数体内无 `var (` 分组声明。改动注释均为中文，仅保留 `note`/`credit_tier` 等必要技术标识。
- `STYLE-04` 函数签名与参数结构：新增 `gradeCreditsForAuth(4)`、`disableForLowCredits(5)`、`reenableForLowCredits(4)`、`recordCreditSnapshot(4)`、`creditProbeOK(3)`、`creditTierFor(3)` 与同包既有 28 个 ≥4 参数函数（`persistAuthDirect(4)`、`buildAuthFileJSON(4)`、`recordAccountFailureForModel(4)` 等）参数密度一致，属该包既有惯例，未以多行签名代替收敛。
- `STYLE-05` 结构与落点：新增 3 个源文件与 1 个测试文件均落在被测包 `workbuddy/`，与同包 `failover_disable.go`、`accountFailover.go` 的落点一致；依赖方向未越界（新增文件仅依赖包内既有函数与标准库）。
- `STYLE-06` 公共复用与工具落点：检索未发现同语义的 tier / 积分快照封装可复用；`crRemainOf`、`creditTierFor` 等为包内私有函数，未导出为公共工具，无需登记公共工具索引。
- `STYLE-07` 接口契约与数据访问：新增日志统一 `[workbuddy] <event>: auth=%s ...` 格式，与既有日志前缀和字段口径一致（`low-credit` 三类事件均带 `auth=` 稳定标识）；错误路径全部 `return err` 或记日志，无吞错。sidecar 写入走临时文件 + `os.Rename` 原子替换。
- `STYLE-08` 语言与框架特异写法：常量显式赋值（未新增 `iota`）；`strings.TrimSpace` 仅用于 `authID` 归一化（比较/判空前统一口径，符合规则允许的必要场景）。`panel.html` 两处 `<script>` 块经 `node --check` 校验通过。
- `STYLE-09` 测试资产与编码前契约：本轮新增 `credits_sweep_test.go` 与被测源码同包同级，符合该仓库 Go 单测落点惯例；`test/workbuddy/` 仅存放 `.mjs` 等跨语言 repro 脚本，本轮未改动。

## 边界声明

- 业务正确性：N/A + 原因：由真实测试负责 + 证据：`cgo-shim-build.py workbuddy` 全绿（build/vet/test）。
- 需求覆盖与发布放行：N/A + 原因：不属于 6-review 职责 + 证据：本轮实施任务 T1–T5 与变异验证记录。

## 交付残留自查（6 维）

| 维度 | 结论 | 证据 |
|---|---|---|
| 维度 1 需求覆盖对账 | 通过 | ①积分落盘+每小时串行全量（不分启用禁用）→ `credits_snapshot.go` `persistCreditSnapshot` / `credits_sweep.go` `runCreditSweep`（串行 for 循环，轮末一次落盘）。②落盘后检查：<100 禁用 → `credit_disable.go` `gradeCreditsForAuth` + `disableForLowCredits`；100~400 保活标 → `creditTierPreserve` + `scheduler.go` `filterCandidatesByTier`。③面板进入不刷上游、读落盘值、仍支持手动刷新且手动刷新后复检 → `panel.go` light-load 分支读 `creditSnapshotGet`；`refresh_runner.go` `doFetchOne` 接 `recordCreditSnapshot` + `gradeCreditsForAuth`。④签到后复检 → `checkin.go:212-216`。四条均有对应落点与测试。 |
| 维度 2 影响面 / 消费方对账 | 通过（发现 3 项，已修复） | 契约面变更：`min_routable_credits` 语义由「硬排除」改为「保活线」，新增配置键 `low_credit_disable_below`，新增 `/accounts` 输出字段 `credit_tier`。全仓扫描命中 3 处未同步：`main.go:362` 配置描述仍写「摘出路由池」、`credits_handler.go:395` 自留一份阈值比较、根 `README.md` 两处旧口径 + `workbuddy/README.md`/`README_CN.md` 描述旧行为。已全部同步（配置描述改写、`credits_handler` 改用 `creditTierFor` 统一入口并输出 `credit_tier`、三份 README 更新、CHANGELOG 补 0.18.1）。`cachedCreditsScore` / `accountRoutable` / `pickNextAuthForModel` 三个内部判据已同步收窄。 |
| 维度 3 残留物清扫 | 通过 | `git status --porcelain` 中本轮产物为 4 个新 `.go` + 1 份 `doc/6-review` 记录 + 14 个修改文件；`go vet` 无「declared and not used / imported and not used」。变异验证用的 `.bak` 临时文件已全部删除（`workbuddy/*.bak_*` 与 `/tmp/rr.bak`、`/tmp/ck.bak` 均清理）。工作树中 `.tmp_*.py`、`images/`、`AGENTS.md`、`CLAUDE.md`、`PROJECT_CURRENT.md` 属其它会话遗留，本轮未触碰、未纳入提交。 |
| 维度 4 文档与引用一致性 | 通过 | 新增 3 个源文件均被包内引用（`credits_snapshot` 1 处、`credits_sweep` 3 处、`credit_disable` 4 处），无零引用孤儿文件；`credits_sweep_test.go` 与源码同包可编译（已由真实测试证实）。`doc/6-review` 新记录路径与既有命名模板一致。 |
| 维度 5 口径一致性 | 通过 | 阈值 100 / 400 在 `policy.go`（`defaultLowCreditDisableBelow` / `defaultMinRoutableCredits`）、`main.go` 配置描述、`README.md`、`README_CN.md`、`CHANGELOG.md` 五处载体一致；`VERSION` 与 `main.go` `var version` 均为 `0.18.1`。 |
| 维度 6 验证有效性反审 | 通过（发现 2 项护栏缺口，已修复） | 变异验证共 5 组：①`creditTierFor` 的 `preserve <` 改 `<=` → 被 `TestCreditTierForBoundaries` 捕获；②轮末一次落盘改为逐账号落盘 → **初始存活**，补 `TestRunCreditSweepPersistsOnceForMultipleAccounts`（3 个账号全成功，中途落盘会得 3）后捕获；③解禁归属判定删除 → **初始存活**，因 `reenableForLowCredits` 内层还有复核，补「同时带 `low_credit_disable` 与对方标记 + 断言探活次数为 0」后捕获；④`doFetchOne` 去掉分级 → 被 `TestDoFetchOneRecordsSnapshotAndGrades` 捕获；⑤签到去掉分级 → 被 `TestProcessAutoCheckinGradesAfterCheckin` 捕获。另修一个真实缺陷：`creditTierFor` 原用 `remain <= disableBelow`，把恰好 100 分判成禁用，与需求「小于 100」及路由侧 `>= disableBelow` 相反，已改为严格小于并被边界测试钉死。新测试文件真实进编译由一次性哨兵 `TestSentinelMustFail` 证实（编译期输出预期失败后删除，`grep -c` 返回 0）。 |
