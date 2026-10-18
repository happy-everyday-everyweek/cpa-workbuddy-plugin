# Changelog

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
