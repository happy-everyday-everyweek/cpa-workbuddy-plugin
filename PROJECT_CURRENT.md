# 项目当前状态

## 目标与范围

- 目标：维护 CLIProxyAPI (CPA) 的 Go 原生插件套件 `cpa-workbuddy-plugin`（涵盖 8 大 AI 服务商 Provider + 1 统一用量统计插件），提供多账号管理、动态模型发现、流式转发、专属现代管理控制台、扫码/Token 登录、保号巡检与统一 token 用量统计分析服务。
- 范围：仓库内 9 大插件体系的代码实现、单元测试、资产构建、发布流水线、registry 元数据同步等：
  1. `workbuddy-provider`：腾讯 CodeBuddy（国内版与 Global 版）；
  2. `workbuddy-ai-provider`：WorkBuddy AI（独立国际版）；
  3. `traework-provider`：Trae SOLO（字节跳动海外/国内 AI IDE 客户端）；
  4. `qoderwork-provider`：QoderWork（阿里通义灵码衍生国内版）；
  5. `qoder-ai-provider`：Qoder AI（国际版 qoder.com，含 UMID 协议支持）；
  6. `cursor-provider`：Cursor（cursor.sh，Connect-RPC 协议与会话连续性）；
  7. `zcode-provider`：ZCode（BigModel Coding Plan，Anthropic 兼容与客户端真实指纹 mimic）；
  8. `copilot-provider`：GitHub Copilot（设备码 OAuth / 内部令牌保活 / VS Code Copilot Chat 指纹模拟）；
  9. `workbuddy-token-usage`：统一 Token 用量统计与实时看板服务。
- 非范围：CLIProxyAPI 核心服务本体；跨项目文件修改。

## 项目概况

- 状态：活跃维护中。9 大插件全景全量编译与单测验证全绿（cgo-shim 零错误）。已彻底废除并删除 Gemini 插件（因依赖庞大导致二进制膨胀翻倍、GCP Project 门槛过高、缺乏调度与管理面板）。最新发布/就绪版本：
  - `zcode-provider`: **0.1.31**（面板额度改为逐模型独立进度条（GLM-5.3 / GLM-5.3-Flash 各一条）并移除「过期」等冗余信息；0.1.30 面板额度改用 `billing/balance` 取真实已用/剩余 + 领取失败文案友好化；0.1.29 修复面板无感验证 captcha 配置缺陷；0.1.28 面板内无感验证领取 Start Plan + OAuth 默认中国区）
  - `cursor-provider`: **0.1.6**（修复 Query 参数反序列化丢失导致真实发现模型被屏蔽、建立点号模型别名归一化根治上游 not_found、测试弹窗支持实时搜索过滤与秒开交互）
  - `qoderwork-provider`: **0.9.44**（0.9.44 流式中途断流零泄漏换号重试 + 断流信封补 `http_status=502` + 冷却落盘失败可观测与内存兜底；0.9.40 重建上游 messages 时保留工具调用结构修复 agent 多轮往返工具结果失配对；0.9.38 签到节奏恢复每 4 小时六槽 + 移除冗余门控；0.9.37 tools 对齐修复；0.9.35 max_tokens 统一 32768 最低输出底线；0.9.34 上游 110 日调用上限 24 小时持久化冷却 + 路由/换号排除 + 面板解冷）
  - `qoder-ai-provider`: **0.1.24**（0.1.24 流式中途断流零泄漏换号重试 + 断流信封补 `http_status=502`；0.1.20 重建上游 messages 时保留工具调用结构修复 agent 多轮往返工具结果失配对；0.1.18 签到节奏恢复每 4 小时六槽 + 移除冗余门控；0.1.17 tools 对齐修复；0.1.15 max_tokens 统一 32768 最低输出底线；0.1.14 上游 110 日调用上限 24 小时持久化冷却 + 补齐 /clear-cooldown 路由注册）
  - `traework-provider`: **0.2.13**（面板签到时刻文案漂移修正为 00/04/08/12/16/20；0.2.12 定时探活降频至 4 小时 + 失败也刷新节流时间戳；0.2.11 max_tokens 32768 底线 + payload 层同步，探活改全量模型按序测试）
  - `workbuddy-provider`: **0.16.5**（0.16.5 流式中途断流零泄漏换号重试 + 断流信封补 `http_status=502`；0.16.4 账号故障冷却改为 30s 递增阶梯、上限 10 分钟；0.16.3 成长中心接单失败不再虚报成功 + 错误原因可见化；0.16.0 成长中心全自动领奖 7 类玩法，`growth_auto` 默认关闭）
  - `workbuddy-ai-provider`: **0.1.16**（0.1.16 授权后自动补注册地等新能力；0.1.15 定时探活降频至 4 小时 + 失败也刷新节流时间戳；0.1.14 max_tokens 32768 底线，探活改全量模型按序测试）
  - `copilot-provider`: **0.1.2**（0.1.2 调度器对齐宿主候选集语义修复越界缺陷，已发布 `0054c28`→`ba471b0`→`bca1264`→`78b4069` 并生产验收：热重载前 5 分钟 50/50 请求告警、热重载后 0/57；0.1.1 宿主 OAuth 弹窗改开插件桥接页展示设备码；0.1.0 首发：Device Flow 登录 + 内部令牌 80% 提前保活 + VS Code Copilot Chat 指纹模拟 + 动态模型发现 + 多账号池换号重试 + 流式/非流式转发 + 额度与面板）
  - `workbuddy-token-usage`: **0.2.8**（0.2.8 让退役实例的 feed 导入协程在连续 3 轮关闭句柄后自行退出；0.2.7 修复宿主配置重载换句柄窗口导致的用量记录丢失；0.2.6 移除四个图表与导出图片、下线 `/stats/trends` 及序列聚合以省算力与响应体积；0.2.5 起请求明细与维度汇总展示原始 provider id）
- 活动工作区：F:\cpa-plugin
- 当前时间：2026-10-17 (GMT+8)

## 活动会话进展摘要

