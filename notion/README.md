# notion-provider

CLIProxyAPI（CPA）插件：把 **Notion AI**（`app.notion.com/ai`、`app.notion.com/chat`）
的模型能力接入 CPA，对外提供 OpenAI 兼容的 `/v1/chat/completions`。

> **非官方、逆向实现，随时可能失效。**
>
> - 本插件不隶属于 Notion，也未获得其授权；接口来自网页端行为的逆向分析。
> - 使用本插件**可能违反 Notion 服务条款**，存在账号被限制或封禁的风险，请自行评估。
> - Notion 把 AI 权限绑定到**登录时的出口 IP**：Cookie 复制到异地会得到 `403`
>   或流内 `trust-rule-denied`。需要跨机部署时，请为账号配置与登录地一致的出口代理。
> - 上游协议（`runInferenceTranscript` 的 patch 流）未公开且可能随时变更；
>   变更后插件会退化为空正文报错，不会静默返回假结果。

## 能力

- **模型目录**：静态注册 39 条 Notion 模型代号（36 条可用），并支持运行时按账号动态发现。
- **对话转发**：`/v1/chat/completions` 的流式与非流式两条路径，走真实的 NDJSON 增量。
- **多工作空间**：Notion 一个账号可同时挂商业版与免费版空间，额度完全独立；
  导入时枚举全部空间并记录各自 `premiumCredits`，可随时切换。
- **多账号**：阶梯冷却（30s 起、每次翻倍、封顶 10 分钟）+ 同请求换号。
- **会话复用**：同一账号+空间+模型复用上游 `threadId`，多轮上下文自动延续。
- **Cookie 滚动**：上游下发的 `token_v2` 等会话 Cookie 会写回账号文件，重启后仍有效。

## 安装

1. 在 CPA 插件市场安装 `notion-provider`（或 `plugin-store install notion-provider`）。
2. 打开管理面板：`/v0/management/plugins/notion-provider/panel`。
3. 从浏览器复制 Notion 的 **Cookie header**（至少包含 `token_v2` 与 `notion_user_id`），
   粘贴到面板「导入」框。
4. 导入时会自动探测工作空间并落盘；选择要使用的工作空间后即可在 CPA 里调用
   `notion/<模型代号>`。

## 配置

```yaml
notion-provider:
  timeout_seconds: 30          # 非流式上游请求超时
  stream_timeout_seconds: 300  # 单轮生成总时长上限
  client_version: "23.13.20260313.1423"
  space_id: ""                 # 指定默认工作空间（空 = 自动选额度最高）
  retry_on_4xx: 3              # 401/403/404/405 的同请求换号预算（0 = 关闭）
  cooldown_seconds: 30         # 冷却阶梯基数
```

`retry_on_4xx` 的缺省键**不会**在 reconfigure 时重置：这是 kill switch，
无关配置变更不应让已关闭的换号策略悄悄复活。

## 协议要点（实现参考）

- 端点：`POST /api/v3/runInferenceTranscript`，响应为 NDJSON patch 流。
- 必须携带 5 个顶层字段：`threadParentPointer`、
  `supportsCustomAgentNudgeTranscriptStep`、`isUserInAnySalesAssistedSpace`、
  `isSpaceSalesAssisted`、`patchResponseVersion: 2`；缺任一会只返回 `patch-start`。
- user 步的 `value` 是**二维字符串数组** `[["文本"]]`，不是对象数组。
- `agent-inference.value` 是异构数组（`thinking` / `text`），正文不固定在索引 0。
- 段落索引必须按快照 `id` 对齐，不能用累积计数器。

## 许可证与致谢

本插件为独立重写实现，未复制任何第三方项目代码。协议分析过程中参考了以下开源项目的
**公开行为描述**（不含代码复制）：

- `SleepingBag945/notion_manager`（CC BY-NC-SA 4.0）——仅参考其端点与字段命名。
- `maverickxone/notion2api`（无 LICENSE）——仅参考其公开 issue 中的失败现象。
- `ArthurMonteiro08586/notion-agent-gateway`——模型代号表来源（静态 fallback）。
