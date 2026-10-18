# Changelog

## 0.1.4 (2026-10-19)

### 变更

- **面板改为与 workbuddy 一致的卡片式布局**。原先是一张拥挤的表格，
  账号、空间、额度全挤在同一行；现在改为三列卡片网格（窄屏降级为两列 / 单列），
  每张卡片按「标题 + 状态徽章 + 账号标识 + 基础额度进度条 + 当前空间 /
  订阅额度 + 全部空间清单 + 操作按钮」分层展示。
- 引入与 CPA 管理台对齐的主题令牌（纸感亮色 / `data-theme=white` / `data-theme=dark`），
  嵌入 iframe 时实时镜像父级主题，独立打开回退系统偏好。
- 顶部新增账号总览（启用数 / 禁用数 / 订阅额度合计 / 工作空间数）、
  关键字搜索与按订阅额度排序。
- 导入、切换工作空间、测试模型、删除改为模态框交互；操作结果改用右上角
  toast 通知栈，不再把日志堆在页面底部。
- 测试模型改为先拉取该账号可见模型列表再点选，避免盲测默认模型。

### 说明

- 面板是纯展示层改动，管理接口路径与语义未变。

## 0.1.3 (2026-10-19)

### 修复

- **账号凭据被宿主覆盖，导致模型别名路由后一律报「未找到可用账号」**。
  生产表现为 `gpt-6-luna`（别名指向 `notion/omodi`）与直连
  `notion/omodi` 返回同一条 `auth_unavailable: ... 未找到可用账号`。
  排查确认与模型别名机制无关：宿主
  `OAuthModelAliasChannel` 对未知 provider 直接返回 provider 自身，
  `notion-provider` 作为别名 channel 是被支持的。
  真正原因是 `authDataForLogin` 只回传 `ID/Provider/Label/Disabled/ProxyURL/Metadata`
  而没有 `StorageJSON`；宿主把 `AuthData.StorageJSON` 当作该账号的持久化存储
  （`storageJSONFromAuth` 在 RawJSON 为空时回落 marshal `Metadata`），
  于是用这份「精简认证数据」重写账号文件，磁盘上的
  `cookie_header`、`token_v2`、`spaces` 全部丢失，只剩
  `disabled/space_id/space_name/type/user_id`（生产实测 194 字节）。
  修复为回传完整账号 JSON 作为 `StorageJSON`。

### 修复（续）

- **`resolveAuth` 匹配不到账号**。宿主的 `AuthID` 由凭据文件相对路径派生，
  形如 `notion-xxx.json`；插件本地账号标识不带扩展名，两者直接比较永远不相等。
  修复为比较前剥离 `.json` 后缀，并跳过停用账号。
  同时当宿主下发的 `StorageJSON` 缺少 `AuthID` 时用 `AuthID` 回填。
- **宿主账号索引不可用时账号列表为空**。`listHostAuthFiles` 在 RPC 失败、
  响应非 OK、结果为空三种情况下回落到磁盘扫描，避免刚热重载时宿主索引
  尚未重建导致推理链路误判无号可用。

### 测试

- `TestAuthDataCarriesFullStorage`：锁定 `StorageJSON` 必须能反序列化回
  `cookie_header` / `space_id` / `spaces[].PremiumCredits`。
- `TestResolveAuthStripsJSONSuffix`：锁定带 `.json` 后缀的宿主 `AuthID` 可匹配本地账号。
- `TestResolveAuthSkipsDisabled`：锁定停用账号不可被解析。

## 0.1.2 (2026-10-19)

### 修复

- **流式响应不输出 usage，导致下游用量与计费全部记零**。宿主只从分片或
  响应体里读 token 数；0.1.1 的终止帧只带 `finish_reason`，没有 `usage`，
  实测生产流式请求在 new-api 通用日志里 Tokens 恒为 `-`、费用 $0
  （非流式路径正常，因为它整体返回 usage）。
  修复为在终止帧输出 OpenAI 规范 `usage`，数值取自上游
  `agent-inference` 段的 inputTokens / outputTokens。增量帧保持不带
  usage，避免下游重复累加。

### 测试

- `TestBuildStreamFinishIncludesUsage`：锁定终止帧必须带 usage 且数值正确。
- `TestBuildStreamDeltaOmitsUsage`：锁定增量帧不得带 usage。

## 0.1.1 (2026-10-19)

### 修复

