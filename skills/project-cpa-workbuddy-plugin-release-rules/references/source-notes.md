# Source Notes

## 2026-08-23 创建：project-cpa-workbuddy-plugin-release-rules

- **创建原因**：用户点名「把提交、push、构建、registry 等后续一整套发布的流程吸收到项目的 skill」
- **来源**：cpa-workbuddy-plugin 0.12.0 / 0.12.1 / 0.12.2 三次完整发布实操（对话 + .workbuddy/memory/2026-08-22.md + 2026-08-23.md）
- **覆盖**：版本 bump 三处、混合文件分离发布、HTTPS+askpass push、dispatch CI、下载 assets、publish registry、远端验证、12 条实测踩坑
- **关联 skill**：cgo-plugin-isolated-test（本地验证职责，本 skill 引用不重复）；luode-skills 仓库 `project-interface-release-execution-rules` 是接口测试门禁（不同职责域，无重叠）
- **边界**：只管发布执行链路；代码正确性验证、需求/编码规则在其他 skill

## 2026-08-29 更新：补齐 0.14.16 / 0.9.6 / 0.1.9 三插件发布实战新铁律

- **更新原因**：2026-08-29 晚间会话完成 workbuddy-provider 0.14.16（失败筛选功能）/ qoderwork-provider 0.9.6 / traework-provider 0.1.9 三插件并行发布闭环，暴露多条 SKILL.md 未覆盖的坑
- **来源**：.workbuddy/memory/2026-08-29.md + MEMORY.md 铁律 + 会话实操
- **主要变更**：
  - push 由 HTTPS+askpass 改为 SSH 直推（origin 是 SSH remote，Step 7 重写；askpass 降级为遗留回退）
  - dispatch 铁律：inputs 必须同时含 plugin（provider id）+ version，缺 version → Release job skipped（不建 tag/Release，assets 下载 404）；plugin 传源码目录名 → 422
  - CI 轮询上限 11 分钟 → 15 分钟（deadline 900s，实测出现过 12 分钟 run）
  - 新增 Step 13 生产服务器部署：POST /v0/management/plugin-store/<provider-id>/install?version=<x.y.z>（docker cp 与 PUT config 均不触发热重载）
  - 下载脚本已参数化 <provider-id> <version>，item 14 标注已修复
  - qoderwork 状态更新为已全量对齐 workbuddy（2026-08-28）
  - 踩坑清单追加 21-27：TLS 备用通道、authFilePrefix 铁律（traework 面板「暂无账号」第三层根因）、多会话共享工作树 fetch 对齐
  - 插件清单更新为四插件 + 当前版本带（workbuddy 0.14.x / qoderwork 0.9.x / traework 0.1.x / token-usage 0.2.x）
- **验证**：全文回读无过时残留；版本示例统一为 0.14.16

## 2026-08-30 修正：download-release-assets.py 参数顺序（0.14.17 发布实测）

- **纠正自误**：2026-08-29 更新时把下载脚本参数顺序误记为 `<provider-id> <version>`，实际签名是 `<version> [plugin]`——与 publish-assets.py 的 `<plugin> <version>` **互为相反**。0.14.17 发布时传反 → tag 拼错 → Release API 404（症状与「资产未建」一致，已用 Release API 复核排除）。
- **修正位置**：Step 9 命令示例、关键命令速查表、踩坑 item 14。
- **经验**：沉淀脚本用法前先用 `head <script> | grep usage` 核对真实签名，不凭记忆写参数顺序。

## 2026-08-30 补充：store install CDN 边缘滞后误报（traework 0.1.10 发布实测）

- **新增踩坑 item 28**：registry push 后数分钟内 store install 可能报 `plugin_manifest_invalid: direct plugin version not found`（502 毫秒级返回），根因是宿主 Go 客户端命中的 raw CDN 边缘节点缓存滞后（同机 curl 反而看到新版）。宿主 FetchRegistry 无本地缓存（v7.2.129 源码实锤）。处置：等几分钟重试即成功。
- 同时记录 0.1.10 链路「接管并行会话 run」实战：CI run 与发布内容一致时不取消、直接接管后续链路；assets/registry 提交被并行会话抢先时以内容一致为准，不再重复提交。

