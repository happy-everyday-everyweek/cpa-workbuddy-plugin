# Changelog

## 0.2.2-verify2 (2026-10-18)

### 验证

- 发布链路提速 TASK-005 演练版本：验证「下载校验 → publish → 单次提交」流程，无功能改动。

## 0.2.1 (2026-10-18)

### 新增

- **积分余额与过期提醒**：账号卡展示可用积分、额度池、消耗进度条与即将过期
  明细；汇总卡新增「可用积分」「即将过期」两格。数据来自
  `GET /api/marketplace/trade/credit/balance`（余额）与
  `.../balance/detail?account_type=2&display_status=2`（逐笔过期明细）。
  实测生产账号：总额 3000 / 可用 3000 / 即将过期 1500，与官网订阅页一致。
- **面板「刷新积分」按钮**：`GET /credits?auth_id=&force=1` 跳过 5 分钟缓存
  强制查询，只重绘该卡片。

### 修复

- **续期此前不是自动的**：宿主自动刷新循环的 `nextRefreshCheckAt` 在
  `NextRefreshAfter` 为零时会回退到 `refresh_interval_seconds` 与
  `ProviderRefreshLead`，而扣子不在宿主内置的 refresh lead 注册表里
  （只有 codex / claude / antigravity / kimi / xai）。插件从未下发该字段，
  导致账号永远排不进刷新队列——生产实测 24 小时内 0 条 auto-refresh 日志，
  三次续期全部来自手动点击。现于登录/刷新下发的 `AuthData.Metadata` 补
  `refresh_interval_seconds`。
- 默认续期巡检间隔由 6 小时改为 **4 小时**，与其它插件一致。

### 说明

- 积分查询失败时面板显示「—」与错误原因，不会把未知渲染成 0。
- 明细接口失败只影响过期提示，余额照常展示。

## 0.2.0 (2026-10-18)

### 新增

- **模型动态发现**：声明 `model_provider` 能力并实现 `model.static` /
  `model.for_auth`。目录来自两个上游端点——`POST /v1/model/list`（按
  `model_type` 逐类拉取 `llm` / `image_generation` / `video_generation` /
  `tts` / `asr` / `embedding`）与
  `POST /api/coze_claw/resource/get_multimodal_model_catalog`（生视频完整族
  只在这里）。实测单账号 9 个模型：对话 3 / 图像 2 / 视频 1 / 嵌入 1 / 语音 2。
  管理台 `auth-files` 的「支持的模型」弹窗从此不再显示「该凭证暂无可用模型」。
- **面板「测试模型」**：新增 `GET /models`（按账号的模型下拉数据源，失败时
  回落内置清单并在 `notice` 说明原因）与 `POST /test-active`（认证层探活：
  校验登录态 + 模型可见性 + 账号归属）。真实推理链路需 `claw_id` /
  `session_id`，属 CYCLE-01。
- **面板按 workbuddy 风格重做**：账号卡（状态徽章 / 身份字段 / 操作按钮组）、
  顶部筛选标签与计数、账号汇总卡、搜索、全部启用/禁用、删除二次确认、
  右上角滑入通知、跟随宿主主题（含暗色）。
- 账号时间统一按 `Asia/Shanghai` 展示为 `YYYY-MM-DD HH:mm:ss`。

### 变更

- 面板移除「授权扣子账号」引导区：授权统一走宿主「添加账号」弹窗（OAuth
  桥接页），面板只承担账号运维。
- `model.static` 不再在宿主启动期发起网络请求，只返回内置种子目录；
  真实目录由 `model.for_auth` 按账号、按需提供——模型注册在宿主认证加载
  路径中同步执行，启动期打上游会拖慢宿主。
- 模型发现改为并发拉取各类型端点，单请求 8 秒超时；缓存按账号分桶，
  多账号不再互相覆盖。

## 0.1.3 (2026-10-18)

### 修复

