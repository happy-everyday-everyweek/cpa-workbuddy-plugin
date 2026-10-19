# 豆包 Provider 插件 CYCLE-03 生成能力与多账号 failover 真实测试

- 测试日期：2026-10-17
- 所属周期：CYCLE-03（生成能力与账号容灾）
- 对应任务：TASK-007（生图生视频 `skill` 参数与多模态返回）、TASK-008（多账号池与阶梯冷却 failover）
- 对应需求：`doc/2-需求/2026-10-17_011146_豆包Provider插件立项需求.md`（AC-005 / AC-006，v1.8 修订）
- 对应实施总览：`doc/3-实施/2026-10-17_033614_豆包Provider插件立项需求_实施总览.md`
- 测试对象：`doubao/creation.go`、`doubao/failover.go`、`doubao/scheduler.go`、`doubao/executor.go`、`doubao/main.go`、`doubao/panel.html`

## 1. 测试环境与入口

| 项目 | 值 |
| --- | --- |
| 平台 | Windows 10（本机，无 C 工具链） |
| 构建入口 | `python scripts/cgo-shim-build.py doubao`（CGO_ENABLED=0 影子构建：build + vet + test） |
| 真实上游入口 | `DOUBAO_LIVE_TEST=1 python scripts/cgo-shim-build.py doubao` |
| 面板回归入口 | `node test/doubao/panel_accounts_repro.mjs`、`node test/doubao/panel_import_repro.mjs` |
| 凭据来源 | 用户自有浏览器登录态（`doubao/.local_cookie.txt`，已 gitignore） |

## 2. 测试项与结果

### 2.1 影子构建（默认无网络）

- 命令：`python scripts/cgo-shim-build.py doubao`
- 结果：**全绿**（build / vet / test 三步均 OK）
- 用例统计：**101 PASS / 12 SKIP / 0 FAIL**（SKIP 均为 `DOUBAO_LIVE_TEST=1` 门控的真实上游用例）

### 2.2 TASK-007 生成结果解析（单元）

| 测试点 | 用例 | 结果 |
| --- | --- | --- |
| 首帧只有占位（宽高/描述）时解析出不带链接的资源 | `TestParseCreationBlockPlaceholder` | PASS |
| 后帧补齐 `key` + 直链时解析出完整资源，图片优先 `image_ori` | `TestParseCreationBlockFinal` | PASS |
| 同一资源多帧按 `id` 合并，后到非空值覆盖 | `TestMergeAssetsFillsFromLaterFrames` | PASS |
| 不同 `id` 不互相合并 | `TestMergeAssetsKeepsDistinctIDs` | PASS |
| 视频块识别（`type=2`） | `TestParseCreationBlockVideo` | PASS |
| 有媒体时 `content` 转多模态数组并带 `media` 扩展 | `TestAttachAssetsBuildsMultimodalContent` | PASS |
| 视频以文本段承载直链 | `TestAttachAssetsVideoUsesTextPart` | PASS |
| 纯文本回复不改写 `content` | `TestAttachAssetsNoopForTextReply` | PASS |
| 无水印解析要求带 bucket 前缀的完整 key | `TestResolveNoWatermarkRequiresFullKey` | PASS |
| 无水印接口失败时回退流内缩略图，不失败整个请求 | `TestHydrateAssetsDegradesOnFailure` | PASS |
| 视频不参与无水印解析 | `TestHydrateAssetsSkipsVideo` | PASS |
| 聚合链路从 SSE 事件中抽出资源 | `TestCollectCompletionExtractsAssets` | PASS |
| 无正文时给出媒体摘要，避免空白消息 | `TestDescribeAssets` | PASS |

### 2.3 TASK-008 冷却与选号（单元）