- **面板在生产环境读不到账号列表**。资源页 `/v0/resource/plugins/<id>/panel`
  由宿主免密钥放行，但数据接口 `/v0/management/plugins/<id>/*` 仍校验
  `Authorization: Bearer <management key>`。0.1.0 的面板只读
  `localStorage['cpa-management-key']`，这个键在 CPA 主面板里并不存在，
  结果浏览器实测显示「加载失败：missing management key」。
  修复为与 coze 面板一致的三级回退：主面板 `cli-proxy-auth`（XOR + base64）
  → `?key=` URL 参数（读后立即从地址栏抹除）→ 手工输入（仅存 sessionStorage）。
  鉴权失败时自动弹出密钥输入框，不再只报一行错误。

### 测试

- 生产环境用真实浏览器打开面板，确认真实渲染与数据加载链路。
- 真实账号端到端：非流式 `PONG`、流式无重复帧、多轮 `PONG-2026` 复述、
  面板探活 1.86s 返回 OK。

## 0.1.0 (2026-10-18)

### 新增

- **插件骨架**：`notion-provider` 注册 `model_provider` / `auth_provider` /
  `executor` / `management_api` 四项能力，模型作用域 `both`，
  输入输出均为 `openai` 格式。
- **uTLS 传输层**：`HelloChrome_Auto` + 自定义 HTTP/2 `DialTLSContext`；
  UA、`sec-ch-ua`、TLS ClientHello 三处 Chrome 主版本统一从 `browser.go` 取，
  避免版本漂移导致指纹不一致。
- **Cookie 导入**：解析 `token_v2` / `notion_user_id` / `notion_browser_id` / `device_id`，
  探测工作空间与额度后原子落盘；探活失败**不落盘**。
- **多工作空间**：按 `getSpaces` 双层结构取内层真实 workspace id，
  逐空间记录 `premiumCredits`，默认选额度最高者。
- **模型目录**：39 条实测代号（36 可用），支持 `notion/` 前缀、裸代号与展示名三种写法。
- **transcript 协议**：首轮建 thread、续写复用 thread；补齐 5 个必需顶层字段；
  user 步使用二维字符串数组。
- **NDJSON patch 状态机**：按 `id` 对齐快照、异构 value 数组分流 text/thinking、
  注册帧自带正文也下发、`patch-sync` 重发不重复下发。
- **执行器**：流式（异步 `host.stream.emit`）与非流式两条路径，
  请求体**优先读 `Payload`**（宿主的翻译结果），为空才回退 `OriginalRequest`。
- **故障转移**：阶梯冷却（基数 `cooldown_seconds`，封顶 10 分钟）+ 同请求换号；
  40x 换号受 `retry_on_4xx` 预算约束，429/402 不受限；
  业务 400 不计入冷却。
- **静默失败识别**：HTTP 200 但无 `agent-inference` 段、正文为空、
  流内 `trust-rule-denied` 帧，均判定为账号级失败并触发换号。
- **会话复用**：`authID|spaceID|model` 三级键，2 小时 TTL，2048 条上限；
  切换工作空间时按前缀整批清理该空间的会话。
- **Cookie 滚动**：上游 `Set-Cookie` 中的身份类 Cookie 写回账号文件；
  写盘前比对内容摘要，外部重新导入后放弃本次滚动，不覆盖新凭据。
- **管理面板**：账号列表、Cookie 导入、启停、删除、工作空间切换、
  真实推理探活、模型列表与额度查询。

### 测试

- `transport_test.go`：指纹版本一致性、transport 配置、请求头。
- `authfile_test.go`：Cookie 三种形态、账号标识稳定性、路径穿越防护、
  落盘回读、工作空间选择、模型代号归一、禁用模型不注册。
- `protocol_test.go`：首轮/续写字段差异、user 步矩阵结构、17 个序列化字段、
  system/assistant 折叠、Payload 优先。
- `stream_test.go`：5 份真实脱敏 NDJSON 样本、thinking 与正文分离、
  多 `patch-sync` 场景、坏行跳过、回调中断。
- `failover_test.go`：冷却阶梯、失败分类、40x 预算、选号跳过、拒绝帧识别。
- `session_test.go`：键隔离、生命周期、TTL、容量淘汰、切空间整批清理、并发安全。
- `cookie_roll_test.go`：Cookie 合并、写回、防覆盖、删除指令。
