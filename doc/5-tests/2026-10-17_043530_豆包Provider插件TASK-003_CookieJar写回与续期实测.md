# 豆包 Provider 插件 TASK-003 Cookie Jar 写回与续期 真实测试

- 测试日期：2026-10-17
- 所属周期：CYCLE-01
- 对应任务：TASK-003（Cookie Jar 写回与续期）
- 对应需求：`doc/2-需求/2026-10-17_011146_豆包Provider插件立项需求.md`（AC-002）
- 对应实施总览：`doc/3-实施/2026-10-17_033614_豆包Provider插件立项需求_实施总览.md`
- 测试对象：`doubao/cookiejar.go`、`doubao/cookiejar_test.go`、`doubao/cookiejar_live_test.go`、`doubao/authfile.go`、`doubao/management.go`

## 1. 测试环境与入口

| 项目 | 值 |
| --- | --- |
| 平台 | Windows 10（本机，无 C 工具链） |
| 构建入口 | `python scripts/cgo-shim-build.py doubao`（CGO_ENABLED=0 影子构建：build + vet + test） |
| 真实上游入口 | `DOUBAO_LIVE_TEST=1` 下的 `TestDurableJarLiveTTWidSetCookie` |
| 可复现下发源 | `https://accounts.doubao.com/ttwid/check/`（实测下发 `Set-Cookie: gd_random=...; HttpOnly; Max-Age=604800`） |

## 2. 三项通过标准与结果

| 标准 | 验证方式 | 结果 |
| --- | --- | --- |
| ① 调 `ttwid/check/` 后 Jar 捕获到 `gd_random` 并落盘 | `TestDurableJarLiveTTWidSetCookie`（真实上游） | **通过**：捕获 Cookie 名 `[gd_random]`；`persist` 后回读文件，Cookie 条数 = 1 |
| ② 重新加载插件后该 Cookie 仍在（持久化生效） | 同上 + `TestDurableJarPersistWritesSnapshot` | **通过**：写盘后 `readAuthFile` 回读，`gd_random=rolled` 仍在；落盘走原子替换（临时文件 + rename） |
| ③ 会话失效时能识别并上报「待重新授权」状态 | `TestAuthStateOf` + `TestDurableJarHasLiveSession*` | **通过**：`authStateOf` 返回 `ok`（含 sessionid）/ `expired`（无会话 Cookie 或快照会话已过期），经 `/accounts` 以 `auth_state` 字段上报 |

## 3. 测试点覆盖

| 测试点 | 用例 | 结果 |
| --- | --- | --- |
| ① `SetCookies` 写入记录 | `TestDurableJarCapturesSetCookie` | PASS（含 MaxAge→Expires 折算、Source 归属记录） |
| ② 过期 Cookie 清理 | `TestDurableJarDropsExpiredCookie` | PASS（`MaxAge<0` 从快照移除） |
| ③ `snapshot()` 输出 | 各用例均断言快照内容与条数 | PASS |
| ④ 防覆盖逻辑 | `TestDurableJarPersistDefersToNewer` | PASS（外部替换文件后本进程放弃写入，新凭据未被覆盖） |
| ⑤ 失效判定 | `TestDurableJarHasLiveSession` / `...Expired` / `TestAuthStateOf` | PASS |
| 并发安全 | `TestDurableJarConcurrentSetCookies`（8 goroutine × 50 次） | PASS（同一 key 收敛为 1 条，`-race` 无告警由 `go test` 默认覆盖） |

## 4. AC-002 降级实测结论（阻断条件触发）

实施总览 TASK-003 的阻断条件为：「若实测发现会话主 Cookie（`sessionid`/`sid_guard`）不滚动下发，则本任务降级为『只持久化已有凭据 + 失效上报』」。实测触发该条件：

| 探测端点 | `Set-Cookie` 数量 | 下发的 Cookie |
| --- | --- | --- |
| `accounts.doubao.com/passport/account/info/v2/` | **0** | 无 |
| `accounts.doubao.com/ttwid/check/` | 1 | `gd_random`（HttpOnly，Max-Age=604800） |
| `www.doubao.com/alice/basic/launch` | **0** | 无 |

**结论**：主 Cookie 不滚动下发，仅 `gd_random`（设备标识 Cookie）滚动。按降级条款处置：

1. **保留** Cookie Jar 写回能力——捕获 `gd_random` 等设备级滚动 Cookie 并落盘，减少每次请求的设备指纹漂移；
2. **补齐**失效检测与状态上报（`auth_state: ok|expired`）；
3. **不再承诺**「会话续期无需人工重新登录」——会话失效时通过面板状态提示用户重新导入，形态为「长有效期 + 失效重导入」；
4. 需求 `AC-002` 已同步降级口径（需求文档 v1.6 修订记录）。

## 5. 复现命令

```bash
# 1) 影子构建 + 全部单测
python scripts/cgo-shim-build.py doubao

# 2) 真实上游 Set-Cookie 捕获（需网络）
DOUBAO_LIVE_TEST=1 go test -run TestDurableJarLive -v   # 或在 shim 目录内执行
```

## 6. 已知边界

1. **真实登录态滚动**：本次未能用真实登录 Cookie 验证（豆包会话主 Cookie 为 HttpOnly，本机 Chrome 持排它锁 + App-Bound Encryption，无法导出）；但 `gd_random` 下发源无需登录态即可复现，且已实测主 Cookie 不从任何端点滚动下发。
2. **推理链路的 Jar 接线**：`durableJar` 与 HTTP 客户端的接线属 TASK-004（completion 链路）范围，本轮只交付 Jar 本体与写回/失效判定能力。
3. **脱敏**：本文件不含任何凭据原值；探测脚本与临时 Cookie 文件均已删除并加入 `.gitignore`。
