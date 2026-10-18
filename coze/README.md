# coze-provider

CLIProxyAPI（CPA）插件：把扣子（Coze）账号接入 CPA，提供**授权登录**与
**会话续期**能力。

> 当前版本 0.1.0 只实现认证链路（CYCLE-00）。对话转发（CYCLE-01）尚未实现，
> 因此插件暂不声明 executor / model_provider。

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

宿主 `auth.refresh` 也会走同一套续期内核，但扣子不声明 executor 能力，
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

- 端到端实测：`doc/5-tests/2026-10-17_235500_Coze授权链路端到端验证.md`
- 授权入口裁定：`doc/5-tests/2026-10-17_234500_CozeDesktopLogin授权入口复验.md`
- 实施总览：`doc/3-实施/2026-10-17_230000_CozeProvider插件立项_实施总览.md`
