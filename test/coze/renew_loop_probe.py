#!/usr/bin/env python3
"""coze 定时续期探针（真实 .so 走 C ABI）。

复刻 CPA 宿主的最小行为：构造 cliproxy_host_api 函数指针表 →
cliproxy_plugin_init → 等待后台循环自行触发 → 检查它真的调用了
host.auth.list / host.log。

验证目标（0.2.2 的核心修复）：
  1. init 之后插件确实自己起了续期循环，不依赖任何 HTTP 请求触发；
  2. 首轮续期真的执行（host.auth.list 被调用、host.log 出现轮次日志）；
  3. 续期动作可观测：日志字段含 accounts / renewed，失败时含原因；
  4. 收到 Shutdown 后循环退出，不再产生新的 host 调用。

不在本探针覆盖范围（已由 Go 单测覆盖）：
  - 上游双向换发与落盘：插件用独立 http.Client 直连 www.coze.cn，
    不经过 host.http.do，探针无法在不做 TLS 中间人的前提下拦截。
    对应单测：TestRenewSession_DoubleExchange、TestApplyRenewal_WritesNewCookie、
    TestApplyRenewal_SkipsWhenExternallyReplaced。
  - 4 小时真实排期：探针只验证首轮自启动，排期由
    TestRenewLoopFiresOnInterval 覆盖。

用法：
    python test/coze/renew_loop_probe.py
环境变量：
    COZE_SO  真实 cgo 产物路径（默认 /tmp/coze_abi/coze-provider.so）
"""
import ctypes
import json
import os
import sys
import tempfile
import time

SO = os.environ.get("COZE_SO", "/tmp/coze_abi/coze-provider.so")


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


HOST_CALL = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_void_p, ctypes.c_char_p,
                             ctypes.c_void_p, ctypes.c_size_t,
                             ctypes.POINTER(Buffer))
# 与插件 C 头 cliproxy_host_free_fn(void*, size_t) 完全一致：
# 宿主 free_buffer 不带 ctx，写成三参数会破坏回调调用约定并直接崩溃。
HOST_FREE = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)


class HostAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("host_ctx", ctypes.c_void_p),
                ("call", HOST_CALL), ("free_buffer", HOST_FREE)]


PLUGIN_CALL = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.POINTER(ctypes.c_uint8),
                               ctypes.c_size_t, ctypes.POINTER(Buffer))
PLUGIN_FREE = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
PLUGIN_SHUTDOWN = ctypes.CFUNCTYPE(None)


class PluginAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("call", PLUGIN_CALL),
                ("free_buffer", PLUGIN_FREE), ("shutdown", PLUGIN_SHUTDOWN)]


libc = ctypes.CDLL("libc.so.6")
libc.malloc.restype = ctypes.c_void_p
libc.malloc.argtypes = [ctypes.c_size_t]
libc.free.restype = None
libc.free.argtypes = [ctypes.c_void_p]

# ---------------------------------------------------------------------------
# 账号桩：一个启用账号，走真实物理文件（续期结果要能落盘验证）
# ---------------------------------------------------------------------------
AUTH_DIR = tempfile.mkdtemp(prefix="coze-probe-")
AUTH_FILE = os.path.join(AUTH_DIR, "coze-probe.json")
AUTH_ID = "coze-probe"

AUTH_DOC = {
    "type": "coze-provider",
    "provider": "coze-provider",
    "auth_id": AUTH_ID,
    "cookie_header": "sessionid=old-session",
    "cookies": [
        {"name": "sessionid", "value": "old-session", "domain": "www.coze.cn", "path": "/"}
    ],
    "sec_user_id": "sec-probe",
    "screen_name": "探针账号",
    "label": "探针账号",
    "disabled": False,
    "login_at": "2026-10-01T00:00:00+08:00",
    "last_renew_at": "2026-10-01T00:00:00+08:00",
    "created_at": "2026-10-01T00:00:00+08:00",
    "updated_at": "2026-10-01T00:00:00+08:00",
}
with open(AUTH_FILE, "w", encoding="utf-8") as f:
    json.dump(AUTH_DOC, f, ensure_ascii=False, indent=2)

host_calls = []
http_calls = []
config_yaml = "renew_interval_seconds: 3600\ntimeout_seconds: 5\n"


def host_response(method, body):
    """按方法名返回宿主桩响应。"""
    req = json.loads(body) if body else {}

    if method == "host.auth.list":
        return {"ok": True, "result": {"files": [{
            "id": AUTH_ID + ".json",
            "auth_index": "idx-probe",
            "name": AUTH_ID + ".json",
            "type": "coze-provider",
            "provider": "coze-provider",
            "label": "探针账号",
            "disabled": False,
        }]}}

    if method == "host.auth.get":
        return {"ok": True, "result": {
            "auth_index": req.get("auth_index", ""),
            "name": AUTH_ID + ".json",
            "path": AUTH_FILE,
            "json": AUTH_DOC,
        }}

    if method == "host.auth.save":
        return {"ok": True, "result": {"name": req.get("name", "")}}

    if method == "host.http.do":
        http_calls.append(req)
        headers = req.get("headers") or {}
        url = req.get("url", "")
        if "token/beat" not in url:
            return {"ok": True, "result": {"status_code": 404, "headers": {}, "body": "{}"}}
        has_cookie = any(k.lower() == "cookie" for k in headers)
        if has_cookie:
            # 第一步：带 Cookie 换新 token。
            return {"ok": True, "result": {
                "status_code": 200,
                "headers": {"x-tt-token": "probe-token"},
                "body": json.dumps({"message": "success", "data": {"error_code": 0}}),
            }}
        # 第二步：带 token 换新 Cookie。
        return {"ok": True, "result": {
            "status_code": 200,
            "headers": {"set-cookie": [
                "sessionid=new-session; Path=/; Domain=.coze.cn",
                "sid_tt=new-sid; Path=/; Domain=.coze.cn",
            ]},
            "body": json.dumps({"message": "success", "data": {"error_code": 0}}),
        }}

    return {"ok": True, "result": {}}


