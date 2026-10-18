# cpa-workbuddy-plugin — CLIProxyAPI 插件集合

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Go 原生插件集合，将主流 AI 服务与私有逆向协议封装为 **OpenAI / Anthropic 兼容 Provider**，供 CPA 网关统一调度。套件支持多账号运维管理、账号阶梯故障转移、40x 同请求换号重试、会话粘性路由、全插件统一调度（硬排除测试标签 + 低积分优先）、动态模型发现与别名映射、流式长推理稳定性加固（首包即停与 liveBody 实时流）、每日自动签到与积分生命周期管理、桌面客户端指纹模拟（Mimic）与阿里安全盾（UMID）协议支持，以及统一 Token 用量统计与实时看板。

目前包含 **9 个并列插件**（8 大 AI 服务商 Provider + 1 统一用量统计插件），每个插件采用独立模块、独立版本与独立 Release 生命周期管理。

---

## 插件全景一览

| 插件 ID | 源码目录 | 服务商 / 目标服务 | 当前商店版本 | 核心特性与一句话定位 |
|---|---|---|:---:|---|
| [`workbuddy-provider`](#workbuddy-provider--腾讯-codebuddy-provider) | [`workbuddy/`](workbuddy/) | 腾讯 CodeBuddy（CN + Global） | `v0.15.5` | 原生 OAuth Provider，积分生命周期 + 每日签到 + 5 模型轮测探活 + 统一调度与用量 Feed |
| [`workbuddy-ai-provider`](#workbuddy-ai-provider--workbuddy-ai-国际版) | [`workbuddy-ai/`](workbuddy-ai/) | WorkBuddy AI 国际版（workbuddy.ai） | `v0.1.10` | 专属国际版端点与前缀，面板扫码登录，Pro 试用与每日签到，低积分优先与全套路由回归单测 |
| [`traework-provider`](#traework-provider--trae-solo-provider) | [`traework/`](traework/) | TRAE SOLO（字节跳动 AI IDE） | `v0.2.8` | 逆向 Provider，面板浏览器授权免 IDE 登录，16 位设备指纹防风控签到，流式首包即停与 liveBody 实时流 |
| [`qoderwork-provider`](#qoderwork-provider--qoderwork-cn-provider) | [`qoderwork/`](qoderwork/) | QoderWork CN（qoder.com.cn） | `v0.9.29` | PKCE 设备授权 + PAT 双登录，纯 Go COSY 签名与 QoderEncoding，多模态解析，彻底消除假成功漏洞 |
| [`qoder-ai-provider`](#qoder-ai-provider--qoder-ai-国际版) | [`qoder-ai/`](qoder-ai/) | Qoder AI 国际版（qoder.com） | `v0.1.8` | 纯 Go 独立实现阿里安全盾 UMID 设备指纹协议，多账号虚拟硬件隔离每日签到（+100 积分），COSY 签名 |
| [`cursor-provider`](#cursor-provider--cursor-provider) | [`cursor/`](cursor/) | Cursor（cursor.sh） | `v0.1.4` | 会话 Token 导入与备份 JSON 恢复，Connect-RPC 协议转译与会话连续性，250+ 真实模型发现与秒开搜索 |
| [`zcode-provider`](#zcode-provider--zcode-provider) | [`zcode/`](zcode/) | ZCode（BigModel Coding Plan） | `v0.1.4` | Anthropic 兼容协议接入，ZCode 桌面客户端真实指纹模拟（Mimic），选模型测试探活与专属 Web 面板 |
| [`copilot-provider`](#copilot-provider--github-copilot-provider) | [`copilot/`](copilot/) | GitHub Copilot（github.com） | `v0.1.0` | GitHub OAuth 设备码登录，Copilot 内部令牌保活，VS Code Copilot Chat 客户端指纹模拟，动态模型发现与多账号故障转移 |
| [`workbuddy-token-usage`](#workbuddy-token-usage--用量统计-dashboard) | [`token-usage-tracker/`](token-usage-tracker/) | 统一 Token 用量统计与实时看板 | `v0.2.6` | 汇聚各插件共享 NDJSON 用量流，提供请求明细（含服务商列）、维度汇总与成本分析，移除图表轻量化 |

> 注：表格中「当前商店版本」以 [registry.json](registry.json)（CPA 插件商店官方可直接安装版本）为准；源码目录内 `VERSION` 可能包含已开发完毕的更高版本。

---

## 插件深度总览

### workbuddy-provider — 腾讯 CodeBuddy Provider

覆盖国内版 `copilot.tencent.com` 与国际版 `workbuddy.ai`（统一基线架构）：

- **OAuth 登录** — 多账号 `workbuddy-<uid>.json` 写入宿主 auth 目录；面板登录轮询按文件路径严格去重。
- **动态模型发现** — 上游 models API 实时拉取 + 5 分钟缓存 + 静态兜底；支持宿主 `oauth-model-alias` 与 `oauth-excluded-models` 治理；兼容宿主 YAML block sequence 与多行 JSON 两种 models 落库形态。
- **执行器** — OpenAI 兼容 chat completions，流式（真 SSE 走 `host.stream.emit`）与非流式；内置 `tool_choice` 归一与 Claude Code 模板清洗。
- **账号 Failover 与换号** — 429/402/5xx/传输错误按 1/3/10 分钟阶梯退避；401/403/404/405 计入账号级故障，走 `retry_on_4xx` 同请求换号（预算默认 3，缺省键安全保持当前值，400 业务错直通不重试）。
- **统一调度内核** — 彻底移除保号池 `preserve`，统一采用「测试标签 `test_failed` + 冷却 cooldown」硬排除 + 存活候选低积分优先调度；全灭时 `Handled: false` defer 交还宿主跨 Provider 容灾。
- **5 模型轮测探活** — 定时探活与面板测试支持最多 5 模型随机轮测，单次 20s 超时，整轮 30s 预算熔断，any-success 快速退出且清标，消除单模型偶发故障造成的误杀。
- **积分生命周期** — CN 账号耗尽自动禁用、签到回血自动恢复；Global 账号耗尽自动删除凭据；执行器硬积分错误即时触发 reconcile。
- **每日签到** — 每 4 小时（00/04/08/12/16/20）定时自动签到，面板可手动批量签；per-account 互斥锁防止并发冲突。
- **计数持久化** — 内存为主 + 跟随定时任务落盘 auth 文件顶层 `success_count` / `failed_count`，容器重启不归零。
- **控制面板** — `/v0/resource/plugins/workbuddy-provider/panel`：积分进度条、CN/Global 筛选、账号删除（二次确认 + 归属校验）、冷却倒计时、选模型测试弹窗。
- **用量 Feed** — 每条请求的 token 消耗以 NDJSON 追加写入共享 feed，供用量插件消费。

---

### workbuddy-ai-provider — WorkBuddy AI 国际版

针对腾讯 CodeBuddy 独立国际版 `workbuddy.ai` 定制：

- **独立国际版端点** — 独立 API 路由与清晰的 `workbuddyai-` 凭据前缀，避免与国内版账号混淆。
- **面板扫码登录** — 内置弹窗二维码登录（QR 登录），无需手动提取 Token。
- **福利与签到** — 支持 Pro 14 天试用领取（+250 积分）与国际版每日签到调度。
- **Token 保活与看门狗** — 4 小时 Token 定时保活，10 分钟状态巡检。
- **完整调度与回归资产** — 移植 5 模型轮测探活、低积分优先选卡，配备完整的路由回归单元测试套件。

---

### traework-provider — TRAE SOLO Provider

基于对字节跳动 TRAE SOLO CN 桌面客户端的逆向封装与工程加固：

- **面板浏览器授权免 IDE 登录** — PKCE S256 + 16 位设备指纹复刻 SOLO 客户端，无需安装桌面 IDE 即可在面板中一键拉起浏览器授权并完成 OAuth 导入。
- **IDE 凭据解析导入** — 支持解析读取本地 IDE `storage.json` 快速同步已有账号。
- **每日签到与防风控** — 定时 + 面板手动批量签到；采用 SHA-256 确定性派生首位 1~3、末位 1~9 的 16 位纯数字设备号，遇到 9074（「当前参与用户太多」）自动轮换重试，杜绝风控拦截。
- **长推理流式稳定性** — 宿主流桥 open/read 超时竞速降级直连（30s/90s）；伪完成检测（content + reasoning 双轴健康度）；上游断流兜底收尾（`finish_reason=length`）。
- **实时流与闲置断开** — 实施 `liveBody: resp.Body` 实时直推，遇上游 `event: done` 立即返回哨兵退出大循环并主动切断连接，彻底根除 HTTP Keep-Alive 闲置挂起数十秒的问题。
- **探活提速与模型治理** — `/test-active` 改为流式首包即停 + Keep-Alive TLS 预热，消除思考模型整段思维链等待；支持 `oauth-excluded-models` 模型过滤与会话粘性路由。
- **真实 Token 用量** — 深入解析上游 `event:token_usage`，将真实 token、首字延迟（TTFT）与会话 Key 写入共享 feed。

---

### qoderwork-provider — QoderWork CN Provider

基于对阿里通义灵码衍生国内版 QoderWork 桌面客户端的逆向工程封装：

- **双登录共存** — ① OAuth 设备授权（PKCE，`dt-` 30 天 + `drt-` 1 年自动旋转，登录成功后自动领取一次性 Pro 升级包）② PAT 导入（`pt-`，长期有效兜底），两家族共存于同一 auth 文件。
- **彻底消除假成功漏洞** — 移除了依赖凭据文件创建时间的假导入逻辑，严格基于设备 Token 轮询判定，未在浏览器授权绝不误报成功。
- **COSY 签名推理** — 纯 Go 标准库实现 RSA 包裹 AES 会话密钥 + MD5 请求签名，直连 `gateway.qoder.com.cn` SSE 流式服务。
- **QoderEncoding 协议** — 自定义 Base64 字母表 + 三段重排的请求体编码。
- **多模态与终止加固** — 自定义解析 OpenAI 数组型 Content 多模态报文；统一识别顶层及嵌套 `[DONE]` 信封，即刻 break 退出防止挂死。
- **架构全量对齐** — 账号调度全面接入「硬排除测试标签 + 故障冷却 + 低积分优先 + 全灭 defer」，面板卡片支持选模型测试探活。

---

### qoder-ai-provider — Qoder AI 国际版

针对阿里通义灵码衍生国际版 `qoder.com`（全功能独立实现）：

- **阿里安全盾 UMID 纯 Go 独立实现** — 逆向官方客户端协议，纯 Go（零 CGO）完整实现 Section 0x1b 数据段封装、zlib 压缩、AES-128-CBC 加密以及标准 XXH64 设备特征提取，计算得出合规的 `machineToken`、`machineType` 与 `machineCode`。
- **多账号虚拟硬件隔离每日签到** — 按账号确定性生成独立的硬件特征，遇风控自动轮换设备凭据，实现多账号稳定的每日自动签到（每日 +100 积分）。
- **双登录与多模态** — 支持 OAuth PKCE 设备流与 PAT 导入，消除未授权假成功漏洞；支持多模态 Content 报文解析与 COSY 签名。
- **调度与探活** — 对齐低积分选卡、测试失败打标隔离与选模型测试探活。

---

### cursor-provider — Cursor Provider

将 Cursor（`cursor.sh`）的 Agent Service 封装为 CPA 标准 Provider：

- **会话 Token 导入** — 支持导入 Cursor 会话 Token（`user_<id>::<jwt>` 或 `WorkosCursorSessionToken`），通过官方 OAuth 令牌端点自动兑换刷新，自动保留原 JWT 段确保持续保活。
- **凭据备份与恢复** — 支持导出全部账号为 JSON 备份，并在导入弹窗中一键恢复。
- **Connect-RPC 协议转译** — 将 OpenAI 格式的 chat completions 请求转译为 Cursor 底层基于 Protobuf 的 Connect-RPC `AgentService/Run` 协议，完整支持 Checkpoint 与会话上下文连续性。
- **模型别名权威归一化** — 实现 `CanonicalModel` 映射，将传入的点号模型（如 `claude-3.5-sonnet`）自动映射为 Cursor 权威连字符 ID（`claude-3-5-sonnet`），彻底消除上游 `not_found` 错误。
- **海量模型发现与搜索** — 修复 Query 参数反序列化缺陷，完整透传账号 DiscoverModels 发现的 250+ 真实可用模型；测试弹窗支持即时模糊搜索过滤与秒开交互。
- **现代管理面板** — 接入 CPA 统一主题规范，支持深浅色自适应切换、状态筛选胶囊、账号卡片指标概览、测试探活与账号启停。
- **Token 用量接入** — 异步将每次请求的 token 记录追加到共享 NDJSON feed。

---

### zcode-provider — ZCode Provider

针对智谱 BigModel Coding Plan 的 Anthropic 兼容协议接入插件：

- **桌面客户端指纹模拟（Mimic）** — `internal/mimic/fingerprint.go` 全量注入 ZCode 桌面客户端 Headers 指纹（包含版本号、平台、trace-id、session-id 等），完美规避上游风控与设备校验。
- **多账号与凭据直写** — 支持 Z.ai 官方 OAuth 授权与 Token 导入，凭据保存在 `~/.antigravity_cockpit/zcode_accounts/`，物理文件直写防宿主字段丢失。
- **专属现代控制台** — 挂载于 `/v0/resource/plugins/zcode-provider/panel`，支持深浅主题切换、状态指标概览与账号批量管理。
- **选模型测试探活** — 面板支持弹出模态框按指定模型发起真实对话探活，测量端到端延迟并支持流式首包即停。
- **用量跟踪** — 统一采集 token 与耗时并上报至共享用量 feed。

---

### copilot-provider — GitHub Copilot Provider

将 GitHub Copilot 的订阅额度封装为 CPA 标准 Provider：

- **GitHub OAuth 设备码登录** — 复刻 GitHub 官方 Device Flow（`client_id` 与 `scope` 为 VS Code Copilot 扩展使用的公开常量），面板内展示设备码与授权地址，插件自动轮询绑定账号，无需手工复制回调。
- **Copilot 令牌自动保活** — 用 GitHub access token 换取 Copilot 内部令牌，按上游 `refresh_in` / `expires_at` 的 80% 提前重取，失败指数退避，避免推理请求因令牌过期中断。
- **客户端指纹模拟** — 复刻 VS Code Copilot Chat 的请求头（`Copilot-Integration-Id`、`Editor-Version`、`Editor-Plugin-Version`、`Openai-Intent`、`X-Initiator` 等），指纹取值集中管理，便于随上游版本单点更新。
- **动态模型发现** — 实时拉取上游 `/models` 并带缓存，支持宿主 `oauth-model-alias` 与 `oauth-excluded-models` 治理。
- **多账号池与故障转移** — 账号轮询调度、429/5xx 换号重试、阶梯冷却、测试失败硬隔离，账号类型（individual / business / enterprise）自动推导上游端点。
- **流式与非流式转发** — 原生 SSE 逐行实时推送，遇 `[DONE]` 立即退出并关闭连接，杜绝 Keep-Alive 空闲挂起。
- **专属管理控制台** — 挂载于 `/v0/resource/plugins/copilot-provider/panel`，支持深浅主题、账号卡片、配额进度条、模型查看与账号启停。

---

### workbuddy-token-usage — 用量统计 Dashboard

记录、统计并可视化跨插件各账户的真实 Token 消耗与费用：

```text
各 Provider 插件（workbuddy / traework / cursor / zcode 等）      workbuddy-token-usage 插件
┌───────────────────────────────────────────────┐   NDJSON   ┌─────────────────────────────────────┐
│ 每次推理请求完成                               │ append ▶  │ 轮询读取（支持增量同步与断点续传）    │
│ publishUsage / usage_feed 汇聚点 ─────────────┼──────────▶│ 导入自身专用 bbolt 数据库           │
│ 异步追加单行记录至共享 feed 文件                 │  O_APPEND │ Web Dashboard（"Token 用量"）看板   │
└───────────────────────────────────────────────┘           └─────────────────────────────────────┘
```

- **无锁共享 Feed 通道** — 基于 `<CLIProxyAPI root>/data/token-usage-feed.ndjson` 文件追加写入，彻底规避 bbolt 跨进程排它锁冲突；文件超过 128MB 自动安全轮转截断。
- **请求明细表** — 完整展示请求时间、使用模型、调用来源、会话 Key、首字延迟（TTFT）、Tokens 明细、费用及**调用服务商（Provider）**，支持四语言导出 CSV。
- **维度汇总表** — 按模型、服务商、账号等维度汇总结算请求量与 token 占比。
- **轻量化架构（v0.2.6+）** — 移除 4 个冗余的前端图表、Canvas 导出与趋势聚合计算接口，内嵌体积大幅削减（CSS −20%，JS 代码 −21%），移除高频定时轮询，默认展示最近 1 小时明细并支持手动即时刷新，极大节省宿主算力与网络负载。

---

## 核心架构与工程规范

### 1. 统一调度与账号健康隔离
所有 Provider 插件采用统一的调度与容灾口径：
1. **硬排除不可用账号**：带有测试失败标签（`test_failed`）以及处于故障冷却期（cooldown）的账号，调度器无条件硬排除，绝对不分配流量。
2. **存活账号低积分优先**：存活的健康候选账号按剩余积分升序排序，优先消耗临近耗尽的账号；当账号额度用尽触发探测失败时自动打标隔离。
3. **全灭 Defer 跨 Provider 容灾**：当该 Provider 下所有账号均被排除时，调度器执行全灭 defer（`Handled: false`），将请求交还 CPA 宿主调度其他 Provider，严禁死锁在故障 Provider 上。
4. **会话粘性绑定**：开启 `session` 模式时，同一会话的后续请求绑定到同一账号；换号重试前主动解绑失效会话。

### 2. 流式终止与测试探活提速规范
- **SSE 扫描即刻退出**：流式扫描器遇到 `[DONE]` 或 `event: done` 时必须立即 `break` 退出并主动关闭连接，严禁死等 Keep-Alive 闲置连接导致客户端无限转圈。
- **`liveBody` 实时直推**：流式转发必须直连 `resp.Body` 实时推流，严禁全量预缓冲。
- **探活流式首包即停**：面板「测试」按钮与后台活跃探活请求采用 `stream: true, max_tokens: 1`，收到首个有效数据帧即判定成功并异步关闭流，消除思考模型生成整段思维链带来的数秒等待。

### 3. 生产配置与热重载红线
- **插件安装/更新热重载**：生产环境下插件的新增与更新一律调用管理 API `POST /v0/management/plugin-store/<id>/install?version=<ver>`，CPA 自动写入配置并热重载生效（`restart_required: false`）。**非必要绝不重启生产 CPA 容器**，避免引发数十秒冷启动与流量中断。
- **数据库配置表只读不写**：生产 CPA 的配置表 `cli_proxy_api.config_store`（单条 `id='config'`）由管理系统自动维护为多行 YAML；严禁手工读取后进行 JSON 二次序列化写回（二次转义会导致真实换行塌缩为单行，导致 YAML 解析为空配置、端口漂移与 502 故障）。

---

## 仓库结构

```text
cpa-workbuddy-plugin/
├── workbuddy/                  # 插件 1：腾讯 CodeBuddy Provider（国内版与 Global 版）
├── workbuddy-ai/               # 插件 2：WorkBuddy AI 国际版 Provider（扫码登录 / 独立端点）
├── traework/                   # 插件 3：TRAE SOLO Provider（浏览器授权 / 签到防风控 / 流式加固）
├── qoderwork/                  # 插件 4：QoderWork CN Provider（COSY 签名 / 双登录 / 消除假成功）
├── qoder-ai/                   # 插件 5：Qoder AI 国际版 Provider（阿里安全盾 UMID / 每日签到）
├── cursor/                     # 插件 6：Cursor Provider（Connect-RPC 协议 / 真实模型发现 / 面板）
├── zcode/                      # 插件 7：ZCode Provider（桌面客户端指纹模拟 Mimic / Web 面板）
├── copilot/                    # 插件 8：GitHub Copilot Provider（设备码登录 / 内部令牌保活 / 指纹模拟）
├── token-usage-tracker/        # 插件 9：统一 Token 用量看板（共享 NDJSON / 明细表 / 维度分析）
├── registry.json               # 插件商店自定义源元数据（schema v2，direct install，包含 sha256 与 size）
├── release-assets/             # 本地资产缓存（.gitignore 忽略；二进制托管在 GitHub Release）
├── assets/                     # 共享资源（官方高清图标 icons/ 等）
├── scripts/                    # 运维与发布脚本
│   ├── cgo-shim-build.py       # 本地跨平台 cgo 验证 shim（Windows 无 C 工具链专用验证工具）
│   ├── download-release-assets.py # 从 GitHub Release 下载 7 资产到本地缓存并校验
│   ├── publish-assets.py       # 扫描 release-assets 并回填 registry.json（URL 指向 Release）
│   ├── validate-registry.py    # registry.json 格式与 SHA-256 完整性自检
│   └── qoder_cn_pat_login.py   # QoderWork CN PAT 辅助获取脚本
├── doc/                        # 研发全链路文档库
│   ├── 1-架构/                 # 总架构设计、目录树与主要业务链路说明
│   ├── 2-需求/                 # 历史需求与功能迭代文档
│   ├── 3-实施/                 # 实施总览与实施计划文档
│   ├── 4-bugs/                 # 生产故障与缺陷根因诊断文档
│   ├── 5-tests/                # 自动化测试与验证记录
│   └── 6-review/               # 代码风格与回归审查文档
└── .github/workflows/          # GitHub Actions 自动化 CI/CD 构建与 Release 流水线
```

---

## 多平台 Release 规范

每个插件发布时均通过 GitHub Actions 构建包含 6 大目标平台的动态链接库发布包，并附带权威的 `checksums.txt`：

```text
<id>_<version>_linux_amd64.zip      # 包含 <id>.so
<id>_<version>_linux_arm64.zip      # 包含 <id>.so
<id>_<version>_darwin_amd64.zip     # 包含 <id>.dylib
<id>_<version>_darwin_arm64.zip     # 包含 <id>.dylib
<id>_<version>_windows_amd64.zip    # 包含 <id>.dll
<id>_<version>_windows_arm64.zip    # 包含 <id>.dll
checksums.txt                       # 6 平台产物的 SHA-256 校验清单
```

**CI/CD 构建与发布触发模式：**

| 触发条件 | 执行动作 |
|---|---|
| `push` / `pull_request` | 触发全量矩阵编译、单元测试（`go test`）、静态检查（`go vet`）与 `validate-registry.py` 校验（仅验证不发 Release）。 |
| `workflow_dispatch` | 手动在 Actions 界面输入 `plugin=<id>` 与 `version=<ver>`，编译 6 平台构建包并自动创建该插件的独立 GitHub Release。 |

---

## 安装与配置

### 方式一：CPA 插件商店一键安装（推荐）

1. 在 CPA Manager Plus 或 CPA 管理后台的「插件商店」中添加自定义源：
   ```text
   https://raw.githubusercontent.com/luode0320/cpa-workbuddy-plugin/main/registry.json
   ```
2. 在商店列表中搜索并按需一键安装对应的插件：
   - `workbuddy-provider`
   - `workbuddy-ai-provider`
   - `traework-provider`
   - `qoderwork-provider`
   - `qoder-ai-provider`
   - `cursor-provider`
   - `zcode-provider`
   - `copilot-provider`
   - `workbuddy-token-usage`

> 提示：服务器生产环境可通过管理 API 热重载安装指定版本：
> ```bash
> curl -X POST "http://localhost:8317/v0/management/plugin-store/<plugin-id>/install?version=<version>" \
>      -H "Authorization: Bearer <your-secret-key>"
> ```

### 方式二：手动部署

从 [Releases](https://github.com/luode0320/cpa-workbuddy-plugin/releases) 下载对应平台的 ZIP 压缩包并解压，将编译产物拷贝到 CPA 宿主的 `plugins/` 目录：

```bash
# 以 traework-provider 为例（Linux amd64）
unzip traework-provider_0.2.8_linux_amd64.zip
cp traework-provider.so /path/to/cliproxyapi/plugins/
```

### `config.yaml` 推荐配置

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy-provider:
      enabled: true
      retry_on_4xx: 3
    workbuddy-ai-provider:
      enabled: true
      retry_on_4xx: 3
    traework-provider:
      enabled: true
      scheduler_mode: "session"
      retry_on_4xx: 3
    qoderwork-provider:
      enabled: true
      retry_on_4xx: 3
    qoder-ai-provider:
      enabled: true
      retry_on_4xx: 3
    cursor-provider:
      enabled: true
    zcode-provider:
      enabled: true
      base_url: "https://open.bigmodel.cn/api/anthropic"
      mimic:
        enabled: true
    copilot-provider:
      enabled: true
      account_type: "individual"
      dynamic_models: true
    workbuddy-token-usage:
      enabled: true
```

---

## 本地开发与测试

由于各插件依赖 CGO 导出 C ABI 供 CPA 动态加载，在 Windows 或缺乏 C 工具链的环境中直接执行 `go test` 会由于忽略 `import "C"` 文件而报错。

仓库内置了跨平台测试 Shim 工具：

```bash
# 验证指定插件的编译、vet 与单元测试（如 traework）
python scripts/cgo-shim-build.py traework

# 验证其他插件
python scripts/cgo-shim-build.py workbuddy
python scripts/cgo-shim-build.py cursor
python scripts/cgo-shim-build.py zcode
python scripts/cgo-shim-build.py copilot
python scripts/cgo-shim-build.py token-usage-tracker

# 校验 registry.json 格式与哈希合法性
python scripts/validate-registry.py registry.json
```

---

## 核心文档导航

| 文档路径 | 说明 |
|---|---|
| [KNOWLEDGE.md](KNOWLEDGE.md) | QoderWork CN 完整逆向知识库（凭证体系、COSY 签名、QoderEncoding 算法） |
| [doc/1-架构/](doc/1-架构/) | 项目全局架构、目录职责、业务链路与设计说明 |
| [doc/3-实施/](doc/3-实施/) | 各插件核心功能与历史重构实施总览报告 |
| [doc/4-bugs/](doc/4-bugs/) | 生产环境疑难故障诊断（同名模型冲突治理、配置存储转义损坏恢复等） |
| [doc/5-tests/](doc/5-tests/) | 测试设计、验证脚本与自动化执行报告 |
| [doc/6-review/](doc/6-review/) | 提交前 6-review 风格与结构审查记录 |