## 2026-09-02 吸收：修复→发布→生产验证闭环（traework 0.1.28 完整跑通）

- **更新原因**：用户点名「将这个修复+发布+生产检验的流程和使用还有 url 密钥都吸收到这个项目的 skill 中。后续也会使用」。
- **来源**：0.1.28「异步流式宿主流桥打开挂死 → 30s 超时 + 降级插件直连 live 流」修复 + 发布 + 生产验证完整闭环实操（2026-09-01/02）。
- **主要变更（SKILL.md）**：
  - description 扩展：加入「修复→发布→生产验证闭环」「生产真实入口 https://cpa.luode.vip/v1 行为验收（流式 qwen3.8-max 长推理 + stream_id 判定）」「GitHub 上行阻断 SOCKS 隧道绕过」触发语义。
  - 新增 Step 14 生产行为验收（验收样本 / 成功指纹 / 失败处理）、Step 15 凭据纪律与清理（key 只写临时文件用后即删、禁回显原值、management key 提取命令宿主机管道串联）、Step 16 GitHub 上行大流量阻断绕过（生产服务器 SOCKS 隧道 + git socks5h + curl --socks5-hostname + urllib 不认 socks5）。
  - 关键命令速查表新增生产行为验收与网络阻断绕过两行；权责边界加入「生产行为验收」职责。
  - 踩坑清单追加 34-39：修复→发布→生产验证闭环、生产验证样本与成败指纹、凭据纪律、GitHub 上行阻断、生产日志中文回显、/v1/responses 与 /v1/chat/completions 格式差异。
- **整理去重**：新增内容与既有 Step 13 生产部署、踩坑 30（management key 提取）等无逐字重复；Step 15 的 key 提取命令与踩坑 30 互补（30 是容器校验分工，15 是凭据纪律），未造成门控层叠。
- **同域冗余扫描**：范围=本 skill + trae-local-verify-rules + cgo-plugin-isolated-test（发布链 / 本地验证 / 生产验收三域）。发现 0 处逐字重复、0 处门控层叠、0 处散落产物；本 skill 新增生产验收为域专属职责，trae-local-verify 只管本地直连验证、cgo-plugin-isolated-test 只管本地编译验证，无交叉。PASS。
- **环境依赖登记**：本 skill 无环境变量 / 宿主配置依赖新增（SOCKS 隧道依赖的 SSH key / 服务器地址是项目事实，已写入 skill 正文本身，非离开机器失效的配置）。
- **凭据纪律**：URL 与密钥不入 skill 正文原值——Step 14/15/16 仅写占位与提取命令，key 不落盘（正文只提「与 management key 相同，从 config 提取」），符合禁止回显凭据原值的仓库红线。
- **字典刷新**：本项目 skills/ 无 data.js / 字典.md（skill 字典机制属于 luode-skills 仓库，跨项目只读），不适用。
- **验证**：全文回读 + `git diff --check` PASS；UTF-8 读取无乱码。

## 2026-10-07 更新：吸收「发布后清理」为 Step 13.5

