# coze-provider

CLIProxyAPI（CPA）插件：把扣子（Coze）账号接入 CPA，提供**授权登录**、
**会话续期**与**对话转发**能力。

> 当前版本 0.3.0：CYCLE-00（认证）与 CYCLE-01（对话链路）已打通。
> 插件声明 `model_provider` + `executor`，对话走 WebSocket 真流式。

## 对话链路

```text
OpenAI 请求
  → claw/info({})            → claw_id
  → session/get_main         → 固定主会话 session_id
  → get_frontier_key         → access_key + device_id
  → 建立 frontier WebSocket  （必须先于下一步，否则丢首帧）
  → message/chat             → 用户消息 message_id（异步，无回复内容）
  → WS 收 reply 帧
       event_delta → 增量 → chat.completion.chunk
       event_final → 收尾
```

### 关键约束（实现必读）

- **WS 连接参数**：`wss://frontier.coze.cn/ws/v2`，`aid=751148`、`fpid=1289`、
  `xsack=0`、子协议 `pbbp2`。
  ⚠️ `/api/fullstack_chux/v1/frontier/connection-params` 返回的
  `aid=578287` / `message_service_id=33555616` 是**另一条通道**，
  用错只能收到 `hi` 心跳、收不到任何业务帧。
- **业务 service 编号是 `33555657`**，不是 `33555616`。
- **模型不由请求决定**：`message/chat` 不接受模型参数，实际模型由扣子侧
  配置（页面右下角选择器）控制。请求里的 `model` 只用于响应回显。
- **多轮上下文有损**：claw 只接受单条文本，OpenAI 的 system / 历史轮次会被
  折叠成「角色: 内容」逐行拼接，角色边界不作为独立字段传给上游。
- **增量必须逐帧下发**：上游 `content.message_data.reply.content` 是 delta，
  可累加；合并多帧下发会让客户端「卡一下然后蹦出一大段」。
- **去重键是 `turn_id + seq_num + phase`**：同一轮的 `event_start` 与首个
  `event_delta` 共享同一个 `id`，只按 id 去重会丢掉真正的首个增量。
- **收尾判据是 `phase == event_final`**：没有 `is_finish` 字段。

## 为什么不是一键授权

扣子没有开放给第三方的一键授权入口（2026-10-17 桌面端 1.1.45 源码取证）：

| 入口 | 阻断原因 |
|---|---|
| `/sign/oauth` | `redirect_uri` 硬编码为 `www.coze.cn/auth/callback`，站外地址被丢弃 |
| `/desktop-login` 的 `redirect` | 登录令牌只流向 `redirect_uri`，`redirect` 不承载令牌 |
| `/desktop-login` 的 `redirect_uri` | 协议白名单硬编码 `coze-desktop:` / `-beta:` / `-dev:` |

插件因此采用「同源授权脚本」：用户在扣子网页完成官方登录后，执行插件给出的
一次性脚本取 `x-tt-token`，再由插件换取会话。**不读浏览器 Cookie 库、
不内嵌浏览器、不注册自定义协议。**

## 授权流程

```text
面板点「生成授权脚本」
  → 插件签发 state（15 分钟有效）并返回可复制脚本
  → 用户在 https://www.coze.cn 完成官方登录
  → 在 Console 粘贴脚本执行
  → 脚本在扣子同源上下文取 x-tt-token，跳转插件回调页
  → 插件 POST /passport/token/beat/v2/（x-tt-token）→ 13 条 Set-Cookie
  → GET /passport/account/info/v2/ → sec_user_id 非空 → 落盘
```

## 续期

`passport/token/beat/v2/` 是**双向**换发端点：

| 调用方式 | 响应 |
|---|---|
| 带会话 Cookie | `x-tt-token` 响应头（Set-Cookie 0 条） |
| 带 `x-tt-token` | `Set-Cookie`（13 条） |

因此会话可无限滚动续期，不存在 Cookie 快照硬失效。续期有两个触发源：

- **插件内定时器**：默认每 4 小时（`renew_interval_seconds`）遍历所有启用
  账号执行一轮续期并落盘，插件热重载后立即先跑一轮；
- **面板「续期」按钮**：对单个账号立即执行一轮。

宿主 `auth.refresh` 也会走同一套续期内核，但扣子不声明 scheduler 能力，
宿主自动刷新循环对本插件只重排不执行（`handleDueAuth` 的 `exec == nil`
分支），因此定时续期由插件自己负责。

## 配置

| 键 | 默认 | 说明 |
|---|---|---|
| `timeout_seconds` | 30 | 上游请求超时（秒） |
| `renew_interval_seconds` | 14400 | 定时续期间隔（秒），默认 4 小时 |

## 管理接口

前缀 `/v0/management/plugins/coze-provider`：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/accounts` | 账号列表（含登录态状态） |
| POST | `/login-start` | 生成一次性授权会话与脚本 |
| POST | `/renew` | 手动续期 |
| POST | `/delete` / `/enable` / `/disable` | 账号管理 |

免密钥资源页：`/v0/resource/plugins/coze-provider/panel`、`/login`、
`/login/callback`。

## 本地验证

```bash
python scripts/cgo-shim-build.py coze
```

## 协议依据

- 对话链路协议（含真实帧 schema）：`doc/5-tests/2026-10-18_164500_Coze对话链路协议补全.md`
- 端到端实测：`doc/5-tests/2026-10-17_235500_Coze授权链路端到端验证.md`
- 授权入口裁定：`doc/5-tests/2026-10-17_234500_CozeDesktopLogin授权入口复验.md`
- 实施总览：`doc/3-实施/2026-10-17_230000_CozeProvider插件立项_实施总览.md`