- 当前会话（2026-10-18）：**coze 0.3.0 发布生产，验收连修三处真实缺陷（0.3.1/0.3.2/0.3.3）**：
  - **发布链（0.3.0）**：commit `ed32383`（22 文件）→ push → dispatch CI run `39120784169` success（取消冗余 push run `39120767331`）→ 下载 7 assets 全 OK → `publish-assets.py` → registry commit `2117b34` → 远端 ALL PASS → 生产 `plugin-store install` 返回 `restart_required:false`，`hot reloaded active_version=0.3.0 retired_version=0.2.2`，落盘 `.so` sha256 `bcc6cdc0…` 与本地 zip 内 `.so` 逐字节一致，零容器重启；accounts/panel 均 200。
  - **缺陷 1（0.3.0 → 0.3.1）：流式响应 500**。生产 `POST /v1/chat/completions` 立即返回 `json: unsupported type: <-chan pluginapi.ExecutorStreamChunk`。根因：宿主 `rpcExecutorStreamResponse.Chunks` 是 **slice**（`internal/pluginhost/rpc_schema.go:52`），`rpc_client_stream.go:29` 先判 `len()` 再转 channel；0.3.0 直接把 channel 塞进 `ExecutorStreamResponse.Chunks` 返回，JSON 序列化即失败。修复：带 `stream_id` 时立即返回响应头，后台用 `host.stream.emit` 逐帧推送、`host.stream.close` 收尾；无 `stream_id` 时同步收集返回 slice；补 `X-Accel-Buffering: no`。回归 `TestStreamResponseSerializable`（变异回 channel 形态精确复现生产报错文案）。发布链 `06bb281` → CI `39121930862` success → registry `45e6851` → 远端 ALL PASS → 生产热重载 `active_version=0.3.1 retired_version=0.3.0`，落盘 sha256 `50dfcd41…` 与本地一致。
  - **缺陷 2（0.3.1 → 0.3.2）：WS 握手缺 Cookie，只能收到心跳**。0.3.1 后流式虽完整但耗时 92 秒、正文在最后一帧一次性到达——首帧后 91 秒零增量，正好卡在首字超时 90s，实际走轮询兜底。**对照实验（同一生产账号，仅差 Cookie 头）**：不带 Cookie 6 帧全是 `hi`；带 Cookie 25 帧含 21 条二进制业务帧，0.34–4.25s 陆续到达。**根因**：frontier 网关对无 Cookie 握手照样返回 101，但只推心跳。修复：`frontierClient` 增加 `cookieHeader`，`runOnce` 握手写入 `Cookie`；`runStream` 传 `sa.CookieHeader`。回归 `TestFrontierHandshakeCarriesCookie`（变异删头精确 FAIL）。
  - **生产日志佐证**：`frontier 已连接`（19:19:06）→ `连接中断`（19:20:37）相隔 91 秒，中间零业务帧。
  - **缺陷 3（0.3.2 → 0.3.3）：正文重复一遍**。0.3.2 部署后生产时序验收：总耗时从 92s 降到 17.71s、29 个正文帧在 5.02–17.70s 陆续到达（间隔约 0.3s），真流式恢复。但发现末帧 578 字**恰好等于前文拼接**，全文 1156 字重复输出两遍。**根因**：`content.message_data.reply.content` 在 `event_delta` 是增量、在 `event_final` 是**累积全文**，`parseFrontierMessage` 一视同仁当 delta 追加。修复：解析完 `content_v2` 拿到 phase 后，`event_final` 清空 `Delta` 只留 `Snapshot`。回归 `TestParseFinalFrameDoesNotDuplicate` + `TestParseDeltaFrameKeepsDelta`（变异移除清空逻辑精确 FAIL）。
  - 文档：`doc/5-tests/2026-10-18_182000_Coze对话链路实现验证.md`、`doc/6-review/2026-10-18_182000_Coze对话链路实现_6-review.md`（STYLE: PASS）。
  - **待办**：0.3.3 走完整发布链并做最终生产流式验收（期望增量陆续到达 + 正文不重复）。

- 前序会话（2026-10-18）：**coze TASK-004 对话链路实现完成（v0.3.0），真流式落地**：
  - 需求：用户「开工 TASK-004，按 §4.6 实现收帧逻辑」。
  - **实现（5 个新文件，约 1600 行）**：`frontier_frame.go`（12 字段 protobuf 手写编解码，按 tag 分派+跳过未知字段）、`frontier.go`（WS 客户端：`aid=751148`/`fpid=1289`/`xsack=0`/`pbbp2`、10s `hi` 心跳、读超时、指数退避重连）、`claw.go`（claw/info→get_main→chat→message/list→get_frontier_key，统一 `code` 业务判定）、`convert.go`（OpenAI messages 折叠、chunk/completion 构造）、`executor.go`（流式/非流式入口、`turn_id+seq_num+phase` 去重、`event_final` 收尾、首字超时降级轮询）。
  - **实现中修复的两个真实缺陷**：① 心跳协程只在 ticker 触发时检查状态，服务端断开后要空等 10 秒才重连 → 改为读循环结束即取消心跳协程；② 增量按「累积差值」下发，两帧间隔小于轮询间隔时被合并成一大块 → 改为 `pending` 队列逐帧 emit。
  - **验证**：cgo-shim build/vet/test 全绿；新增 46 个用例（含 2 个 live test）；3 次变异反证均被精确捕获；关键用例用真实帧 hex 而非自造样本。

- 前序会话（2026-10-18，已压缩）：**coze CYCLE-01 协议取证：抓到真流式帧，TASK-004 阻断解除**：浏览器内给 `WebSocket.prototype.send` 打补丁捕获页面真实 WS 实例，抓到 100 帧业务数据（44 条 reply 增量）。关键实测：`aid=751148`/`fpid=1289`/`xsack=0`/`pbbp2`（`connection-params` 的 `aid=578287`/`service=33555616` 是另一条通道，用错只收心跳）；claw 业务 `service=33555657`；protobuf 12 字段；阶段机 `event_start`→`event_delta`→`event_final`；`content.message_data.reply.content` 是 delta、`content_v2.session.final` 是累积快照（实测 10 个 delta 拼接 371 字符 == final 371）；去重键 `turn_id+seq_num+phase`。文档见 `doc/5-tests/2026-10-18_164500_Coze对话链路协议补全.md` §4.6。

- 前序会话（2026-10-18）：**coze-provider 0.2.2 定时续期改由插件自建定时器执行**：
  - 需求：用户问「续期是自动的吗?」，随后要求「定时 4 小时补 `refresh_interval_seconds`，续期，和其他插件的定时一样」，并要求面板展示积分余额。
  - **根因（宿主源码取证）**：`refresh_interval_seconds` 只决定宿主「何时把账号排进刷新队列」；`auto_refresh_loop.handleDueAuth` 在 `exec == nil` 时只做 `l.upsert(authID, now.Add(interval))` 空转重排，**不调用 `auth.refresh`**。`RegisterExecutors` 只注册声明 executor 的插件，coze 声明 `Executor:false`，因此宿主的 4 小时排期对本插件永不产生续期。生产证据：24h 内 0 条 auto-refresh 日志，3 次续期全是手动点击。
  - **实现**：新增 `coze/renew_loop.go`（默认 4 小时遍历启用账号执行 `renewSession` + `applyRenewal`；热重载后立即跑首轮；`reconfigure` 即时重排；`Shutdown` 关闭停止信号；并发上限 4；经宿主 `host.log` 留痕）。`main.go` 在 `cliproxy_plugin_init` 启动、`Shutdown` 停止。
  - **顺带修复真实缺陷**：`applyRenewal` 原防覆盖判据 `bindStore(path).replaced()` 在写前才取摘要，比对对象永远是文件自身 → 外部重新授权的新凭据会被旧快照静默覆盖。改为读取时锚定 `StoredAuth.LoadedDigest`，删除失效的 `storeBinding`/`bindStore`。
  - **验证**：cgo-shim build/vet/test 全绿（新增 12 个用例）；哨兵证明测试真实进编译；7 次变异反证全部精确命中；真实 cgo 构建 10.9MB `.so`（8 个 ABI 符号、版本串 0.2.2）；真实 `.so` ctypes 探针 `test/coze/renew_loop_probe.py` **7/7 PASS**（含判别力变异：注释掉 `startRenewLoop` → 4 项 FAIL）。
  - **发布（0.2.2）**：CI run `39106119186` success → `90edaae`(源码) → 资产 6/6 `ALL CHECKSUMS OK` → `4f45084`(registry)；远端 git 内容 version=0.2.2、6 artifacts sha256+size 齐全（raw CDN 有缓存延迟）。
  - **生产部署（15:15:18）**：`plugin-store install?version=0.2.2` → 200 + `restart_required:false`，**零容器重启（Up 2 days）**；`active_version=0.2.2 retired_version=0.2.1`；落盘 `.so` sha256 `589830201cba…` 与本地 zip 一致。
  - **生产行为验收（关键）**：15:15:19 与 15:15:38 各一轮 `定时续期完成`（**无手动点击**）；`last_renew_at` 推进到 `15:15:38`，`auth_state=ok`；`/panel` 200；`/credits` 返回 3000/3000/1500；0 error/panic。
  - **未验证（诚实标注）**：4 小时排期的第二/三轮（仅等到首轮与热重载轮），由 `TestRenewLoopFiresOnInterval` 覆盖；插件直连 `www.coze.cn` 不经宿主 `host.http.do`，`.so` 探针无法拦截上游往返，换发与落盘由 Go 单测覆盖。