- **更新原因**：用户要求"每次发布后都需要清理不需要的垃圾"，把该规则固化到发布链路。
- **来源**：2026-10-07 实操——`release-assets` 累积 3.75 GB，删 185 个历史版本目录 / 1469 文件，保留 registry 当前 6 个版本目录，降到 0.13 GB；提交 `095f4a4` 并推送。
- **主要变更（SKILL.md）**：新增 `### Step 13.5 · 发布后清理`（保留集 = registry 每插件当前版本；`scripts/prune-release-assets.py` 一键清理；只暂存 release-assets 删除；远端 200/404 抽查）；description 增补"发布后清理（Step 13.5）"触发语义；命令速查表加"发布后清理"一行。
- **拆分落点**：细节规则另立项目 skill `project-cpa-workbuddy-plugin-release-asset-prune-rules`，本 skill 只保留一句指针，避免重复维护。
- **整理去重**：Step 13.5 与 Step 13（发布链内临时文件 /tmp、cpa-shim-* 清理）职责互补——13 清发布临时文件，13.5 清仓库 release-assets 资产与本地缓存，无逐字重复。
- **同域冗余扫描**：范围=release-rules + release-asset-prune-rules + trae-local-verify-rules。发现 0 处逐字重复、0 处门控层叠、0 处散落产物。PASS。
- **环境依赖登记**：无新增环境依赖。
- **字典刷新**：本项目 skills/ 无 data.js / 字典.md，不适用。
- **验证**：SKILL.md 全文回读无乱码；`prune-release-assets.py` py_compile OK + dry-run 幂等（0 待删）。

## 2026-10-11 更新：吸收「生产容器重启红线」（非必要不重启）

- **更新原因**：用户要求「非必要不要重启生产的 cpa 容器，插件的更新安装一般不需要重启。这个规则吸收到项目规则和 skill 中」。
- **来源**：生产运维实践——`plugin-store install` 默认返回 `restart_required:false`，热重载即生效；`docker restart cli-proxy-api` 会中断在线流量并触发 60–80 秒冷启动（9 插件 + 模型刷新）。
- **主要变更（SKILL.md）**：Step 13 校验项改为用 `docker logs cli-proxy-api | grep "hot reloaded"` 判定 `active_version`（明确不需要重启容器）；Step 13 新增「非必要不重启生产 cpa 容器（强制，2026-10-11）」条；权责边界末条由「发布完成后提醒用户重启宿主」改为「不主动重启生产 cpa 容器」。
- **同步落点**：仓库级规则 AGENTS.md / CLAUDE.md「项目特有规则 → 仓库与发布」新增「生产容器重启红线（非必要不重启，强制）」；`project-cpa-workbuddy-plugin-host-config-safety-rules` 配置写入红线新增第 7 条（配置损坏恢复为重启的必要例外）。
- **整理去重**：与踩坑 48（偶发不热重载时「不必重启、把时机交用户」）口径一致，新红线是其上位化表达，未逐字重复。
- **字典刷新**：本项目 skills/ 无 data.js / 字典.md，不适用。
- **验证**：SKILL.md 全文回读无乱码，UTF-8 无漂移。

## 2026-10-11 更新：吸收「更新插件不需要手工改数据库表」强制红线

- **更新原因**：用户点名「写进发布 skill」——把 2026-10-11 两次生产事故（外部会话直读直写 `config_store` 导致配置转义损坏）得出的规范固化到发布链路，避免发布过程中再出现同类操作。
- **来源**：2026-10-11 两次生产 CPA 故障实操（`doc/4-bugs/2026-10-11_172000_...md`、`doc/4-bugs/2026-10-11_1812_...md`）。实证：18:04:53 `plugin-store/zcode-provider/install?version=0.1.3` → 200；安装前后两次配置快照反转义后 diff，唯一实质差异 = `zcode-provider` 的 `version: 0.1.2 → 0.1.3` 与 7 个 artifacts，**全部由 CPA 自动写入，零人工介入**。反例：`mysql -sN -e 'select content from cli_proxy_api.config_store where id="config"'` 后经 JSON 二次序列化写回 → 真实换行退化为字面 `\n`（0x5c 0x6e）→ 整篇塌缩单行 → YAML 判空 → `remote-management.secret-key` / `port` 全丢 → `/v0` `/v8` 管理路由全 404、端口漂移、外网 502。
- **主要变更（SKILL.md）**：
  - Step 13 新增子章节「⛔ 更新插件不需要手工改数据库表（强制红线）」：唯一正确路径 = `plugin-store install`；CPA 自动写 `plugins.configs.<id>`；⛔ 禁止 `select content` 后写回；只读可、读完绝不写回；非插件配置走 `PUT /v8/management/config`；损坏铁证（反斜杠再转义一层 `\"`→`\\"`、`char(10)` 真实换行数=1）；配置存储契约速查（`md5(content)` 恒等 spool `md5sum`）；恢复流程指向 `project-cpa-workbuddy-plugin-host-config-safety-rules`。
  - 权责边界新增「不直接读写生产 `config_store` 表」条。
  - 踩坑清单追加 54：直读直写 `config_store` 是生产事故，含 5 条铁律。
