---
schema_version: 1
template_version: implementation-overview-v1
doc_id: IMPL-QODERWORK-CHECKIN-CAMPAIGN-20261011
doc_type: implementation_overview
source_ids: [BUG-QODERWORK-CHECKIN-CAMPAIGN-20261011]
status: completed
version: v1.1
complexity: L1
current_slice: TASK-001 至 TASK-008 发布部署与生产验收已全部完成
updated_at: 2026-10-11 08:30:00
reader_level: business_general
writing_style: plain_chinese
appendix_policy: preserve_existing_or_one_terminal_appendix
style_regression: required_after_tests
---

# QoderWork CN 签到改走 campaigns API 修复实施总览

结论：CN 每日签到端点 `daily-check-in/{status,claim}` 已在生产废弃（status 恒 DISABLED、claim 恒 409 AlreadyExists），插件未同步迁移导致「返回刷新成功但积分不变」；本轮按 qoder-ai 国际版 0.1.4 同源方案，把 qoderwork 签到改走 campaigns（活动）API 并补齐 Cosy 客户端头；范围：`main.go`、`billing.go`、`checkin.go`、`management.go`、`panel.html`、新增 `checkin_campaigns_test.go`、版本与文档；非范围：推理链路、模型映射、账号调度与生命周期、宿主 ABI；完成标准：本地 build/vet/test 全绿 + 双反证 + 发布链闭环 + 生产账号 `u09a5b6ab` 真实签到 +100。

## 1. 当前计划最终方案的简要说明

- 推荐方案一句话结论：把 qoder-ai 0.1.4 已验证的 campaigns 方案移植到 qoderwork（端点 + Cosy 头 + 活动挑选 + claim），随后走完整发布链并做生产行为验收。
- 主落点 / 主路径：`qoderwork/main.go`（端点常量）、`qoderwork/billing.go`（campaigns 头/解析/领取）、`qoderwork/checkin.go`（流程与统计）、`qoderwork/panel.html`（文案）。
- 为什么走这条路线：同产品线国际版已有铁证与已发布先例，直接复用同一修复口径最小、风险最低；不改外部接口与调度。

## 2. Agent 对当前问题的理解

- 问题 / 目标：生产账号 `u09a5b6ab` 具备签到资格（客户端显示 +100），但签到后积分不增加，只等到「后台刷新」提示。
- 本轮计划范围：CN 签到端点迁移、请求头契约、活动挑选与领取、回归测试、版本与文档、发布与生产验收。
- 明确不在范围：推理链路、模型映射、账号调度与生命周期、宿主 ABI。
- 当前优先闭环：发布链（commit → push → CI → assets → registry → 远端验证）+ 生产部署与行为验收。
- 关键假设 / 待确认点：迁移后若账号当日未领，claim 应返回 `status=CLAIMED` 并使积分 +100；若已领则 `replayed=true` 归一为已签到。
- unresolved_decisions：无。

## 3. 实施周期与任务拆分

- 周期一：根因修复与本地验证（已完成）
  - `TASK-001` 取证与根因冻结：生产 token 实测旧端点 409/DISABLED；CN 客户端 v0.4.4 确认走 campaigns + Cosy 头。
  - `TASK-002` 端点迁移：`main.go` 增 `endpointCampaigns` / `endpointCampaignClaim`。
  - `TASK-003` billing 改造：`campaignHeaders` + campaigns 解析 + `pickCheckinCampaign` + `performCheckinCall(sa, campaignID)`。
  - `TASK-004` 流程与统计：`checkin.go` 传参、`!Active` 分支、`no_activity`；`management.go` 加 `campaign_id`。
  - `TASK-005` 面板文案：`panel.html` 成功提示/无活动文案/批量统计。
  - `TASK-006` 回归与反证：新增 `checkin_campaigns_test.go`，cgo-shim 全绿 + 双反证 + 提交态独立编译。
- 周期二：发布与生产验收
  - `TASK-007` 发布链：commit → push → CI → assets → registry → 远端验证。
  - `TASK-008` 生产部署与行为验收：plugin-store install 0.9.27，账号 `u09a5b6ab` 签到 +100。

## 3.1 最小任务清单

| 任务 ID | 唯一目标 | 状态 |
| --- | --- | --- |
| `TASK-001` | 取证与根因冻结：CN 签到端点废弃，真实入口为 campaigns | completed |
| `TASK-002` | 端点常量迁移 | completed |
| `TASK-003` | billing campaigns 头/解析/领取 | completed |
| `TASK-004` | checkin 流程/统计 + campaign_id | completed |
| `TASK-005` | 面板文案与批量统计对齐 | completed |
| `TASK-006` | 回归测试 + 双反证 + 提交态独立编译 | completed |
| `TASK-007` | 发布链：commit、push、CI、assets、registry、远端验证 | completed |
| `TASK-008` | 生产部署与账号 u09a5b6ab 行为验收 | completed |

## 4. 真实测试安排

- 测试入口：`python scripts/cgo-shim-build.py qoderwork`（build + vet + test）。
- 覆盖项：端点契约、Cosy 头契约、活动挑选（命中/已领取/窗口外/无活动/非 CREDITS）。
- 反证：① 破坏 `ClaimStatus=="CLAIMED"` → `TestPickCheckinCampaign_ClaimedInWindow` 必失败；② 删除 Cosy 头 → `TestCampaignHeaders_CosyContract` 必失败。
- 通过标准：本地全绿 + 双反证 + 提交态独立编译全绿 + 生产真实签到 +100。

## 5. 任务完成条件 / 停止条件 / 最大推进边界

- 完成条件：本地全绿 + 双反证 + 发布链闭环 + 生产账号行为验收（积分 188 → 288）。
- 停止条件：若活动未开放或账号已领，如实返回；不伪造成功。
- 最大推进边界：只改 qoderwork 签到链与面板文案，不扩散到推理/调度/宿主。
