# Changelog

## 0.3.2 (2026-10-18)

### 修复

- **WS 握手缺会话 Cookie，只能收到心跳**。0.3.1 修好序列化后，生产流式
  虽能返回完整正文，但耗时 92 秒且正文在最后一帧一次性到达——首帧之后
  91 秒没有任何增量，正好卡在首字超时（90s）上，实际走的是轮询兜底。
- **根因**：frontier 网关对「无 Cookie 的握手」照样返回 `101 Switching
  Protocols`，但只推 `hi` 心跳，一帧业务数据都不给。生产日志里
  `frontier 已连接` 到 `连接中断` 相隔 91 秒、中间零业务帧，与此吻合。
- **对照实验（同一生产账号，仅差一个 Cookie 头）**：
  - 不带 Cookie：6 帧，全部是 `hi`；
  - 带 Cookie：25 帧，其中 21 条二进制业务帧，在 0.34s–4.25s 陆续到达。
- **修复**：`frontierClient` 增加 `cookieHeader`，`runOnce` 握手时写入
  `Cookie` 请求头；`runStream` 从账号取 `sa.CookieHeader` 传入。

### 测试

- 新增 `TestFrontierHandshakeCarriesCookie`：假服务端记录握手 Cookie 并断言。
  变异删除该请求头后用例精确失败。

## 0.3.1 (2026-10-18)

### 修复

- **流式请求 500：宿主 RPC 无法序列化 channel**。0.3.0 的
  `handleExecStream` 把 `Chunks` 填成 `chan pluginapi.ExecutorStreamChunk`
  直接返回，而宿主 `rpcExecutorStreamResponse.Chunks` 是 slice，RPC 桥在
  JSON 序列化阶段就失败，客户端拿到
  `{"error":{"message":"json: unsupported type: <-chan pluginapi.ExecutorStreamChunk"}}`，
  生产实测 0 字节正文、0 个 chunk。
  改为宿主提供的异步路径：带 `stream_id` 时立即返回响应头，由后台协程用
  `host.stream.emit` 逐帧推送、`host.stream.close` 收尾；无 `stream_id`
  时退化为同步收集，一次性返回 slice 形态的完整 chunk 列表。
- **响应头补 `X-Accel-Buffering: no`**。缺这一项时反向代理会攒够缓冲才下发，
  真流式在客户端表现为「长时间无输出后一次性蹦出全文」。

### 测试

- 新增 `TestStreamResponseSerializable`：锁定「流式响应必须可 JSON 序列化」
  契约。变异回 channel 形态后该用例精确复现生产报错文案。
- 新增 `TestStreamHeadersCarryNoBufferHint`：锁定 `text/event-stream` 与
  `X-Accel-Buffering: no`。

## 0.3.0 (2026-10-18)

### 新增

- **对话链路打通（CYCLE-01 / TASK-004）：声明 executor 能力，支持真流式**。
  此前插件只暴露模型目录、不声明 executor，推理请求不会路由进来。
  现在完整链路为：`claw/info` 取 `claw_id` → `session/get_main` 取主会话 →
  `get_frontier_key` 取 WS 凭证 → 建立 frontier WebSocket →
  `message/chat` 异步提交 → 从 WS 收增量 → 转 `chat.completion.chunk`。
- **`frontier_frame.go`：protobuf 帧编解码**。上游帧是 12 字段 protobuf
  （`SeqID`/`LogID`/`service`/`method`/`headers`/`payloadEncoding`/
  `payloadType`/`payload`/`LogIDNew`/`serverTiming`/`msgID`/`frameType`），
  没有 `.proto` 可用，字段号由客户端 SDK 反编译并用真实帧校准。
  解码按 tag 分派并跳过未知字段，上游加字段不会让链路当天断掉。
- **`frontier.go`：WebSocket 客户端**。连接参数 `aid=751148`、`fpid=1289`、
  `xsack=0`、子协议 `pbbp2`；10 秒文本心跳 `hi`；读超时兜底；
  指数退避重连（1s 起、上限 30s，连上即重置）。
- **`claw.go`：claw HTTP 协议层**。四个端点统一错误判定
  （HTTP 200 + `code` 表达业务结果，只看状态码会把失败判成成功）。
- **`convert.go` / `executor.go`：OpenAI 协议转换与执行器**。
  多轮 messages 折叠成单条文本；增量逐帧下发（不合并）；按
  `turn_id + seq_num + phase` 去重；`event_final` 收尾；
  首字超时降级到 `message/list` 轮询兜底。

### 修复

- **重连延迟**：初版心跳协程只在 ticker 触发时检查状态，服务端断开后
  要空等一个完整心跳周期（10 秒）才重连。改为读循环结束时立即取消
  心跳协程，重连不再有额外延迟。
- **增量被合并**：初版按「累积正文差值」下发，两帧到达间隔小于轮询
  间隔时会被合并成一大块，客户端表现为「卡一下然后蹦出一大段」。
  改为维护待下发队列，每个上游帧独立 emit 一次。

### 说明

- **模型选择不由插件控制**：`message/chat` 不接受模型参数，实际模型由
  扣子侧配置决定（页面右下角选择器）。请求里的 `model` 只用于响应回显。
- **多轮上下文是有损的**：claw 只接受单条文本，OpenAI 的 system / 历史
  轮次会被折叠成「角色: 内容」逐行拼接，角色边界不作为独立字段传给上游。
- **`message/list` 只作兜底**：它返回已落库的完整消息（实测约 4 秒延迟），
  没有增量语义；只在 WS 首字超时（90 秒）时启用。

## 0.2.2 (2026-10-18)

### 修复

- **定时续期此前只是「排进队列」，并没有真正执行**：0.2.1 补的
  `refresh_interval_seconds` 只能让宿主把账号排进自动刷新队列，而
  `auto_refresh_loop.handleDueAuth` 在 `exec == nil` 时只做
  `l.upsert(authID, now.Add(interval))` 空转重排、不调用 `auth.refresh`；
  宿主 `RegisterExecutors` 只注册声明了 executor 能力的插件，扣子不声明
  executor，因此宿主的 4 小时排期对本插件永远不产生续期。
  现改为**插件内自建续期定时器**（`renew_loop.go`）：默认每 4 小时遍历所有
  启用账号执行 `renewSession` + `applyRenewal` 落盘；插件热重载后立即先跑
  一轮；`reconfigure` 改动间隔会立即重排，无需重启；`Shutdown` 时关闭停止
  信号，旧实例循环立刻退出。
- 续期失败只记日志，不改账号启用状态——临时网络问题不该把账号下线，
  否则它会在下一轮彻底失去自愈机会。
- 续期动作经宿主 `host.log` 留痕，`docker logs` 可直接看到每轮账号数、
  成功数与失败原因；此前后台续期没有任何日志出口，无法判断是否真的发生。

### 说明

- 单轮并发上限 4 个账号，避免账号多时一轮拖到几分钟，也避免触发上游风控。
- 缺少物理路径的账号会被跳过并记一条 warn：续期结果无处落盘，继续跑只会
  白耗上游请求，这种情况通常意味着宿主账号发现链路异常。

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