- **同步落点**：项目 skill `project-cpa-workbuddy-plugin-host-config-safety-rules`（配置存储契约 + 「更新插件的正确姿势」章节 + 写入红线）、用户级 `config-persistence-safety-rules`、知识库 `cli-proxy-api 宿主配置存储形态` 笔记与 MOC、`PROJECT_MEMORY.md`、`.workbuddy-ai/memory/2026-10-11.md`。
- **整理去重**：新子章节与 Step 13 既有「非必要不重启」条互补（一条管重启、一条管配置写入），与踩坑 25（唯一部署路径）同域但不重复（25 讲「为什么不走 docker cp / PUT config」，本条讲「为什么不动数据库表」）。同域冗余扫描范围 = release-rules + host-config-safety-rules，发现 0 处逐字重复、0 处门控层叠（本 skill 讲「发布时不要做什么」，host-config-safety 讲「坏了怎么修」）。PASS。
- **环境依赖登记**：无新增环境依赖（服务器地址 / SSH key / 库表名为项目事实，已由既有章节承载）。
- **凭据纪律**：未回显任何密码 / key 原值；示例命令只用占位符。
- **字典刷新**：本项目 skills/ 无 data.js / 字典.md，不适用。
- **验证**：SKILL.md 全文回读无乱码；UTF-8 无漂移。

## 2026-10-11 更新：吸收「取消冗余 push CI 提速 + 轮询 urlopen 超时 + Windows SSH 二进制流防 CRLF」

- **更新原因**：2026-10-11 连续发布 `qoderwork-provider 0.9.29`、`qoder-ai-provider 0.1.8`、`traework-provider 0.2.6/0.2.8` 实战总结。
- **主要变更（SKILL.md）**：
  - 踩坑清单追加 55：取消 `event=push` 冗余 CI 运行释放并发 Runner（发布从 30m 缩至 7.5m）+ `urllib.request.urlopen(r, timeout=20)` 防半开连接挂死；
  - 踩坑清单追加 56：Windows `subprocess.Popen(["ssh", "cpa-server", "bash"])` 必须传 `bytes`（禁 `text=True` 防 `\r\n` 注入）并首行声明完整 `PATH`。
- **验证**：SKILL.md 全文回读无乱码；UTF-8 无漂移。


## 2026-10-18 更新：资产迁到 GitHub Release，发布链路合并为两次推送

- **更新原因**：用户要求「优化发布速度」；实测单次 dispatch 85 个 job、端到端 17–27 分钟，且每次发布额外推送 18MB 资产。
- **主要变更（SKILL.md）**：
  - Step 9 合并「下载 + checksums 校验」，明确产物只是本地缓存（已被 `.gitignore` 忽略，不入仓）。
  - Step 10 合并「publish + 单次 commit + push」，`publish-assets.py` 生成的 URL 指向 `releases/download/<id>-v<version>/<zip>`。
  - 删除原 Step 10（提交 assets）与原 Step 11 的独立编号，后续步骤顺延：远端验证改为 Step 11。
  - Step 13.5 由「发布后清理（必做）」改为「本地缓存清理（可选）」。
  - 踩坑 2（0.9.7 assets 必须先 push）标注为失效；踩坑 44 去掉 `git add` 说明；踩坑 52 的落点由「提交 assets」改为「publish 进 registry」。
- **同步落点**：`scripts/publish-assets.py`（新增 `--all-current`）、`scripts/prune-release-assets.py`（删除）、`.gitignore`、`registry.json`。
- **验证**：72 个 artifact URL 全部 200 且 sha256/size 一致；`git ls-files release-assets` 为 0。
