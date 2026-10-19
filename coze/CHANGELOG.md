# Changelog

## 0.4.6 (2026-10-19)

### 修复

- **一次超长请求会让整个账号池不可用**。生产实测：`doubao-seed-2-0-lite`
  连续返回 `503 auth_unavailable: no auth available`，宿主日志里
  两个账号各失败一次后全部短路（3–5ms 返回），错误文案都是
  `提交消息失败：数据库错误（code=1000003）`。
  根因是上游 `message/chat` 对 `content` 有 65535 字节硬上限
  （二分实测：65535 成功、65536 失败），越界时返回的是
  `1000003 数据库错误` 而不是参数错误；插件原样上抛后，宿主按 5xx
  判定为账号级故障，把两个账号依次打入冷却，之后所有请求都因
  「无可用账号」被拒。
  现在在同步入口按字节预检，超限直接返回 **413** 请求级错误：
  不再触达上游，宿主也不会冷却账号或换号重试。
  中文按 UTF-8 字节计（一个汉字 3 字节），不按字符数。
- 插件错误信封支持 `http_status`：只有带 4xx 状态码的错误才会被宿主
  归为请求级故障。其余上游失败（如积分耗尽 `1000521`、会话失效
  `700012006`）仍走原有账号级通道，换号与冷却语义不变。

## 0.4.5 (2026-10-19)

### 修复

- **失效账号在面板上仍显示「可用」**。生产实测：三账号中两个已被上游
  判定失效（续期返回「会话过期，请重新登录」、积分端点返回
  `code=700012006`），面板却全部显示「可用」，只有一个能真正积分成功。
  根因是登录态判据只看本地 Cookie 的 `Expires`，而扣子的会话 Cookie
  根本不下发 `Expires`，本地判活永远通过。
  现在新增 `auth_invalid` 标记：续期被上游明确判为会话失效时落盘置位，
  面板据此显示「待重新授权」；续期成功即清除。
  瞬态错误（超时、限流、5xx）不置位，避免误报。
- 失效判定只认上游会话类信号（「会话过期」「登录认证失败」
  `700012006`、`Login verification is invalid`），不按任意错误一刀切。

## 0.4.4 (2026-10-19)

### 修复

- **上游失败被当成「成功但 0 输出」返回**。生产实测：账号积分耗尽时
  上游 `message/chat` 返回 `code=1000521`，插件却仍补一个空正文的
  `finish_reason=length` 终止帧；宿主把「首个 payload」当作流已成功的判据，
  于是整轮请求被记为成功、下游既看不到错误也不计费（new-api 日志
  `Tokens: -`、费用 `$0`，CPA 面板输出 0、耗时仅 6~12ms）。
  现在首帧延迟到真正有内容时再补，一个 payload 都没产出时改走
  `stream.emit` 的 `error` 字段上报，错误文案原样透传给下游；
  正文已泄漏后失败仍用 `length` 收尾，避免作废已渲染内容。
- **收尾但无正文/图片时不再返回成功**。`waitForReply` 在
  `event_final` 到达且兜底轮询也拿不到内容时返回错误，
  不再让空响应当成功响应流到下游。

## 0.4.3 (2026-10-19)

### 诊断

- 补齐空正文路径的日志：`isFinished` 已收尾但正文为空时记录
  `message_id` 与兜底轮询失败原因。0.4.2 只覆盖 `runStream` 返回错误，
  而实测空正文走的是「正常返回但正文为空」，因此需要两条新日志才能定位。

## 0.4.2 (2026-10-19)

### 诊断

- 对话执行失败时输出一条 warn 日志（模型、prompt 长度、已收正文长度、
  收尾原因、错误原文）。空正文 + `length` 收尾在下游表现为「成功但 0 输出」，
  没有任何错误信息，只能靠这条日志定位。

## 0.4.1 (2026-10-19)

### 修复

