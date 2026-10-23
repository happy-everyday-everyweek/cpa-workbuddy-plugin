#!/usr/bin/env python3
"""probe_growth_endpoints.py — 成长中心端点契约探针（只读优先）。

用途：在写任何 Go 实现前，用真实 CN 账号测出每个成长中心端点的
真实域名 / 路径 / 方法 / 可用性，冻结成契约矩阵。

背景：旧版 0.16.x 全部端点假设 codebuddy.cn/v2/activity/growth/*，但
外部参考项目实测连登与领奖走 www.workbuddy.cn/activity/growth/*（无 /v2）。
两条路径冲突，必须实测而不能靠猜。

两种模式：
  --readonly（默认）  只发 GET，不改变任何账号状态。
  --write-once        额外发两条幂等写请求：tasks/accept（accept 只是报名，
                      不产生进度）与单次 report。绝不调用 claim / redeem /
                      lottery / makeup-cards —— 那些会真实消耗奖励与次数。

凭据纪律：脚本只读取 auth 文件，输出中 uid 一律掩码，access token 永不打印。

用法：
    python scripts/probe_growth_endpoints.py --auth-file <path> [--readonly]
    python scripts/probe_growth_endpoints.py --auth-file <path> --write-once

退出码：0 全部探测完成；1 参数或凭据错误；2 无可探测端点。
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

# 双 base 候选：旧版实现用 A，外部参考项目用 B。两者都要测，结果决定契约。
BASE_CODEDUDDY_V2 = "https://www.codebuddy.cn/v2/activity/growth"
BASE_WORKBUDDY = "https://www.workbuddy.cn/activity/growth"

# 只读端点：GET，不改变账号状态。
READONLY_PATHS: list[tuple[str, str]] = [
    ("连登状态", "/streak"),
    ("签到热力图", "/heatmap"),
    ("Buddy 能量", "/energy"),
    ("兑换状态", "/redeem/summary"),
    ("抽奖次数", "/lottery/chances"),
    ("任务列表", "/tasks"),
    ("旅行状态", "/buddy/travel/status"),
    ("旅行目的地", "/buddy/travel/config"),
    ("Buddy 额度", "/buddy/quota"),
]

# 活跃上报走 billing 域（经验证在 codebuddy.cn 根路径下，不在 growth 前缀里）。
REPORT_URL = "https://www.codebuddy.cn/v2/report"

# 账号凭据文件默认查找位置（含导出包与插件 auth 目录两种形态）。
DEFAULT_AUTH_GLOBS = [
    os.path.expanduser("~/.antigravity_cockpit/workbuddy_accounts"),
    os.path.expanduser("~/Downloads"),
]


def mask_uid(uid: str) -> str:
    """把 uid 掩码成首 6 位 + 省略号，避免证据里出现完整账号标识。"""
    uid = str(uid or "")
    if len(uid) <= 6:
        return uid or "-"
    return uid[:6] + "..."


def load_auth_document(path: str, prefer_uid: str = "") -> dict:
    """读取 auth 文件并归一化成单个账号文档。

    支持两种形态：
      1. 插件物理 auth 文件：{type, provider, auth:{...}, account:{...}}
      2. 导出包：{accounts:[{credential:{auth,account,...}, region, uid}]}

    导出包里会优先挑「未禁用 + token 未过期」的 CN 账号，避免拿已禁用账号
    探出满屏 401 而误判端点不存在。

    [参数] path：auth 文件路径；prefer_uid：期望账号 uid 的前缀（可选）。
    [返回] 含 auth / account 两个子对象的账号文档。
    """
    with open(path, "r", encoding="utf-8") as handle:
        doc = json.load(handle)

    # 形态 2：导出包，挑一个可用的 CN 账号。
    if isinstance(doc, dict) and isinstance(doc.get("accounts"), list):
        now = int(time.time())
        fallback = None
        for item in doc["accounts"]:
            if str(item.get("region", "")).lower() != "cn":
                continue
            cred = item.get("credential")
            if not isinstance(cred, dict) or not cred.get("auth"):
                continue
            uid = str(item.get("uid") or (cred.get("account") or {}).get("uid") or "")
            if prefer_uid and not uid.startswith(prefer_uid):
                continue
            if fallback is None:
                fallback = cred
            expires = int((cred.get("auth") or {}).get("expiresAt") or 0)
            if not cred.get("disabled") and expires > now:
                return cred
        if fallback is not None:
            return fallback
        raise SystemExit(f"[probe] {path} 中没有可用的 CN 账号")

    # 形态 1：单账号文档。
    if isinstance(doc, dict) and isinstance(doc.get("auth"), dict):
        return doc

    raise SystemExit(f"[probe] 无法识别的 auth 文件结构：{path}")


def build_headers(account: dict) -> dict:
    """构造上游鉴权头：Bearer + 一组身份头（与插件 billingHeaders 同口径）。

    [参数] account：含 auth / account 子对象的账号文档。
    [返回] HTTP 头字典；缺失字段不写入，避免发送空头。
    """
    auth = account.get("auth") or {}
    info = account.get("account") or {}
    headers = {
        "Authorization": "Bearer " + str(auth.get("accessToken", "")),
        "Accept": "application/json",
        "Content-Type": "application/json",
    }
    for header, key in (
        ("X-User-Id", "uid"),
        ("X-Enterprise-Id", "enterpriseId"),
        ("X-Tenant-Id", "enterpriseId"),
        ("X-Domain", "domain"),
    ):
        value = info.get(key) or auth.get(key)
        if value:
            headers[header] = str(value)
    return headers


def probe_once(url: str, method: str, headers: dict, body: bytes | None) -> tuple[int, str]:
    """发送一次探测请求，返回状态码与响应正文摘要（已脱敏截断）。

    [参数] url：完整端点 URL；method：GET/POST；headers：鉴权头；body：请求体。
    [返回] (HTTP 状态码, 截断后的响应正文)；网络失败时状态码为 0。
    """
    request = urllib.request.Request(url, data=body, method=method, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=25) as response:
            raw = response.read()
            return response.status, summarize_body(raw)
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        return exc.code, summarize_body(raw)
    except Exception as exc:  # 网络层失败：状态码 0 表达「未拿到响应」
        return 0, f"{type(exc).__name__}: {exc}"


def summarize_body(raw: bytes) -> str:
    """把响应正文压成单行摘要：解 JSON 取 envelope 的 code/msg 与 data 顶层键。

    [参数] raw：响应原始字节。
    [返回] 形如 `code=0 msg=OK keys=[streak,tasks]` 的单行摘要。
    """
    text = raw[:4000].decode("utf-8", errors="replace")
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        return "non-json: " + text[:160].replace("\n", " ")
    if not isinstance(parsed, dict):
        return "json-array len=" + str(len(parsed))
    code = parsed.get("code")
    msg = str(parsed.get("msg", ""))[:80]
    data = parsed.get("data")
    keys = sorted(data.keys())[:12] if isinstance(data, dict) else type(data).__name__
    return f"code={code} msg={msg} keys={keys}"


def print_matrix(auth_path: str, write_once: bool, prefer_uid: str = "") -> int:
    """主流程：读账号 → 逐个 base 探只读端点 → 可选单次写 → 打印矩阵。

    [参数] auth_path：auth 文件路径；write_once：是否补测幂等写端点；
           prefer_uid：期望账号 uid 前缀。
    [返回] 进程退出码。
    """
    account = load_auth_document(auth_path, prefer_uid)
    info = account.get("account") or {}
    headers = build_headers(account)
    print(f"[probe] 账号 uid={mask_uid(info.get('uid'))} 昵称={info.get('nickname', '-')}")
    print(f"[probe] 模式={'write-once' if write_once else 'readonly'}")
    print()

    for base_label, base in (("A codebuddy/v2", BASE_CODEDUDDY_V2), ("B workbuddy", BASE_WORKBUDDY)):
        print(f"=== base {base_label}: {base} ===")
        for label, path in READONLY_PATHS:
            status, summary = probe_once(base + path, "GET", headers, None)
            print(f"  [{status:>3}] {label:<12} GET  {path}")
            print(f"        {summary}")
        print()

    if not write_once:
        print("[probe] readonly 模式结束（未发送任何写请求）")
        return 0

    # 幂等写 1：accept 只是把 not_accepted 改成 accepted，不产生进度。
    # 先取任务码，仅接单前 3 个未接单任务，控制影响面。
    status, summary = probe_once(BASE_CODEDUDDY_V2 + "/tasks", "GET", headers, None)
    print(f"=== write-once ===\n  [{status:>3}] 取任务码 GET /tasks")
    task_codes = extract_pending_codes(BASE_CODEDUDDY_V2, headers)
    if task_codes:
        body = json.dumps({"task_codes": task_codes}).encode("utf-8")
        status, summary = probe_once(BASE_CODEDUDDY_V2 + "/tasks/accept", "POST", headers, body)
        print(f"  [{status:>3}] 接单(幂等) POST /tasks/accept {task_codes}")
        print(f"        {summary}")
    else:
        print("  [skip] 无未接单任务，跳过 accept")

    # 幂等写 2：单次活跃上报。userId 必填，缺失会被上游静默丢弃。
    event = build_report_event(info)
    status, summary = probe_once(REPORT_URL, "POST", headers, json.dumps([event]).encode("utf-8"))
    print(f"  [{status:>3}] 活跃上报 POST /v2/report（单次）")
    print(f"        {summary}")

    # 回读校验：上报后进度异步结算，等 10s 再读一次任务列表做前后对比。
    print("\n[probe] 等待 10s 让上报结算，然后回读任务进度…")
    time.sleep(10)
    after = fetch_task_progress(BASE_CODEDUDDY_V2, headers)
    for code, (current, target) in sorted(after.items())[:10]:
        print(f"  {code:<28} current={current} target={target}")
    return 0


def extract_pending_codes(base: str, headers: dict) -> list[str]:
    """取任务列表里未接单且未锁定的前 3 个任务码。

    [参数] base：growth base；headers：鉴权头。
    [返回] 任务码列表；取不到时返回空列表。
    """
    request = urllib.request.Request(base + "/tasks", method="GET", headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=25) as response:
            data = json.load(response).get("data") or {}
    except Exception:
        return []
    codes = []
    for task in data.get("tasks") or []:
        if task.get("locked"):
            continue
        if task.get("accept_status") == "not_accepted" and task.get("task_code"):
            codes.append(task["task_code"])
        if len(codes) >= 3:
            break
    return codes


def fetch_task_progress(base: str, headers: dict) -> dict[str, tuple[int, int]]:
    """回读任务进度，返回 {task_code: (current, target)}。

    [参数] base：growth base；headers：鉴权头。
    [返回] 任务码到 (current, target) 的映射。
    """
    request = urllib.request.Request(base + "/tasks", method="GET", headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=25) as response:
            data = json.load(response).get("data") or {}
    except Exception:
        return {}
    progress = {}
    for task in data.get("tasks") or []:
        code = task.get("task_code")
        if not code:
            continue
        nested = task.get("progress")
        if isinstance(nested, dict):
            progress[code] = (int(nested.get("current") or 0), int(nested.get("target") or 0))
        else:
            progress[code] = (int(task.get("current") or 0), int(task.get("target") or 0))
    return progress


def build_report_event(info: dict) -> dict:
    """构造 chat_request_send 活跃上报事件（字段形状对齐参考项目实测口径）。

    [参数] info：账号信息（需含 uid）。
    [返回] 单条事件字典；userId 缺失会被上游静默丢弃，故必填。
    """
    now_ms = int(time.time() * 1000)
    conv = f"wbprobe-{now_ms}"
    return {
        "eventCode": "chat_request_send",
        "timestamp": now_ms,
        "reportDelay": 0,
        "mode": "craft",
        "conversationId": conv,
        "requestId": conv,
        "inputLength": 12,
        "requestModelId": "deepseek-v4-flash",
        "requestModelName": "DeepSeek V4 Flash",
        "isPlan": False,
        "isAutoExecuteTerminal": False,
        "isAutoModify": False,
        "codebaseEnable": False,
        "maxToken": 0,
        "maxSteps": 0,
        "temperature": 0,
        "maxRetries": 0,
        "mentionContexts": [],
        "knowledgeId": [],
        "knowledgeName": [],
        "codebaseId": "",
        "mentionContextCount": 0,
        "command": "",
        "expertId": "",
        "recommendId": "",
        "skillId": "",
        "skillCount": 0,
        "totalCount": 0,
        "fileUri": "",
        "presentAt": now_ms,
        "traceId": "",
        "rootRequestId": conv,
        "parentConversationId": conv,
        "agentName": "default",
        "agentType": "conversation",
        "userId": str(info.get("uid") or ""),
    }


def main() -> int:
    """命令行入口：解析参数并执行探测。

    [参数] 无（从 argv 读取）。
    [返回] 进程退出码。
    """
    parser = argparse.ArgumentParser(description="成长中心端点契约探针")
    parser.add_argument("--auth-file", required=True, help="workbuddy auth 文件路径")
    parser.add_argument("--readonly", action="store_true", help="只发 GET（默认行为，显式声明用）")
    parser.add_argument("--write-once", action="store_true", help="补测幂等写端点（accept + report）")
    parser.add_argument("--uid-prefix", default="", help="按 uid 前缀指定账号（导出包内多账号时用）")
    args = parser.parse_args()
    if args.readonly and args.write_once:
        print("[probe] --readonly 与 --write-once 互斥", file=sys.stderr)
        return 1
    if not os.path.isfile(args.auth_file):
        print(f"[probe] auth 文件不存在：{args.auth_file}", file=sys.stderr)
        return 1
    return print_matrix(args.auth_file, args.write_once, args.uid_prefix)


if __name__ == "__main__":
    sys.exit(main())