- 前序会话（2026-10-18）：**coze-provider 0.2.0 模型动态发现 + 面板重做**：声明 `model_provider` 并实现 `model.static`/`model.for_auth`（`/v1/model/list` 的 `model_type` 必须是字符串枚举，多模态目录才有完整视频族，生产实测 16 模型）；面板移除授权区、按 workbuddy 风格重做、加 `/models` 与 `/test-active`、时间统一 `Asia/Shanghai`。已发布 0.2.0 并生产热重载验收（`active_version=0.2.0`、`/test-active` 可见 16 模型）。未验证：真实推理链路（属 CYCLE-01）。
- 当前会话（2026-10-18）：**qwenwork CYCLE-02 多账号调度：修复账号身份 key 失配（P0）**：
  - 需求：用户「启动 CYCLE-02 多账号调度」，范围为账号池 / 调度三模式+会话粘性 / 冷却阶梯+换号重试。
  - **核心缺陷（克隆遗留，P0）**：执行器把 `auth.accessToken` 当账号身份，而调度候选、冷却表、会话绑定、积分缓存全按宿主 `auth.ID`（`qwenwork-<uid>.json`，由 [authIDForPath](file:///C:/Users/luode/go/pkg/mod/github.com/router-for-me/!c!l!i!proxy!a!p!i/v7@v7.2.129/internal/pluginhost/auth_callbacks.go#L504) 从相对路径派生）索引。实测 token 是 552 字符 JWT ⇒ 冷却写到永远查不到的键 ⇒ 故障账号永不退出轮转，多账号调度退化为单账号。
  - **修复 6 项**：① 新增 `accountIdentity()`，执行器与流式路径统一用宿主 `auth.ID`（流式补 `initialAuthID` 参数）；② `scheduler_mode: off` 解析补全（原被静默当 session）；③ keepalive 令牌刷新改 `foldCredentialIntoDoc` 折叠式直写，保住 `disabled`/`manual_disable`/`exhausted_disable`/计数/`daily_cooling_until`；④ `markSessionDead` 改直写；⑤ `hostAuthIndexForPhys` 补 Name/规范名两级匹配；⑥ 删除 `labelForAuth` 恒假分支。
  - **验证**：新增 10 个单测；四次变异反证全部精确命中（身份退回 token / 删 off 解析 / 折叠改重建 / 同关两处冷却闸门 → 分别 2/1/2/4 个用例 FAIL）；真实 `.so` ctypes 探针 `test/qwenwork/multiaccount_probe.py` 9/9 PASS（含探针自身判别力变异验证）；WSL 真实 cgo 构建 + test + vet 全绿；gofmt 空；版本 0.1.0→**0.2.0**。
  - **未验证（诚实标注）**：真实双账号端到端切换。本机仅 1 个真实账号，且失败路径需先过真实 MaaS 签发（`sharedHTTPClient` 直连，不经宿主桥），桩驱动无法不联网注入 401；该链路由「同键一致性单测 + 调度侧冷却排除单测」组合覆盖。
  - **0.2.0 已发布（2026-10-18）**：CI run `39062771266` 全绿（Test + 6 平台 build/cross + Release）→ `cc7fe59`(assets，先于 registry 推送) → `bf36ab1`(registry 0.2.0，6 artifacts) → `33d5fb2`(prune 0.1.0)。远端 6/6 raw URL HTTP 200 且 Content-Length 与 registry size 全等，下载复算 sha256 6/6 一致；6 个二进制内均含 `0.2.0`；远端 registry 12 插件无空 artifacts；旧 0.1.0 URL 现 404（预期）。
  - **生产部署（2026-10-18 04:04:54，首次安装）**：`plugin-store install?version=0.2.0` → HTTP 200 + `restart_required:false`，**零容器重启**；日志 `plugin loaded`/`plugin registered` 均为 `version=0.2.0`；落盘 `.so` sha256 `b75b5628…` 与本地 zip 逐字节一致；`/accounts`（`scheduler_mode:session`、0 账号）/`/config`/`/models`/面板资源全 200；外网 `/v1/models` 200（36 模型）；`config_store` 真实换行 1502 行未塌缩、端口稳定 `:8317`。
  - **别名 400 缺陷修复（0.2.3~0.2.6，2026-10-18）**：new-api 测试 `qwen-3.8-max` 失败而面板测试同模型成功。**四层缺陷串联，逐层暴露**：① 0.2.3 执行器不做别名反解；② 0.2.4 `ExecutorModelScope=OAuth` 致宿主跳过 `StaticModels`、别名缓存恒空；③ **0.2.5 真正主因——透传 body 未改写 `model`**（生产日志 `model=qwen3.8-max-preview alias=qwen-3.8-max` 证明宿主早已反解 `ExecutorRequest.Model`，插件却透传 `req.Payload` 原文）；④ 0.2.6 调度会话键被当作 MaaS `session_id` → `422 not a valid UUID`（新增 `maasSessionID` 做 SHA-256 确定性派生 v4 UUID）。
  - **生产验收（0.2.6，热重载零重启）**：`qwen3.8-max-preview`/`qwen-3.8-max`/`qwen-3.8`/`qwen3.8-max` 四条全部 **200**（修复前 200/500/503/503）；流式 + `X-Session-ID` 头同样 200，SSE 正常 `[DONE]`，响应 `model` 为真实 ID。每次修复均做变异反证精确命中。
  - **安装坑**：`plugin-store install` 不带 `source` 参数报 `direct plugin version "x" not found`（多源合并路径版本解析失败），必须带 `&source=source-636fa01121af`。
  - **有意保留**：`clientUA = "qoderwork/1.3.1"` 是实测通过的 UA（探针同值），非残留；`isDailyCountExceeded` 的 110 判定实测四形态不误伤，留作防御。
  - 文档：实施总览 § CYCLE-02 执行结果、`doc/4-bugs/2026-10-18_054500_qwenwork执行器漏接oauth-model-alias反解致别名请求400根因诊断.md`、`doc/5-tests/2026-10-18_021000_*`、`doc/6-review/2026-10-18_021000_*`（STYLE: PASS）。
  - **额度耗尽修复（0.2.7，2026-10-18）**：用户指出面板账号「可用积分」永久卡「加载中…」且无 0 积分耗尽表现。根因两层：① 上游 `quota.exceeded=true` 未写入快照，免费版账号 `total_used/total_size` 恒 0 ⇒ 确定耗尽被判成「未知」；② 四处代码按「0 积分仍可用免费模型」设计，但生产实测该账号对 `qwen3.8-max-preview`/`flash`/`plus`/`glm-5.2` **全部 429 `balance_insufficient`**——QwenWork 无免费模型。修复：`creditsSummary` 加 `Exceeded`、解析抽 `buildCreditsSummary`；新增 `isCreditsLowOrExhausted` 统一调度/换号/可路由排除；新增 `disableAuth`（`disabled`+`exhausted_disable`，恢复自动启用，`manual_disable` 永不自动改）；面板 `isAccountExhausted` 单一入口 + 「已耗尽 · 余0」。`lifecycle_auto` 默认关闭（写 disabled 需开启），路由排除与面板展示无条件生效。
  - **0.2.7 发布与生产验收**：CI `39114864713` 全绿 → `f1fa228`(registry) → 生产 install `restart_required:false`、`hot reloaded active_version=0.2.7 retired_version=0.2.6`、容器 Up 2 days 零重启；落盘 `.so` sha256 `235a3720…96258` 与 zip 逐字节一致；`/accounts` 返回该账号 `exhausted=true exceeded=true`；面板资源含 `isAccountExhausted`×7 与「已耗尽 · 余0」；推理 `qwen3.8-max-preview`/`qwen-3.8-max` 均 200（调度正确跳过耗尽账号）。
  - **0 计费修复（0.2.8，2026-10-18）**：用户报 new-api 侧大量 `$0` 记录 + 104.93s 首字。根因：上游把计费 usage 挂在 `delta:{}` 且无 `finish_reason` 的终止 chunk 顶层，`cleanChunkJSON` 的「全空 delta 整帧丢弃」规则把它当噪声吞掉 ⇒ 下游拿不到 token 数。直连上游实测 `usage_frames=1`，抓 CPA 输出流确认末帧只有 `finish_reason:stop` + `[DONE]`（`include_usage:true` 也拿不到），锁定丢弃点在插件侧。修复：顶层存在 `usage` 时保留整帧（仍清空 `tool_calls`/`function_call` 噪声），异步与同步两条流式路径共用同一函数。**104.93s 经实测为 38.8 万 token 预填充耗时**（feed 216 条中 TTFT 前二均对应 `input=388,677`），非插件开销；同会话输入涨到 31 万后单轮 TTFT 稳定 7~12s。附带修复：`lifecycle_auto` 被写进 `config_yaml` 子键导致插件读不到（改用 `PATCH /v0/management/plugins/<id>/config` 写顶层），以及 `refresh_runner.doFetchOne` 缺生命周期对账导致开关打开后耗尽账号仍不停用（补 `reconcileOneAccount(..., false)`）。
  - **0.2.8 发布与生产验收**：CI `39118226004` 全绿 → `920aa5c`(registry，6 平台远端全 200 且体积全等) → 生产 install `restart_required:false`、`hot reloaded active_version=0.2.8 retired_version=0.2.7`、容器 Up 2 days 零重启；落盘 `.so` sha256 `1d1ab751…d45f87c` 与 zip 逐字节一致；生产流式响应含 usage 帧（`total_tokens:87`，修复前 0 个）；new-api 18:16:29 起连续 3 条计费正常（`prompt_tokens=349312/quota=17475`），此前同会话连续 8 条为 0；耗尽账号 `disabled=True exhausted=True`。文档：`doc/4-bugs/2026-10-18_183000_qwenwork流式usage终止帧被丢弃致new-api全量零计费根因诊断.md`。
- 当前会话（2026-10-18）：**qwenwork-provider 0.1.0 立项实现并全链路发布**：Device Flow + PKCE（`gateway.qwenwork.cn`）换宿主 JWT → `POST /api/desktop-sessions`（**201 非 200**）签发 `sk-mm-v1.*` 短票据（TTL 2h）→ 裸 Bearer 打 `{base_url}/v1/chat/completions`；MaaS 无 `/v1/models`，目录在 `GET /api/desktop/chat-modes`。推理走**请求体透传**（仅强制 `stream=true` + `max_tokens` 自适应），管理路由收敛 13 条。发布链 `a9585c7` → CI `39050879075` → `91e050e` → `14bc392`(6 平台) → `3568d37`(registry)；图标改用仓库内 `assets/icons/QwenWork.png`（原外链 404）。
- 当前会话（2026-10-18）：**发布矩阵下线 freebsd-amd64，平台数 7 → 6**：
  - 需求背景：用户反馈「7 平台产物里有一个平台之前构建时出过问题，能否缩减」，并裁定「只砍 freebsd-amd64」。
  - 根因确认：`go-cross/cgo-actions@v1` 硬编码下载 FreeBSD `14.3-RELEASE/base.txz`，该版本已从官方源下架（404），2026-10-17 曾导致全部 10 插件 freebsd 构建集体失败并连带 Release job skipped；当时的 wget 包装层改写 `14.3→14.4` 属绕过，本次直接下线该平台根治。
  - **改动范围**：`.github/workflows/build.yml`（删除 12 条 freebsd 交叉矩阵项、删除 wget 补丁步骤、release notes 表格删行）；`scripts/publish-assets.py`（`PLATFORMS` 移除 `freebsd_amd64`、删除 `OPTIONAL_PLATFORMS` 可选平台逃生门）；`registry.json`（10 插件各 7→6 artifacts）；`release-assets/`（删除 10 个 freebsd zip 并同步各 `checksums.txt`）；文档同步 `README.md` / `cursor/README.md` / `项目设计.md` / `AGENTS.md` / `CLAUDE.md` / 两份发布 skill。
  - **保留项**：`build-cross` 仅剩 windows-arm64（12 条）；release job 的 6 平台完整性检查本就不含 freebsd，未改。
  - **验证**：workflow YAML 解析通过（native 60 条 5 组合 / cross 12 条仅 windows-arm64）；`validate-registry.py` OK（10 插件各 6 artifacts，组合恰为 6 个）；`publish-assets.py` 临时目录正向实跑（6 zip → 6 artifacts 回写成功）与负向实跑（缺 windows_arm64 → `missing zips` + exit=1）；10 个版本目录 `sha256sum -c checksums.txt` 全 OK；临时目录已清理。
  - **真实 CI 已验证（run `39052278051`，提交 `0118051`）**：84 success / 1 skipped（Release job 因纯 push 跳过）/ 0 failure；作业总数由 108 降至 84，**freebsd 作业为 0**，交叉构建仅剩 12 条 windows-arm64。
  - 文档：`doc/6-review/2026-10-18_010000_发布矩阵下线freebsd平台_6-review.md`（STYLE: PASS）。
- 当前会话（2026-10-17）：**qoder 三插件流式断流换号与冷却落盘修复（CYCLE-01 + CYCLE-02 全部闭环）**：
  - 需求背景：生产会话 `5fa71566` 中 `qwen-3.8-flash` 请求（ID `202610171157242386880308268d9d6KQb4LXIN`）上游断流后未换号。根因：插件断流分支无换号路径（主）+ 断流信封无状态码（次）+ 24h 冷却 92 次触发零落盘（独立缺陷）。
  - **已实施 5 个任务**：qoderwork / workbuddy / qoder-ai 三插件新增 `isStreamReadError` + `shouldRotateOnStreamReadError` 纯函数；异步与同步断流分支接入分级换号（`emitted` 零泄漏守卫）；信封补 `http_status=502`（已泄漏时抑制）；qoderwork 冷却落盘失败补日志 + 内存冷却提前到 host RPC 之前 + watchdog 刷新合并保留未落盘条目。
  - **验证**：三插件 cgo-shim 全绿（qoderwork 10.05s / workbuddy 26.14s / qoder-ai 10.14s）；8 次哨兵反证均精确命中单例；5 份 `doc/6-review/` 记录均为 `STYLE: PASS`。
  - **已发布并生产部署（2026-10-17 22:40）**：qoderwork 0.9.44 / workbuddy 0.16.5 / qoder-ai 0.1.24；提交链 `d035e5e`(fix 23 文件) → `a47d5ad`(CI FreeBSD 14.4 镜像修复) → `f467b4d`(assets 24) → `6535ca5`(registry) → `d8faa83`(prune 0.14GB)。远端 21/21 artifacts ALL PASS 无残留；三插件 `plugin hot reloaded` 且 `active_version` 命中目标版本、落盘 `.so` sha256 与本地 zip 逐字节一致、panel/accounts 全 200，**零容器重启**（容器 Up 2 days）。
  - **CI 阻断修复（既有缺陷，非本轮引入）**：`go-cross/cgo-actions@v1` 内硬编码下载 FreeBSD `14.3-RELEASE/base.txz`，该版本已从 `download.freebsd.org/releases` 下线（404），既有 patch 指向的 `archive.freebsd.org/old-releases` 同样 404，导致全部 10 插件 freebsd-amd64 构建失败并连带 `Release` job 被 skipped。已改为在 wget 包装层把 `14.3-RELEASE` 改写为仍可用的 `14.4-RELEASE`。
  - **生产验收边界**：热重载后 15 分钟窗口内无断流事件，端到端「断流 → 换号 → 第二次成功」未能实证；已改为确认新符号 `shouldRotateOnStreamReadError` / `after partial output` / `daily cooling persist failed` 已进入三插件生产二进制（qoderwork 另含 `mergeActiveDailyCooling`），服务 `/v1/models` 200、无 error/panic 日志。后续需在真实断流发生时观察日志。
  - 文档：`doc/4-bugs/2026-10-17_202500_qoderwork流式中途断流不换号且24h冷却未落盘根因诊断.md`、`doc/3-实施/2026-10-17_203500_qoder三插件流式断流换号与冷却落盘修复实施总览.md`（§14 记录执行结果与偏差）。
- 会话（2026-10-15，已压缩）：**跨插件同步第二轮复检（只读审计）**：以 md5 对比 + 机制分布 grep + 版本矩阵复核 9 插件，原批次 A~D 全部闭环。

- 会话（2026-10-15，已压缩）：**qoder 双插件重建上游 messages 保留工具调用结构（qoderwork 0.9.40 / qoder-ai 0.1.20）**：根因是模板 baseprompt 的 agent 上下文 + 插件重建 messages 丢掉 `tool_calls`/`tool_call_id`；方案 A 补三字段并同步自定义 `UnmarshalJSON`（陷阱：加字段必须同步解析否则恒空）。**判别力边界**：单工具任务 6/6 无判别力，探索型任务不收敛 ⇒ 只修结构丢失，不承诺消除持续工具调用。发布链 `f74a5e8`→`6d32f84`→`d9e8592`→`5124003`，热重载零重启，远端 14/14 ALL PASS。

- 会话（2026-10-15，已压缩）：**五插件签到节奏统一为每 4 小时**：qoderwork/qoder-ai 由 `[10,22]` 恢复为 `[0,4,8,12,16,20]` 并移除失效门控；修正面板与文档错标的 `09:00/21:00`；四插件发布生产、零重启，远端 28/28 ALL PASS。

- 会话（2026-10-15，已压缩）：**zcode-provider 0.1.31 面板额度改为逐模型独立进度条**：`quotaHTML` 由单条汇总条改为按 `entitlements[]` 逐模型渲染，移除「过期」行；回归脚本补逐模型断言；发布链 `0702b84`→CI success→`b7b754a`(assets)→`0d6c46a`(registry)；生产面板 `pb-item`/`pb-name` 各 4 处、旧结构残留 0。

- 会话（2026-10-15，已压缩）：**zcode-provider 0.1.30 面板额度改用 `billing/balance` 取真实用量**：根因是 `billing/current` 只返回授予量、无已用/剩余，且面板进度条硬编码 100%；改走 balance（强制设备三头 `X-Platform`/`X-Device-Mid`/`X-ZCode-App-Version`）+ `friendlyClaimError` 文案友好化；发布链 `a49101a`→`1b80e6f`(assets)→`6fd166d`(registry)，生产热重载零重启，`/credits` 返回 `source=balance has_usage=True` 且用量真实。Bug 文档 `doc/4-bugs/2026-10-15_023000_zcode面板额度永远显示满额根因诊断.md`。

- 会话（2026-10-14/15，已压缩）：**zcode-provider 默认中国区 + 面板内无感验证领取额度（0.1.28→0.1.29）**：0.1.28 加中国区默认与动态 planId；0.1.29 修 `captchaVerifyParam` 仅 76 字符缺陷（未传 `prefix`/`region`），修复后 280 字符含 `securityToken`。**未覆盖**：上游对全部账号 `billing/preview` 返回 `plans:[]`，完整激活未实证（上游活动态）。

- 会话（2026-10-14，已压缩）：**zcode-provider 支持 bigmodel（中国区）OAuth，0.1.25~0.1.27**：复用既有 CLI 轮询链路（仅 provider/域名/回调路径参数化）；0.1.26 修非法 provider 静默回退，0.1.27 修 bigmodel 推理 401（`open.bigmodel.cn/api/anthropic` 不接受 ZCode JWT）。上游对生产 IP 有时段性 WAF/限流，属外部因素。

- 会话（2026-10-14，已压缩）：**四插件定时探活降频至 4 小时并修失败账号节流缺陷**：**真实缺陷**：`recordActivePing` 原仅成功时刷新时间戳，失败账号不计时 → 每个 tick 被反复重测，比健康账号更耗配额；改为「无论成败都先刷新」。四插件各补回归用例，全链路发布生产、零容器重启。

- 会话（2026-10-14，已压缩）：**zcode-provider 0.1.22 领取指引**：改为如实 Start Plan 指引（`guideModal`）；根因：额度领取须官方客户端手动完成（claim 强制阿里云人机验证头，无头一律 `400 3007`）。

- 当前会话（2026-10-15）：**copilot-provider 0.1.1：宿主 OAuth 弹窗改开插件桥接页展示设备码，全链路发布生产**：
  - 需求背景：用户在管理台点「登录」后，GitHub 授权页 8 个空白框需手输设备码，但宿主 OAuth 弹窗**不显示设备码**（附截图）。根因：宿主 `ServePluginAuthURL`（`auth_files_oauth_callback.go:218`）把插件 start 响应的 `Metadata` 只写进宿主 session store、**不回传前端**，硬编码返回 `{status,url,state}`；宿主内置 provider 是把 `user_code` 放响应顶层才显示，插件无法照做。用户采纳「选项 B：桥接页」方案。
  - **实现（方案 B）**：`handleStartLogin` 的 `URL` 改返回**插件自有 resource 前缀桥接页相对路径** `/v0/resource/plugins/copilot-provider/login-bridge?state=<state>`（resource 路由是插件唯一免鉴权 GET 面），新增 `bridge.go`（`loginBridgeURL`/`handleLoginBridge`/`loginBridgePage`/`htmlEscape`），桥接页渲染设备码（32px/letter-spacing:4px/user-select:all）+ GitHub 授权链接 + 三步指引；注册进 `managementRegistration().Resources`（**不挂 Menu**，不进菜单）。
  - **关键设计**：桥接页**只展示、不驱动轮询**——`pollDeviceLogin` 成功即 `Delete` 会话（read-once），桥接页若自行轮询会偷走凭据致宿主 `PollLogin` 拿不到 `AuthData`；轮询与落盘仍归宿主 `get-auth-status` → `PollLogin` 链路。
  - **发布闭环**：`100bc6b`(fix 11 文件) → CI dispatch `38732577295` success（并 cancel 同 head 冗余 push run `38732367637`）→ `2324317`(8 assets，sha256sum -c 7/7 OK + zip 内 `.so` 含 `0.1.1`/`login-bridge`) → `a5c2da0`(registry) → `fe8ed5a`(prune 0.1.0)；生产热重载 `active_version=0.1.1 retired_version=0.1.0`（20:26:17），**零容器重启**（Up 42h），落盘 `.so` sha256 `12c9fb2e…` 与本地 zip 逐字节一致。
  - **生产行为验收（真实端点）**：宿主弹窗端点 `GET /v0/management/copilot-provider-auth-url` 返回 `url=/v0/resource/plugins/copilot-provider/login-bridge?state=…`（`is_bridge=True`）；用该 state 打开桥接页真实渲染出设备码（与 start 返回一致）、含 GitHub 授权链接、HTML 完整；`get-auth-status` 返回 `{"status":"wait"}`；accounts/panel/bridge 内网与外网全 200。远端 7/7 artifacts sha256+size ALL PASS、0.1.0 已 404。
  - 门禁：cgo-shim build/vet/test 全绿（30 例）+ 面板回归 PASS + 双重变异反证（哨兵进编译 / 还原 URL 精确 FAIL）+ 真实浏览器 DOM 取证（正常页与错误页两分支）+ `gofmt -l` 空 + `POLLUTION: PASS` + `STYLE: PASS` + OCR 无 Critical；提交全程路径限定（踩坑 57）。
  - 文档：`doc/3-实施/2026-10-16_010000_copilot宿主OAuth弹窗设备码桥接页实施总览.md`、`doc/6-review/2026-10-16_013000_*`。

- 上一会话（2026-10-15）：**新增 copilot-provider 插件（0.1.0 首发）并完成 Kiro 可行性评估（裁定不立项）**：
  - **copilot-provider 立项与实现**：按 `doc/3-实施/2026-10-15_Copilot与Kiro插件立项实施总览.md` 的 CYCLE-1~4 推进。GitHub 免费档实测可用（`sku=free_limited_copilot` 可换内部令牌、发现 57 个模型、流式与非流式推理均通），关闭 `GAP-001`；`GAP-002` 关闭（持久化上游 `endpoints.api` 权威端点）。单测 27 例。
  - **CYCLE-4 发布闭环**：`c546b57` → `56ac86f`(8 assets) → `8966b0c`(registry)；CI 双 run success；远端 7/7 ALL PASS；生产 `plugin-store install` 返回 `restart_required=false`，**零容器重启**，落盘 `.so` sha256 `65d428b8…` 逐字节一致，四路由全 200。`AC-006`/`AC-007` 双 PASS。
  - **过程中的真实故障（已修复）**：侦察阶段提前写入的 registry 占位条目（`artifacts: []`）被并行会话的宽泛 `git add` 一并推送 → CPA 源级校验失败 → **所有插件** install 返回 `plugin_not_found`。由并行会话 `10a1f2f` 移除恢复；本轮随后严格按发布 skill Step 11 语义重做。
  - **Kiro 可行性评估（CYCLE-5，评估非实现）**：结论**不立项**。① Kiro FAQ 明文禁第三方 harness；② 免费档仅 **50 credits/月**；另有社区终局封禁实证。报告 `doc/3-实施/2026-10-15_235000_Kiro可行性评估报告.md`。

- 会话（2026-10-13，更早历史已压缩，详见 git log 与 PROJECT_HISTORY.md）：
  - **六插件 max_tokens 统一 32768 底线**：`applyMaxTokensFloor` 四出口收敛；zcode 规格矩阵 14→64 条；四插件探活改「全量按序 any-success」。
  - **Qoder 110 日上限 24h 持久化冷却**：`isDailyCountExceeded` 精确识别 110；`daily_cooling_until` 落盘 + 内存镜像 + watchdog 重建；路由/换号/粘性绝对排除，`resetAccountFailover` 刻意保留；新增 `POST /clear-cooldown` 与面板徽标。
  - **max_tokens 官方规格自适应**：Qoder 客户端写死 `max_output_tokens=8192` 致思维链吃满配额、正文被 `finish_reason: length` 截断；内置规格表，推理模式放宽至官方上限、超限 clamp。
  - **ZCode 0.1.14/0.1.15**：修 `usage_feed.go` 孤立路径与缺 `tokens` 嵌套、`ExecutorModelScopeStatic` 误声明致别名未绑定（改 `ExecutorModelScopeOAuth`）；claim 需 `X-Aliyun-Captcha-Verify-Param` + `sgp`（后被 0.1.28 无感验证取代）。
  - **qoder 排队帧未识别致 60s 挂死（0.1.9 / 0.9.30）**：`parseQoderSSEChunk` 增第 4 类顶层 `code` 形态 + `unwrapNestedMessage` 逐层展开；哨兵反证移除后精确复现 10.00s 挂死。
  - **删除 Gemini 插件**（体积 14.4MB、强依赖 GCP 结算、无面板与容灾）；**ZCode OAuth 重构为官方 CLI 轮询**（0.1.5→0.1.8，deep-link 唤起本地客户端）；**Qoder 403 首包假成功与流式诊断**（生产 nginx `proxy_buffering off` 后外网 1.35s）；**deepseek-v4.1-flash 推理断开根因**（同名裸模型多服务商认领 + 会话亲和钉死）；**Token 用量面板移除 4 图表**（0.2.6~0.2.8）。

- （更早的 13 条会话进展摘要已按 51200 字节上限裁剪，历史见 git log 与 PROJECT_HISTORY.md）

<!-- BEGIN RECENT PROJECT SESSIONS -->
## 最近 5 个同项目会话

| 会话 ID | 标题 / 意图 | 状态 | 活跃时间 | 关键改动 / 影响 |
|---|---|---|---|---|
| `01a11b9b-eb7d-7830-afed-7c8dd64c6629` | WorkBuddy AI 国际版签到改造实施 | completed | 2026-10-09 05:46 | 面板「领取专家加油包」失效入口替换为国际版每日签到，新增自动签到开关与批量签到；0.1.8 发布部署验收通过，上游活动离线登记为 GAP-001 |
| `01a11ba7-3193-7ab0-9618-11075ecaba63` | Token 用量面板性能与图标修复 | completed | 2026-10-09 03:06 | token-usage 0.2.4 移除面板 SSE 短连接轮询与 15s 定时轮询、改手动刷新并补插件图标；发布部署验收通过 |
| `01a115b7-7f04-7b81-8e96-cb269a144fa4` | Cursor 插件移植与 Token 导入实施 | completed | 2026-10-07 23:41 | 新增第六个插件 cursor-provider 0.1.0，支持会话 Token 导入账号；全链路发布、生产热重载与端到端验收均通过 |
| `01a11624-7389-7fe2-ac2b-82eb433267bd` | release-assets 3.75 GB 清理旧发布缓存 | completed | 2026-10-07 19:34 | release-assets 从 3.75 GB 清理至 0.13 GB；发布后清理规则固化为项目 skill 与 prune 脚本 |
| `01a110f3-1891-72c1-914e-7600d0e8b6df` | WorkBuddy AI 彻底清空写死模型与 0.1.3 全链路发布部署 | completed | 2026-10-07 03:20 | workbuddy-ai 彻底剔除硬编码模型，完全动态拉取；0.1.3 发布并生产部署热重载验证通过 |
<!-- END RECENT PROJECT SESSIONS -->
<!-- BEGIN TASK PLAN PROJECTION -->
```json
{
  "version": 5,
  "registry_schema": "task_plan_projection_registry",
  "registry_updated_at": "2026-10-18T09:12:12Z",
  "projections": [
    {
      "projection_id": "SESSION/30608b3616fa2311bfb720a2776c09ac96b2738601ec3752b03a252c810cdb70",
      "session_id": "01a115da-13cb-7f90-ac94-25c133fdc31e",
      "projection_origin": "synthesized",
      "synthesis_mode": "exact",
      "state": "active",
      "plan_key": "IMPL-CURSOR-PROVIDER-PORT-20261007",
      "source_document": "doc/3-实施/2026-10-07_Cursor插件移植与Token导入实施总览.md",
      "plan_fingerprint": "9eea12fd114fb3e34cb60dd2a60eceb2eacd227a7e9a481c5b3f73c954a2a3b6",
      "updated_at": "2026-10-07T12:25:35Z",
      "steps": [
        {
          "id": "TASK-001",
          "step": "[TASK-001] 移植源码并统一标识",
          "status": "completed"
        },
        {
          "id": "TASK-002",
          "step": "[TASK-002] 令牌解析与导入",
          "status": "completed"
        },
        {
          "id": "TASK-003",
          "step": "[TASK-003] 导出删除启停接口",
          "status": "completed"
        },
        {
          "id": "TASK-004",
          "step": "[TASK-004] 面板与本地回归收口",
          "status": "completed"
        },
        {
          "id": "TASK-005",
          "step": "[TASK-005] 发布与部署",
          "status": "in_progress"
        }
      ]
    },
    {
      "projection_id": "SESSION/df86ce8afa18a9515072ffdfb460ee7e85402285d28ea505ed37f9133e10c0e8",
      "session_id": "01a115b7-7f04-7b81-8e96-cb269a144fa4",
      "projection_origin": "synthesized",
      "synthesis_mode": "exact",
      "state": "active",
      "plan_key": "IMPL-CURSOR-PANEL-AUTOENTRY-20261008",
      "source_document": "doc/3-实施/2026-10-08_Cursor面板免密直入与workbuddy对齐实施总览.md",
      "plan_fingerprint": "16acf9f349058ff187e5a3a7fb9bcc2f856cc447c47c98da76c8206319350112",
      "updated_at": "2026-10-07T17:15:57Z",
      "steps": [
        {
          "id": "TASK-001",
          "step": "[TASK-001] 面板免密直入改造：密钥区默认隐藏、启动自动加载、移除订阅额度说明区块",
          "status": "in_progress"
        },
        {
          "id": "TASK-002",
          "step": "[TASK-002] 测试与本地回归：management_test 断言更新、面板自动进入回归脚本、cgo-shim-build 全绿",
          "status": "pending"
        },
        {
          "id": "TASK-003",
          "step": "[TASK-003] 文档与版本收口：DESIGN/CHANGELOG/VERSION 0.1.1、6-review 风格回归",
          "status": "pending"
        },
        {
          "id": "TASK-004",
          "step": "[TASK-004] 发布与生产部署验证：commit/push/CI/资产入库/registry/热重载验收",
          "status": "pending"
        }
      ]
    },
    {
      "projection_id": "SESSION/5818ea886692422d8a462f2eb89e9b453f44422b609457bddc7ce1391942abe6",
      "session_id": "01a1219b-6aff-7fb2-822f-d9160626c22c",
      "projection_origin": "synthesized",
      "synthesis_mode": "exact",
      "state": "inactive",
      "plan_key": "IMPL-QODERWORK-CHECKIN-FALSE-SUCCESS-20261010",
      "source_document": "doc/3-实施/2026-10-10_QoderWork签到假成功修复实施总览.md",
      "plan_fingerprint": "7aab74187dac06f2d24f3b680a2913cfb6f68784eaf7f6a7f403d3a5d2607581",
      "updated_at": "2026-10-11T00:30:00.000000Z",
      "steps": [
        {
          "id": "TASK-001",
          "step": "[TASK-001] 取证与根因冻结：宿主桥状态码解码错误",
          "status": "completed"
        },
        {
          "id": "TASK-002",
          "step": "[TASK-002] 修复 host_bridge.go：抽出 parseHostHTTPDoResult",
          "status": "completed"
        },
        {
          "id": "TASK-003",
          "step": "[TASK-003] 收紧 billing.go：performCheckinCall 缺 success 字段返回失败",
          "status": "completed"
        },
        {
          "id": "TASK-004",
          "step": "[TASK-004] 新增 host_bridge_test.go 回归并完成反证与本地全绿",
          "status": "completed"
        },
        {
          "id": "TASK-005",
          "step": "[TASK-005] 版本与文档收口：bump 0.9.26、CHANGELOG、5-tests、6-review",
          "status": "completed"
        },
        {
          "id": "TASK-006",
          "step": "[TASK-006] 发布链：commit、push、CI、assets、registry、远端验证",
          "status": "completed"
        },
        {
          "id": "TASK-007",
          "step": "[TASK-007] 生产部署与账号 u09a5b6ab 行为验收",
          "status": "completed"
        }
      ]
    },
    {
      "projection_id": "SESSION/7226fa6af0bcd53d1de3ea0168019a75d3e69d438a2e508583aa50b16c3a005e",
      "session_id": "4a50f1f2-f554-4fcd-bb84-216778147dd2",
      "projection_origin": "synthesized",
      "synthesis_mode": "exact",
      "state": "inactive",
      "plan_key": "RELEASE-ZCODE-CLAIM-GUIDE-0.1.22",
      "source_document": "zcode/CHANGELOG.md",
      "plan_fingerprint": "7d0ab4c92600e8679dcb8ddfa5a628622ec80d2cc8a5e54f2ba1b61c543edec3",
      "updated_at": "2026-10-13T18:09:27.704552Z",
      "steps": [
        {
          "id": "TASK-001",
          "step": "[TASK-001] 提交 zcode 0.1.22 领取指引改动（仅 5 个 zcode 文件）",
          "status": "completed"
        },
        {
          "id": "TASK-002",
          "step": "[TASK-002] 推送 main 并派发 zcode-provider 0.1.22 CI 构建",
          "status": "completed"
        },
        {
          "id": "TASK-003",
          "step": "[TASK-003] 轮询 CI 运行并下载 8 个发布资产",
          "status": "completed"
        },
        {
          "id": "TASK-004",
          "step": "[TASK-004] 提交发布资产并运行 registry 发布脚本",
          "status": "completed"
        },
        {
          "id": "TASK-005",
          "step": "[TASK-005] 提交 registry.json 并验证远端 raw URL 可达",
          "status": "completed"
        },
        {
          "id": "TASK-006",
          "step": "[TASK-006] 生产环境 plugin-store 热重载安装并验收",
          "status": "completed"
        }
      ]
    },
    {
      "projection_id": "SESSION/4c4dab02fd732c0d17940754bc3095a8a25815699011645d49b517efc9cac1bb",
      "session_id": "75082e35-bf34-49e3-8ec9-d8c884098190",
      "projection_origin": "persisted",
      "synthesis_mode": "none",
      "state": "inactive",
      "plan_key": "FIX-QODER-STREAM-READ-FAILOVER-20261017",
      "source_document": "doc/3-实施/2026-10-17_203500_qoder三插件流式断流换号与冷却落盘修复实施总览.md",
      "plan_fingerprint": "f9fba8f7e10ccf53f10814cc188a002c7c92165a7c6c4561909339e3c52a3b70",
      "updated_at": "2026-10-17T13:26:54Z",
      "steps": [
        {
          "id": "TASK-001",
          "step": "[TASK-001] qoderwork 零泄漏断流换号：断流分支接入换号决策与重建请求",
          "status": "completed"
        },
        {
          "id": "TASK-002",
          "step": "[TASK-002] qoderwork 断流错误信封补 http_status=502",
          "status": "completed"
        },
        {
          "id": "TASK-003",
          "step": "[TASK-003] qoderwork 冷却落盘失败补日志并保留内存冷却兜底",
          "status": "completed"
        },
        {
          "id": "TASK-004",
          "step": "[TASK-004] workbuddy 断流换号并新增信封状态码通道",
          "status": "completed"
        },
        {
          "id": "TASK-005",
          "step": "[TASK-005] qoder-ai 断流换号与信封状态码扩展",
          "status": "completed"
        }
      ]
    },
    {
      "projection_id": "SESSION/dd4640b0e3990effb3a66aee0da5c3fe25288b12610cbe5e75c08c80d2a34b14",
      "session_id": "bbc80e05-e600-4495-bdc2-9fd5e1bfc7cf",
      "projection_origin": "persisted",
      "synthesis_mode": "none",
      "state": "active",
      "plan_key": "IMPL-QWENWORK-MULTI-ACCOUNT-20261018",
      "source_document": "doc/3-实施/2026-10-17_234000_QwenWorkProvider插件立项_实施总览.md",
      "plan_fingerprint": "21081b87f6808d09b806eb30cc74aedcc99e3c61226a4fa090740970209e032f",
      "updated_at": "2026-10-17T18:06:46Z",
      "steps": [
        {
          "id": "TASK-001",
          "step": "[TASK-001] 侦察 qwenwork 多账号/调度/冷却现状与 CYCLE-02 差距",
          "status": "completed"
        },
        {
          "id": "TASK-002",
          "step": "[TASK-002] 账号池与账号落盘对齐（含令牌刷新折叠式直写）",
          "status": "completed"
        },
        {
          "id": "TASK-003",
          "step": "[TASK-003] 调度三模式 + 会话粘性对齐（含 off 解析修复）",
          "status": "completed"
        },
        {
          "id": "TASK-004",
          "step": "[TASK-004] 冷却阶梯 + 换号重试对齐（账号身份 key 统一为宿主 auth.ID）",
          "status": "completed"
        },
        {
          "id": "TASK-005",
          "step": "[TASK-005] 双账号故障切换验证 + 文档与风格收口",
          "status": "completed"
        }
      ]
    },
    {
      "projection_id": "SESSION/b104d3bd7fe586700ffcf91a64d1b13f225f91728325744aad9b76fccf1540cb",
      "session_id": "b98da698-d42c-4920-afdf-34f3bc242d7c",
      "projection_origin": "persisted",
      "synthesis_mode": "none",
      "state": "active",
      "plan_key": "DIAG-PROJECT-DISK-USAGE-20261018",
      "source_document": "doc/4-bugs/2026-10-18_172500_项目磁盘占用过大根因诊断.md",
      "plan_fingerprint": "41eebc847b50e161b00543a1659f14721dff70011ae0728f14474bf54e37d36c",
      "updated_at": "2026-10-18T09:12:12Z",
      "steps": [
        {
          "id": "TASK-001",
          "step": "[TASK-001] 统计工作树各目录与 .git 的真实磁盘占用",
          "status": "completed"
        },
        {
          "id": "TASK-002",
          "step": "[TASK-002] 定位大体积来源：release-assets、Git 对象、缓存、构建产物",
          "status": "completed"
        },
        {
          "id": "TASK-003",
          "step": "[TASK-003] 归纳根因与安全清理建议，落盘诊断文档",
          "status": "completed"
        }
      ]
    }
  ],
  "quarantined_projections": [
    {
      "reason_code": "invalid_projection",
      "affected_session_id": "eccae029-046a-47a1-a6f9-f417e5d06b29",
      "source_version": 5,
      "source_index": 6,
      "raw_entry": {
        "projection_id": "SESSION/eccae029046a47a1a6f9f417e5d06b29queueframefailover000000000000000000",
        "session_id": "eccae029-046a-47a1-a6f9-f417e5d06b29",
        "projection_origin": "synthesized",
        "synthesis_mode": "exact",
        "state": "active",
        "plan_key": "FIX-QODER-QUEUE-FRAME-CLASSIFY-20261015",
        "source_document": "doc/4-bugs/2026-10-15_160000_Qoder排队帧被误判账号级故障且跨provider状态码通道缺失根因诊断.md",
        "plan_fingerprint": "queue-frame-classify-and-status-channel-20261015",
        "updated_at": "2026-10-15T09:40:00Z",
        "steps": [
          {
            "id": "TASK-001",
            "step": "[TASK-001] Bug 单落盘：排队帧误判账号级 + 跨 provider 通道缺失",
            "status": "completed"
          },
          {
            "id": "TASK-002",
            "step": "[TASK-002] 第1步：排队帧不再误判为账号级故障",
            "status": "completed"
          },
          {
            "id": "TASK-003",
            "step": "[TASK-003] 第2步：envelopeError 补 http_status 打通宿主状态码通道",
            "status": "completed"
          },
          {
            "id": "TASK-004",
            "step": "[TASK-004] 宿主侧改动计划：异步流错误通道透传状态码",
            "status": "completed"
          },
          {
            "id": "TASK-005",
            "step": "[TASK-005] 真实测试：单测 + 必失败哨兵 + cgo-shim 全绿",
            "status": "completed"
          },
          {
            "id": "TASK-006",
            "step": "[TASK-006] 6-review + 发布 + 生产热重载验收",
            "status": "completed"
          }
        ]
      }
    }
  ]
}
```
<!-- END TASK PLAN PROJECTION -->