- **授权脚本转义的真正根因在宿主侧**：宿主回写 `ManagementResponse.Body`
  时会再做一次带 HTML 转义的 JSON 序列化。实测证据：`/renew` 的错误文案
  把入参 `x<y>z'w` 回显成 `x&lt;y&gt;z&#39;w`——插件侧关闭自己的转义无效。
  改为**以 base64 下发脚本**（base64 不含 HTML 特殊字符），面板端
  `atob` + `TextDecoder` 解码还原；响应带 `encoding: "base64"` 标识。
- 0.1.1 / 0.1.2 两次修复均无效（分别关信封层与 payload 层转义），
  本版是根因修复。

## 0.1.2 (2026-10-18)

### 修复

- **面板授权脚本仍被 HTML 实体化**（0.1.1 修复不完整）：0.1.1 只改了
  `okEnvelope`，但 `handleLoginStartFromPanel` 在更早一层用 `json.Marshal`
  生成 payload，转义发生在那一步，信封层再关转义也无济于事。改为
  `marshalNoHTMLEscape`。生产复验：0.1.1 的 `login-start` 仍含 26 处
  `&#39;` / 2 处 `&gt;` / 0 处 `=>`。
- 补 `TestLoginStartFromPanel_ScriptNotEscaped` 直接打真实入口（此前只有
  信封级用例，覆盖不到这条路径）。

## 0.1.1 (2026-10-18)

### 修复

- **授权脚本被 HTML 实体化**：`okEnvelope` 原先用 `json.Marshal`，它默认开启
  HTML 转义，把脚本里的 `=>`、`'`、`&&` 变成 `=&gt;`、`&#39;`、`&amp;&amp;`，
  面板复制到的脚本无法执行。改用 `json.Encoder` + `SetEscapeHTML(false)`。
  生产实测证据：`login-start` 响应 774 字节中含 26 处 `&#39;`、2 处 `&gt;`、
  0 处 `=>`。

## 0.1.0 (2026-10-18)

CYCLE-00 首发：扣子（Coze）授权登录链路。

### 新增

- **授权登录**：面板生成一次性同源授权脚本，用户在扣子页面执行后把
  `x-tt-token` 交给插件；插件用该令牌调 `passport/token/beat/v2/` 换取会话
  Cookie 并落盘。全程不读浏览器 Cookie 库、不内嵌浏览器、不注册自定义协议。
  令牌经 URL fragment + 同源请求头两段式回传：fragment 不发给服务端，
  因此不会落进宿主的访问日志。
- **登录态判据**：`passport/account/info/v2/` 的 `message=success` 且
  `sec_user_id` 非空双条件判定；判据不通过不落盘。
- **Cookie 持久化**：按 `(name, domain, path)` 三元组存储，保留同名分域
  Cookie（真实端点下发的 13 条中 `uid_tt` / `uid_tt_ss` / `session_tlb_tag`
  各出现两次）。
- **滚动续期**：`passport/token/beat/v2/` 是双向换发端点——带 Cookie 换
  `x-tt-token`，带 token 换新 `Set-Cookie`；插件据此实现
  「Cookie → 新 token → 新 Cookie」循环，无需用户反复授权。
- **账号管理**：列表、删除、启用、禁用、手动续期；面板展示授权时间、
  最近续期时间与登录态状态。
- **宿主集成**：`auth.parse` / `auth.login.start` / `auth.login.poll` /
  `auth.refresh` 全链路实现，宿主「添加账号」弹窗可正常拉起。

### 已知限制

- 只实现认证链路（CYCLE-00）：**未声明 executor / model_provider**，
  对话能力（CYCLE-01）尚未实现。
- 授权需用户在扣子页面手动执行一次脚本（因官方 `redirect_uri` 协议白名单
  只认 `coze-desktop:` 系列，插件无法作为 OAuth 回调接收方）。
- 续期巡检尚未接入后台定时器，当前由宿主 `auth.refresh` 与面板手动触发。

### 实测依据

- 真实账号端到端验证：令牌 355 字符 → 13 条 Set-Cookie → `sec_user_id`
  76 字符 → 续期闭环成立。见
  `doc/5-tests/2026-10-17_235500_Coze授权链路端到端验证.md`。