- **下游用量全部记零**。生产面板显示 coze 请求「输入 0 / TPS 0.00」，
  new-api 通用日志里该模型 `Tokens` 恒为 `-`、费用 `$0`，无法计费。
  根因是插件从未在响应里输出 `usage` 字段：宿主只从 chunk/响应体的
  `usage` 取 token 数，扣子上游又没有 token 计数端点、WS 帧里也不带用量，
  于是整条链路拿到的都是零值。
  现在流式在**终止帧**、非流式在响应体输出 OpenAI 规范的 usage，
  按字符特征估算（CJK ≈ 1 字 1 token，拉丁 ≈ 4 字符 1 token），
  并带 `estimated: true` 便于区分估算值与上游实测值。

### 测试

- 新增 `TestBuildUsageEstimatesTokens`（估算值与 total 自洽）、
  `TestUsageOnlyInTerminalChunk`（中间帧不带 usage、终止帧必须带）、
  `TestBuildCompletionCarriesUsage`（非流式响应带非零 usage）。
  分别变异移除分片与非流式两条挂载路径，两个用例精确失败。

## 0.4.0 (2026-10-19)

### 新增

- **对话内生图能力打通**。此前生图回复只能拿到 `[图片](computer:///...)` 占位
  文本，图片本身被整条丢弃。现在图片会在正文之后以 Markdown 图片语法下发，
  流式收尾补一帧、非流式拼入响应体，下游客户端可直接预览。
  契约按 2026-10-18 真实账号实测确定：图片地址在消息的**顶层 `files` 数组**
  （`file_url` 为 `static.coze.site` 签名直链，有效期 30 天），不在
  `content_v2` 的内容分片里；生图约 18~25 秒原子出现，无中间态可流。
- `clawFile` / `clawMessage.Files` 解析附件；`frontierRawMessage.Files`
  收取 WS 帧顶层附件（`content_v2` 的 image/file 分片作为兼容路径保留）。
- 兜底轮询改用 `findReplyMessageFor` 取整条消息，避免只看内层 payload 漏图。

### 修复

- **非流式正文重复**。`execute` 原先把 `runStream` 的 emit 增量与返回值各算一次，
  响应体里出现两份正文。抽出共用执行体 `runConversation`，
  正文只取返回值，图片由调用方决定交付方式。
- 图片去重：同一地址在 `event_delta` 与 `event_final`（或 `files` 与
  `content_v2` 分片）重复出现时只下发一张。

### 测试

- 新增 12 个用例覆盖附件解析、顶层 `files` 取址、跨来源去重、
  空 url 守卫、收尾下发、非流式去重、兜底轮询取图。
- 夹具 `replyFrameWithImages` 改为真实上游形态（图片在顶层 `files`）：
  早期版本把它写成 `content_v2` 的 image 分片，导致「WS 取不到图」
  的单测完全不可见；变异回旧实现后 3 个用例精确失败。
- 新增 `live_image_test.go` / `live_download_test.go` / `live_seed_test.go`
  真实链路探针，仅在设置 `COZE_LIVE_COOKIE` 时执行，签名参数全部脱敏。

## 0.3.6 (2026-10-18)

### 修复

- **`/v1/responses` 与 new-api 转发请求必然失败**。生产日志大量出现
  `503 auth_unavailable ... last upstream error: 请求缺少 messages`。
  根因：`executorRequestBody` 优先读 `OriginalRequest`（客户端原始请求体），
  而宿主**只对 `Payload` 做协议翻译**。走 Responses 入口时原始体只有 `input`
  字段，没有 `messages`，插件在入口直接报错；该错误还会让宿主把账号标记为
  不可用，连带后续正常的 chat / completions 请求一起 503。
  修复为优先读 `Payload`，仅在为空时回退 `OriginalRequest`，与兄弟插件
  doubao 的取值策略一致。

### 测试

- `TestExecutorRequestBodyPreference` 改为锁定新优先级（Payload 优先）。
- 新增 `TestExecutorResponsesEntryParsesTranslatedPayload`：模拟 Responses
  入口（原始体只有 `input`、Payload 含 `messages`），验证能正确折叠成 prompt。
  变异回旧实现后两个用例精确复现生产报错文案「请求缺少 messages」。