@HOST_CALL
def host_call(ctx, method, req, req_len, out):
    m = method.decode()
    body = ctypes.string_at(req, req_len).decode() if req_len else ""
    host_calls.append((m, body[:600]))
    payload = host_response(m, body)
    raw = json.dumps(payload, ensure_ascii=False).encode()
    p = libc.malloc(len(raw))
    ctypes.memmove(p, raw, len(raw))
    out.contents.ptr = p
    out.contents.len = len(raw)
    return 0


@HOST_FREE
def host_free(ptr, ln):
    if ptr:
        libc.free(ctypes.c_void_p(ptr))


lib = ctypes.CDLL(SO)
lib.cliproxy_plugin_init.restype = ctypes.c_int
lib.cliproxy_plugin_init.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
lib.cliproxyPluginCall.restype = ctypes.c_int
lib.cliproxyPluginCall.argtypes = [ctypes.c_char_p, ctypes.c_void_p,
                                   ctypes.c_size_t, ctypes.POINTER(Buffer)]
lib.cliproxyPluginFree.restype = None
lib.cliproxyPluginFree.argtypes = [ctypes.c_void_p, ctypes.c_size_t]
lib.cliproxyPluginShutdown.restype = None
lib.cliproxyPluginShutdown.argtypes = []


def call_plugin(method, payload=None):
    raw = json.dumps(payload).encode() if payload is not None else b""
    buf = ctypes.create_string_buffer(raw, len(raw)) if raw else None
    out = Buffer()
    rc = lib.cliproxyPluginCall(method.encode(), buf, len(raw), ctypes.byref(out))
    data = ctypes.string_at(out.ptr, out.len).decode() if out.ptr and out.len else ""
    if out.ptr and out.len:
        lib.cliproxyPluginFree(ctypes.c_void_p(out.ptr), out.len)
    return rc, data


results = []


def check(label, cond, detail=""):
    results.append((label, bool(cond), detail))
    print(f"   {'PASS' if cond else 'FAIL'}  {label}" + (f"  [{detail}]" if detail and not cond else ""))


def methods_seen(prefix):
    return [m for m, _ in host_calls if m.startswith(prefix)]


def logs_with(text):
    return [body for m, body in host_calls if m == "host.log" and text in body]


def main():
    if not os.path.exists(SO):
        print(f"找不到 .so：{SO}", file=sys.stderr)
        return 2

    print(f"[coze] SO = {SO}")
    print(f"[coze] auth dir = {AUTH_DIR}")

    host = HostAPI()
    host.abi_version = 1
    host.host_ctx = None
    host.call = host_call
    host.free_buffer = host_free
    plugin = PluginAPI()

    rc = lib.cliproxy_plugin_init(ctypes.byref(host), ctypes.byref(plugin))
    check("cliproxy_plugin_init 返回 0", rc == 0, f"rc={rc}")

    # 注册时下发配置：把间隔压到 1 小时（探针只等首轮，不验证 4h 排期）。
    rc, data = call_plugin("plugin.register", {"config_yaml": list(config_yaml.encode())})
    env = json.loads(data) if data else {}
    check("plugin.register 成功", env.get("ok") is True, data[:200])

    # 关键点：没有任何 HTTP 触发，等后台循环自己跑首轮。
    deadline = time.time() + 20
    while time.time() < deadline:
        if logs_with("定时续期一轮结束"):
            break
        time.sleep(0.2)

    check("init 后自启续期循环（出现轮次结束日志）", bool(logs_with("定时续期一轮结束")))
    check("续期调用了 host.auth.list", len(methods_seen("host.auth.list")) > 0,
          f"calls={len(methods_seen('host.auth.list'))}")
    check("续期向宿主日志上报账号数", any('"accounts"' in b for b in logs_with("定时续期一轮结束")))
    # 首轮必然尝试续期：真实上游不可达时以 warn 形式留痕，这是可观测性的关键。
    check("续期尝试可观测（完成日志或失败日志）",
          bool(logs_with("定时续期一轮结束")) and (
              bool(logs_with("定时续期完成")) or bool(logs_with("定时续期失败"))),
          f"logs={[b[:120] for b in logs_with('定时续期')][:3]}")

    # Shutdown 后循环应停止：记录当前调用数，等 2s 确认不再增长。
    before = len(host_calls)
    lib.cliproxyPluginShutdown()
    time.sleep(2.0)
    after_shutdown = len(host_calls)
    check("Shutdown 后循环停止（无新 host 调用）", after_shutdown == before,
          f"before={before} after={after_shutdown}")

    passed = sum(1 for _, ok, _ in results if ok)
    total = len(results)
    print(f"\n[coze] {passed}/{total} PASS")
    for label, ok, detail in results:
        if not ok:
            print(f"  FAILED: {label} {detail}")
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
