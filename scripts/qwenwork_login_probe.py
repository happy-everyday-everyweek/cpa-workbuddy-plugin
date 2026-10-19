#!/usr/bin/env python3
"""
QwenWork（千问办公）登录闭环探针

用途：在正式实现 qwenwork-provider 之前，用真实账号验证三段链路是否成立。
  1. Device Flow + PKCE 登录：拿到宿主 access token 与 refresh token
  2. POST /api/desktop-sessions：用宿主 token 换 MaaS access key（含 base_url 下发值）
  3. 用 MaaS key 探测模型清单，并对 OpenAI chat 与 Anthropic messages 两种协议各打一次真实推理

用法:
  python scripts/qwenwork_login_probe.py login     # 打印授权链接，浏览器完成登录后自动落盘凭据
  python scripts/qwenwork_login_probe.py mint      # 复用已存凭据，换取 MaaS key 并打印下发 base_url
  python scripts/qwenwork_login_probe.py probe     # mint + 模型清单 + 双协议最小推理
  python scripts/qwenwork_login_probe.py all       # login → probe 一条龙

产物（均不入库）:
  .qwenwork-probe/credentials.json  宿主 token（access/refresh/expiry）
  .qwenwork-probe/maas.json         MaaS key（access/expiry/base_url）
  .qwenwork-probe/probe.log         全流程日志

安全约束:
  - 日志与终端只输出 token 指纹（sha256 前 12 位）与长度，绝不回显原值
  - 不读取、不解密客户端 auth.dat/auth-v2.dat（DPAPI 绑定本机用户，容器内不可用）
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import secrets
import string
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from urllib.parse import quote, urlencode

# 端点与客户端常量：来自 app.asar 的 out/main/main.js（客户端 1.3.1-26101419）
#
# 登录链路（2026-10-17 只读探测确认）：
#   1. 浏览器打开 AUTH_BASE + /device/selectAccounts?...&nonce=...&machine_id=...
#      注意 AUTH_BASE 必须是 gateway 域名：同路径在 qwenwork.cn 上是 404（2026-10-17 对照实测）
#   2. 该页 302 到 qwenwork.cn/oauth2/auth，网关重新生成自己的 PKCE，
#      redirect_uri 指向 gateway.qwenwork.cn/oauth/callback，scope 含 qwen_work
#   3. 用户在网页完成登录后，网关内部把 code 换成宿主 token 并绑定到 nonce
#   4. 本脚本轮询 GATEWAY_BASE + /api/v1/deviceToken/poll，拿 token + refresh_token
# 因此探针只需要轮询，不需要参与第 2、3 步的 PKCE 交换。
GATEWAY_BASE = "https://gateway.qwenwork.cn"
AUTH_BASE = "https://gateway.qwenwork.cn"
CLIENT_ID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
REDIRECT_URI = "qwenwork-cn://"
AUTH_SELECT_PATH = "/device/selectAccounts"
POLL_PATH = "/api/v1/deviceToken/poll"
REFRESH_PATH = "/api/v1/deviceToken/refresh"
MINT_PATH = "/api/desktop-sessions"
DEFAULT_MAAS_BASE = "https://maas.xiaoluozi.cn"

# 客户端身份头：宿主每次请求都会带，用于验证网关是否强校验
CLIENT_VERSION = "1.3.1"
RELEASE_VERSION = "1.3.1-26101419"
PLATFORM = "win32"
ARCH = "x64"

PROBE_DIR = Path(__file__).resolve().parent.parent / ".qwenwork-probe"
CRED_PATH = PROBE_DIR / "credentials.json"
MAAS_PATH = PROBE_DIR / "maas.json"
LOG_PATH = PROBE_DIR / "probe.log"

POLL_INTERVAL_S = 2
POLL_TIMEOUT_S = 900


def log(message: str) -> None:
    """写一行带时间戳的日志，同时落盘与打印。"""
    line = f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {message}"
    # 终端可能处于 GBK 代码页，打印失败时降级为转义输出，日志文件始终 UTF-8
    try:
        print(line, flush=True)
    except UnicodeEncodeError:
        print(line.encode("utf-8", "replace").decode("ascii", "backslashreplace"), flush=True)
    PROBE_DIR.mkdir(parents=True, exist_ok=True)
    with LOG_PATH.open("a", encoding="utf-8") as fh:
        fh.write(line + "\n")


def fingerprint(value: str) -> str:
    """返回凭据指纹，避免日志出现原值。"""
    if not value:
        return "<empty>"
    digest = hashlib.sha256(value.encode("utf-8")).hexdigest()[:12]
    return f"{digest}/len={len(value)}"


def client_headers(extra: dict | None = None) -> dict:
    """构造千问办公客户端身份头。"""
    headers = {
        "Accept": "application/json",
        "User-Agent": f"qoderwork/{CLIENT_VERSION}",
        "X-QwenWork-Version": CLIENT_VERSION,
        "X-QwenWork-Release-Version": RELEASE_VERSION,
        "X-QwenWork-Build": "26101419",
        "X-QwenWork-Platform": PLATFORM,
        "X-QwenWork-Arch": ARCH,
        "X-QwenWork-Channel": "stable",
        "X-QwenWork-App-Mode": "default",
    }
    if extra:
        headers.update(extra)
    return headers


def http_request(method: str, url: str, headers: dict | None = None, body: dict | None = None,
                 timeout: int = 30) -> tuple[int, dict, bytes]:
    """发一次 HTTP 请求，返回 (状态码, 响应头, 响应体)。"""
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, dict(resp.headers), resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, dict(exc.headers), exc.read()


def generate_pkce() -> tuple[str, str]:
    """生成 PKCE verifier 与 S256 challenge。"""
    alphabet = string.ascii_letters + string.digits + "-._~"
    verifier = "".join(secrets.choice(alphabet) for _ in range(64))
    digest = hashlib.sha256(verifier.encode("ascii")).digest()
    challenge = base64.urlsafe_b64encode(digest).decode("ascii").rstrip("=")
    return verifier, challenge


def load_json(path: Path) -> dict:
    """读取 JSON 文件，不存在时返回空字典。"""
    if not path.exists():
        return {}
    return json.loads(path.read_text(encoding="utf-8"))


def save_json(path: Path, payload: dict) -> None:
    """原子写入 JSON 文件。"""
    PROBE_DIR.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    os.replace(tmp, path)


def cmd_login(_: argparse.Namespace) -> int:
    """执行 Device Flow 登录并落盘宿主凭据。"""
    verifier, challenge = generate_pkce()
    nonce = str(uuid.uuid4())
    # 客户端使用纯 UUID 作为 machine_id（machine-id 文件内容即 randomUUID），不要加前缀
    machine_id = str(uuid.uuid4())
    query = urlencode({
        "challenge": challenge,
        "challenge_method": "S256",
        "nonce": nonce,
        "machine_id": machine_id,
        "client_id": CLIENT_ID,
        "redirect_uri": REDIRECT_URI,
    })
    auth_url = f"{AUTH_BASE}{AUTH_SELECT_PATH}?{query}"
    log("请用浏览器打开以下链接完成登录（900 秒内）：")
    log(auth_url)
    log(f"nonce={nonce} machine_id={machine_id}")

    poll_url = f"{GATEWAY_BASE}{POLL_PATH}?{urlencode({'nonce': nonce, 'verifier': verifier, 'challenge_method': 'S256'})}"
    deadline = time.time() + POLL_TIMEOUT_S
    attempt = 0
    while time.time() < deadline:
        attempt += 1
        status, headers, raw = http_request("GET", poll_url, client_headers({"X-Request-Id": str(uuid.uuid4())}))
        state = headers.get("x-device-flow-state", "unknown")
        if status == 404:
            if attempt % 10 == 1:
                log(f"轮询第 {attempt} 次：等待授权（state={state}）")
            time.sleep(POLL_INTERVAL_S)
            continue
        if status != 200:
            log(f"轮询失败：http={status} state={state} body={raw[:200].decode('utf-8', 'replace')}")
            return 2
        payload = json.loads(raw.decode("utf-8"))
        token = payload.get("token", "")
        if not token:
            time.sleep(POLL_INTERVAL_S)
            continue
        if payload.get("nonce") != nonce:
            log(f"登录绑定校验失败：nonce 不匹配（返回={payload.get('nonce')}）")
            return 3
        save_json(CRED_PATH, {
            "access_token": token,
            "refresh_token": payload.get("refresh_token", ""),
            "expires_at": payload.get("expires_at", ""),
            "refresh_token_expires_at": payload.get("refresh_token_expires_at", ""),
            "nonce": nonce,
            "client_id": CLIENT_ID,
            "created_at": int(time.time()),
        })
        log(f"登录成功：access={fingerprint(token)} refresh={fingerprint(payload.get('refresh_token', ''))}")
        log(f"access 过期时间={payload.get('expires_at')} refresh 过期时间={payload.get('refresh_token_expires_at')}")
        return 0
    log("登录超时：未在 900 秒内完成授权")
    return 4


def refresh_host_token(cred: dict) -> dict | None:
    """用 refresh token 换新的宿主 access token。"""
    refresh_token = cred.get("refresh_token", "")
    if not refresh_token:
        return None
    status, _, raw = http_request(
        "POST", f"{GATEWAY_BASE}{REFRESH_PATH}",
        client_headers({"Content-Type": "application/json"}),
        {"refresh_token": refresh_token, "target": "c"},
    )
    if status != 200:
        log(f"宿主 token 刷新失败：http={status} body={raw[:200].decode('utf-8', 'replace')}")
        return None
    payload = json.loads(raw.decode("utf-8"))
    access = payload.get("device_token") or payload.get("token") or ""
    new_refresh = payload.get("refresh_token") or refresh_token
    if not access:
        log("宿主 token 刷新返回空 access token")
        return None
    cred.update({"access_token": access, "refresh_token": new_refresh,
                 "expires_at": payload.get("expires_at", cred.get("expires_at"))})
    save_json(CRED_PATH, cred)
    log(f"宿主 token 已刷新：access={fingerprint(access)}")
    return cred


def mint_maas_key(access_token: str, session_id: str | None = None) -> dict | None:
    """用宿主 access token 换取 MaaS key，返回含下发 base_url 的凭据。"""
    body = {"session_id": session_id} if session_id else {}
    status, headers, raw = http_request(
        "POST", f"{GATEWAY_BASE}{MINT_PATH}",
        client_headers({"Authorization": f"Bearer {access_token}", "Content-Type": "application/json"}),
        body,
    )
    text = raw.decode("utf-8", "replace")
    # 实测签发成功返回 201 Created，不能只认 200
    if not 200 <= status < 300:
        log(f"MaaS 签发失败：http={status} www-authenticate={headers.get('WWW-Authenticate')} body={text[:300]}")
        return None
    payload = json.loads(text)
    key = payload.get("access_token", "")
    if not key:
        log(f"MaaS 签发返回空 key：{text[:300]}")
        return None
    result = {
        "access_token": key,
        "expires_at_ms": payload.get("expires_at_ms", 0),
        "ttl_ms": payload.get("ttl_ms", 0),
        "session_id": payload.get("session_id", ""),
        "base_url": (payload.get("base_url") or "").strip() or DEFAULT_MAAS_BASE,
        "minted_at": int(time.time()),
    }
    save_json(MAAS_PATH, result)
    log(f"MaaS key 签发成功：key={fingerprint(key)} ttl_ms={result['ttl_ms']} "
        f"expires_at_ms={result['expires_at_ms']} session_id={result['session_id'] or '<empty>'}")
    log(f"服务端下发 base_url={result['base_url']}")
    return result


def maas_headers(key: str) -> dict:
    """构造 MaaS 推理请求头（裸 Bearer，无签名）。"""
    return {"Authorization": f"Bearer {key}", "Content-Type": "application/json", "Accept": "application/json"}


def cmd_mint(_: argparse.Namespace) -> int:
    """复用已存宿主凭据换取 MaaS key。"""
    cred = load_json(CRED_PATH)
    access = cred.get("access_token", "")
    if not access:
        log("缺少宿主凭据，请先执行 login")
        return 2
    result = mint_maas_key(access)
    return 0 if result else 5


def probe_models(base_url: str, key: str) -> list[str]:
    """探测 MaaS 模型清单，返回模型 id 列表。"""
    for path in ("/v1/models", "/models"):
        status, _, raw = http_request("GET", f"{base_url}{path}", maas_headers(key))
        text = raw.decode("utf-8", "replace")
        log(f"模型清单 {path}：http={status} body={text[:400]}")
        if status != 200:
            continue
        try:
            payload = json.loads(text)
        except json.JSONDecodeError:
            continue
        items = payload.get("data") or payload.get("models") or []
        ids = [item.get("id") for item in items if isinstance(item, dict) and item.get("id")]
        if ids:
            return ids
    return []


def probe_openai_chat(base_url: str, key: str, model: str) -> None:
    """用 OpenAI chat 协议打一次最小流式推理。"""
    body = {
        "model": model,
        "messages": [{"role": "user", "content": "只回复 OK 两个字母"}],
        "max_tokens": 16,
        "stream": True,
    }
    status, _, raw = http_request("POST", f"{base_url}/v1/chat/completions", maas_headers(key), body, timeout=60)
    text = raw.decode("utf-8", "replace")
    log(f"OpenAI chat 探测（{model}）：http={status} 前 400 字节={text[:400]!r}")


def probe_anthropic_messages(base_url: str, key: str, model: str) -> None:
    """用 Anthropic messages 协议打一次最小流式推理。"""
    body = {
        "model": model,
        "messages": [{"role": "user", "content": "只回复 OK 两个字母"}],
        "max_tokens": 16,
        "stream": True,
    }
    headers = maas_headers(key)
    headers["anthropic-version"] = "2023-06-01"
    status, _, raw = http_request("POST", f"{base_url}/v1/messages", headers, body, timeout=60)
    text = raw.decode("utf-8", "replace")
    log(f"Anthropic messages 探测（{model}）：http={status} 前 400 字节={text[:400]!r}")


def cmd_probe(args: argparse.Namespace) -> int:
    """mint + 模型清单 + 双协议最小推理。"""
    cred = load_json(CRED_PATH)
    if not cred.get("access_token"):
        log("缺少宿主凭据，请先执行 login")
        return 2
    maas = load_json(MAAS_PATH)
    if not maas.get("access_token") or not args.reuse:
        maas = mint_maas_key(cred["access_token"])
        if not maas:
            return 5
    base_url = maas["base_url"].rstrip("/")
    key = maas["access_token"]

    models = probe_models(base_url, key)
    if models:
        log(f"模型总数={len(models)} 样例={models[:10]}")
    else:
        log("未取到模型清单，改用已知模型名继续协议探测")

    openai_model = args.openai_model or next((m for m in models if m.startswith(("glm", "deepseek", "kimi"))), "glm-5.2")
    anthropic_model = args.anthropic_model or next((m for m in models if m.startswith("qwen3.8")), "qwen3.8-max")
    probe_openai_chat(base_url, key, openai_model)
    probe_anthropic_messages(base_url, key, anthropic_model)
    return 0


def cmd_all(args: argparse.Namespace) -> int:
    """登录后直接进入探测。"""
    code = cmd_login(args)
    if code != 0:
        return code
    return cmd_probe(args)


def main() -> int:
    """命令行入口。"""
    parser = argparse.ArgumentParser(description="QwenWork 登录闭环探针")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("login", help="Device Flow 登录并落盘宿主凭据")
    sub.add_parser("mint", help="用宿主凭据换 MaaS key")
    probe = sub.add_parser("probe", help="mint + 模型清单 + 双协议推理")
    probe.add_argument("--reuse", action="store_true", help="复用已存 MaaS key 不重新签发")
    probe.add_argument("--openai-model", default="", help="OpenAI chat 探测使用的模型名")
    probe.add_argument("--anthropic-model", default="", help="Anthropic messages 探测使用的模型名")
    allp = sub.add_parser("all", help="login 后直接 probe")
    allp.add_argument("--reuse", action="store_true")
    allp.add_argument("--openai-model", default="")
    allp.add_argument("--anthropic-model", default="")
    args = parser.parse_args()
    handlers = {"login": cmd_login, "mint": cmd_mint, "probe": cmd_probe, "all": cmd_all}
    return handlers[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