| 测试点 | 用例 | 结果 |
| --- | --- | --- |
| ① 阶梯计算 30s × n，封顶 600s（含 0 / 负数边界） | `TestFailoverCooldownFor` | PASS |
| 失败分类：传输错误 / 5xx / 429 / 402 / 401 / 403 / 流内码计入，业务 400/404 不计入 | `TestIsAccountFailureClassification` | PASS |
| 同请求换号只对 429/402/401/403/流内码触发，5xx 与传输错误不烧预算 | `TestShouldRotateClassification` | PASS |
| ② 失败记账后进入冷却 | `TestNoteAccountFailureAndReset` | PASS |
| 业务 400 不计数、不冷却 | `TestNoteAccountFailureIgnoresBusiness400` | PASS |
| 连续失败超上限时冷却不超过 600s | `TestNoteAccountFailureCapsCooldown` | PASS |
| ② 选号跳过停用与冷却账号 | `TestPickAccountSkipsDisabledAndCooling` | PASS |
| 换号排除刚失败的账号 | `TestPickAccountExcludesFailedAccount` | PASS |
| ④ 池内无可承载账号时返回明确错误 | `TestPickAccountExhausted` | PASS |
| ⑤ 启停直写物理文件并刷新 `UpdatedAt` | `TestSetAccountDisabledPersistsPhysically` | PASS |
| ④ 无可用候选时 `Handled:false` 让位宿主 | `TestHandleSchedulerPickDefersWhenNoUsableCandidate` | PASS |
| 冷却候选被剔除，全灭时 defer | `TestHandleSchedulerPickSkipsCoolingCandidate` | PASS |
| 只返回宿主候选集内的 AuthID（不越界取号） | `TestHandleSchedulerPickReturnsHostCandidate` | PASS |
| ③ 429 后换号并在新账号成功，失败账号冷却、成功账号计数清零 | `TestRunWithFailoverRotatesOn429` | PASS |
| 业务 400 不换号、不冷却任何账号 | `TestRunWithFailoverStopsOnBusiness400` | PASS |

### 2.4 TASK-008 面板账号管理（DOM 回归）

- 命令：`node test/doubao/panel_accounts_repro.mjs`
- 结果：**全部断言通过**（17 项）

| 断言 | 结果 |
| --- | --- |
| 一启用一禁用两账号均渲染，状态徽章与按钮文案一致 | PASS |
| 每个账号都有删除按钮 | PASS |
| 禁用账号点「启用」→ `/enable` 携带 `auth_id`，成功后刷新列表 | PASS |
| 启用账号点「禁用」→ `/disable` 携带 `auth_id` | PASS |
| 删除前弹确认，确认后 `/delete` 携带 `auth_id` | PASS |
| **取消确认时不发删除请求**（防误删） | PASS |
| 操作失败回显原因且为错误态，不误报成功 | PASS |

- 命令：`node test/doubao/panel_import_repro.mjs`
- 结果：**全部断言通过**（导入链路回归，未受本轮改动影响）

### 2.5 TASK-007 真实上游（`DOUBAO_LIVE_TEST=1`）

| 用例 | 验证点 | 结果 |
| --- | --- | --- |
| `TestCreationLiveImage` | `skill=3` + 绘制提示词 → 上游返回图片、可访问 URL、尺寸、无水印直链、多模态格式 | **PASS（14:34 实测）**：1 张图，2048×2048，生成器 `Seedream 5.0 Flash`，无水印 URL 长度 272 |
| `TestCreationLiveControlWithoutSkill` | 对照组：记录 `skill` 的真实作用 | **PASS（14:43 实测）**：不带 `skill` 仍生成图片，见第 3 节 |
| `TestCreationLiveExecutorMultimodal` | 完整 executor 链路返回多模态数组 | **PASS（14:38 实测）**：`content` 2 段、含 1 个 `image_url`，`media` 1 条 |

> 复测说明：14:57 复跑真实上游用例时，全部对话类请求（含生图）返回 `710022004 rate limited`，为上游账号级限流，非代码缺陷；上述 PASS 结论来自限流前的同版本实测。限流期间 `probe` / 导入类用例仍全部 PASS，证明凭据与传输链路本身正常。

### 2.6 TASK-007 视频路径

- 视频解析、合并、多模态承载、跳过无水印解析均由单元用例覆盖（2.2 节）；
- 新增 `TestCreationLiveVideo`（真实生视频，超时 540s），已进编译并按 `DOUBAO_LIVE_TEST=1` 门控；
- **真实生视频未取得正向证据**：上游限流阻断，且视频生成耗时更长、更易触发风控。记录为受限交接项，不虚报通过。

## 3. 关键发现：`skill` 不是生成路径的必要条件

原假设（来自 Lxapk 参考实现与需求 v1.5）：必须带 `skill=3`/`5` 才能激活生成路径，否则静默降级为文本描述。