## 0.3.5 (2026-10-18)

### 变更

- **面板「测试模型」升级为真实推理探活**。此前只校验登录态与模型可见性，
  测不出对话链路故障——本轮 0.3.0→0.3.4 的流式 500、空正文、多轮空回复
  都属于「认证层全绿但推理不通」的类型，面板却一直显示测试通过。
  现在 chat 模型会真的发一次极小推理（提示词「只回复两个字：可用」），
  走与用户请求相同的 executor 链路，能捕获只有对话链路才暴露的问题。
- 图片/视频模型仍只做前两层校验：它们不适用对话链路，且单次生成要数十秒。
- 探活结果的 `scope` 字段随之扩展为 `auth+models+inference`，
  面板文案同步改为反映真实覆盖范围。

### 修复

- **删除过期提示**：面板弹窗里「当前为认证层探活……对话链路将在后续周期
  接入」是 CYCLE-01 之前的说明，对话链路早已上线，该文案已不成立。

### 测试

- 新增 `TestRunActiveTest_InferenceSucceeds`（chat 模型走推理并成功）、
  `TestRunActiveTest_InferenceFails`（认证与模型都正常但推理失败须判失败）、
  `TestRunActiveTest_SkipsInferenceForImageModel`（图片模型不发推理）。
  变异移除推理分支后前两个用例精确失败。
- 推理探活抽成可注入变量 `inferenceProbe`，单测用假实现避免真实网络。

## 0.3.4 (2026-10-18)

### 修复

- **多轮对话返回空正文**。生产实测：单条 user 正常（65 字），带
  `assistant` 历史的多轮请求正文 0 字、`finish_reason=stop`。
- **根因**：`runStream` 用固定 `300ms` 猜 WS 是否连上再提交消息。
  网络慢时 chat 抢在连接之前发出；快速回复（多轮场景实测约 2 秒结束）
  的 `event_delta` 在连接就绪前就推完了，只收到重放的 `event_final`，
  而 final 的 delta 已被 0.3.3 清空 → 正文为空。
  WS 抓帧佐证：同一 prompt 上游确实推了 start / delta(28 字) / final 三帧。
- **修复**：
  - 新增 `frontierClient.WaitReady`，握手成功即关闭 `ready` 通道，
    调用方等真实就绪信号（上限为握手超时 10s），不再靠猜。
  - `waitForReply` 增加兜底：已收尾但正文为空时，按本轮 `message_id`
    去 `message/list` 取落库正文补齐。
  - `fallbackPoll` 改为按 `reply_to_message_id` 精确配对；原实现取
    「最新一条 reply」，可能把上一轮回复当成本轮结果。

### 测试

- 新增 `TestWaitReadySignalsAfterHandshake`（握手后立即就绪）与
  `TestEmptyReplyFallsBackToPolling`（只发 final 时靠轮询补回正文）。
  变异移除空正文兜底后，后者精确失败并在断言里复现空正文。

## 0.3.3 (2026-10-18)

### 修复

- **正文重复一遍：`event_final` 的累积全文被当成增量追加**。0.3.2 恢复真流式
  后，生产实测 39 个正文帧、正文 1156 字，其中末帧 578 字**恰好等于前文
  拼接**——同一段答案完整输出了两遍。
- **根因**：`content.message_data.reply.content` 在 `event_delta` 帧里是增量，
  但在 `event_final` 帧里装的是**累积全文**。`parseFrontierMessage` 对所有
  reply 帧一视同仁地取 content 当 delta，收尾帧就把全文又追加了一次。
- **修复**：解析完 `content_v2` 拿到 `phase` 后，`event_final` 帧清空 `Delta`，
  只保留 `Snapshot` 供校对。

### 测试

- 新增 `TestParseFinalFrameDoesNotDuplicate`（final 帧 delta 必须为空）与
  `TestParseDeltaFrameKeepsDelta`（普通增量帧仍保留 delta）。
  变异移除清空逻辑后前者精确失败。

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