实测对照（同一真实账号，同一提示词「画一只橘色的猫，简洁风格」）：

| 变体 | 上游行为 | 解析结果 |
| --- | --- | --- |
| `skill=3` + 绘制提示词 | 返回 `CREATION` 块（`block_type=2074`） | 图片 1 张 + 无水印直链 |
| **无 `skill`** + 同一绘制提示词 | **同样返回 `CREATION` 块** | 图片 1 张（`rc_gen_image` 出现 22 次，`block_type=2074` 出现 2 次） |
| `skill=3` + 闲聊提示词（"你好"） | 只回文本，无 `CREATION` 块 | 纯文本；上游回显 `input_skill.skill_id="0"`（未采纳该参数） |

结论：**上游按提示词意图决定是否生成，`skill` 参数未被采纳**。因此：

1. `skill` 注入保留（语义正确、无副作用，且上游策略可能随时收紧）；
2. 需求 AC-003 / AC-005 中「无 `skill` 必不生成」的断言已在 v1.8 撤回；
3. `TestCreationLiveControlWithoutSkill` 改写为断言「解析层在两种形态下都自洽」（不出现无 URL 的空壳资源；媒体与文本至少有一个），不再断言「无 `skill` 必无图片」。

## 4. 关键发现：防覆盖判定必须用内容摘要

`TestDurableJarPersistDefersToNewer` 出现间歇性失败，排查后确认是**真实缺陷**而非测试抖动：

- 原实现用 `os.SameFile + ModTime + Size` 判断账号文件是否被外部替换；
- Windows 下文件删除后重建可能复用文件索引，等长 JSON 也可能落在同一时间戳刻度；
- 三项同时失效时，旧快照会覆盖用户刚重新导入的凭据，导致登录态凭空失效。

修复：改用 SHA-256 内容摘要比对（`fileDigest`）。新增 `TestDurableJarPersistDetectsSameSizeReplacement`（等长替换必须被识别）与 `TestDurableJarPersistSucceedsWhenUnchanged`（内容未变时必须正常写入），连续 5 次运行全绿。

## 5. 变异自测（验证断言有效性）

| 变异 | 注入位置 | 捕获结果 |
| --- | --- | --- |
| 把业务 400 误计入账号失败 | `isAccountFailure` 增加 `status == 400` | 3 个用例同时 FAIL（`TestIsAccountFailureClassification` / `TestNoteAccountFailureIgnoresBusiness400` / `TestRunWithFailoverStopsOnBusiness400`） |
| 去掉删除前 `confirm` 闸门 | `panel.html` 卡片点击处理 | 2 项断言 FAIL（「删除前弹出确认」/「取消确认不发 /delete」） |

变异后源码已恢复并复跑全绿。

## 6. 复现命令

```bash
# 1) 影子构建 + 全部单测（无网络）
python scripts/cgo-shim-build.py doubao

# 2) 面板 DOM 回归
node test/doubao/panel_accounts_repro.mjs
node test/doubao/panel_import_repro.mjs

# 3) 真实上游全量（需 doubao/.local_cookie.txt）
DOUBAO_LIVE_TEST=1 GOFLAGS=-v python scripts/cgo-shim-build.py doubao
```

## 7. 已知边界

1. **真实生视频未验证**：受上游限流阻断，仅解析层由单测覆盖。
2. **多账号真实轮换未验证**：仅有一个真实账号，换号链路用「httptest 上游 + 双账号文件」验证（429 → 换号 → 成功）；真实双账号并存与互踢风险（GAP-004）仍未观测。
3. **冷却状态不持久化**：仅存内存，进程重启即清空。这是有意设计——冷却属运行期路由优化，不是账号状态；跨重启的账号故障由宿主侧 auth 状态承担。
4. **无「测试」标签**：与 workbuddy 不同，本插件没有定时探活，冷却只由真实请求失败驱动。
5. **限流为上游账号级**：连续约 6 次真实请求后触发 `710022004`，带 `shark_admin / verify / semantic_reasoning` 风控细节；插件侧已按阶梯冷却处理，但无法绕过。
6. **脱敏**：本文档不含 Cookie、`sec_user_id` 原值、会话 id 原值或手机号；日志仅输出长度与状态码。
