#!/usr/bin/env python3
"""drive_growth_e2e.py — 用 ctypes 驱动真实 c-shared 插件，端到端验证成长中心编排。

目的：在不上生产的前提下，让真实 Go 代码路径（runGrowthForAccount →
buddy 领养 → 旅行 → 任务 → 补登 → 兑换 → 抽奖 → 盲盒）跑一次真实上游账号，
确认整条链路能返回结构化结果，而不是只靠单测桩验证。

前置：WSL 侧已用 CGO_ENABLED=1 -buildmode=c-shared 编译出 .so。

用法：
    python scripts/drive_growth_e2e.py --so <path.so> --auth-file <auth.json>

凭据纪律：auth 文件只传给插件内部，本脚本不打印 token / uid 原值。
"""

from __future__ import annotations

import argparse
import base64
import ctypes
import json
import os
import sys
import urllib.error
import urllib.request


class CliproxyBuffer(ctypes.Structure):
    """插件 ABI 的响应缓冲区结构。"""

    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


HOST_CALL_FN = ctypes.CFUNCTYPE(
    ctypes.c_int32,
    ctypes.c_void_p,
    ctypes.POINTER(ctypes.c_int8),
    ctypes.POINTER(ctypes.c_uint8),
    ctypes.c_size_t,
    ctypes.POINTER(CliproxyBuffer),
)
HOST_FREE_FN = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)


class HostAPI(ctypes.Structure):
    """宿主回调表：本脚本只需 call/free，其余字段留空。"""

    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("host_ctx", ctypes.c_void_p),
        ("call", HOST_CALL_FN),
        ("free_buffer", HOST_FREE_FN),
    ]


PLUGIN_CALL_FN = ctypes.CFUNCTYPE(ctypes.c_int32, ctypes.c_char_p, ctypes.POINTER(ctypes.c_uint8), ctypes.c_size_t, ctypes.POINTER(CliproxyBuffer))
PLUGIN_FREE_FN = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
PLUGIN_SHUTDOWN_FN = ctypes.CFUNCTYPE(None)


class PluginAPI(ctypes.Structure):
    """插件回调表：init 由插件填写，宿主用它来发起调用。"""

    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("call", PLUGIN_CALL_FN),
        ("free_buffer", PLUGIN_FREE_FN),
        ("shutdown", PLUGIN_SHUTDOWN_FN),
    ]


LIBC = ctypes.CDLL("libc.so.6")
LIBC.malloc.restype = ctypes.c_void_p
LIBC.malloc.argtypes = [ctypes.c_size_t]
LIBC.memcpy.restype = ctypes.c_void_p
LIBC.memcpy.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_size_t]


_KEEP: list = []


def send_response(out: ctypes.POINTER(CliproxyBuffer), payload: dict) -> int:
    """把 JSON 响应写进插件期望的缓冲区。

    插件侧读完会用 C free() 释放该指针，因此必须用 libc malloc 分配，
    不能用 Python 的 buffer（否则插件 free 时会被堆管理器判为非法指针而崩溃）。
    """
    raw = json.dumps(payload).encode("utf-8")
    mem = LIBC.malloc(len(raw))
    if not mem:
        return 1
    LIBC.memcpy(mem, raw, len(raw))
    out.contents.ptr = mem
    out.contents.len = len(raw)
    return 0


def make_host_call(auth_doc: dict):
    """构造宿主回调：实现插件会调用到的 auth.list / auth.get 等方法。

    所有响应必须是 {ok,result} 信封——插件侧 hostAuth* 会校验 env.OK，
    返回裸对象会被判为 "bad envelope"。
    """

    def ok(result) -> dict:
        return {"ok": True, "result": result}

    def host_call(_ctx, method_ptr, req_ptr, req_len, out):
        method = ctypes.string_at(method_ptr).decode("utf-8") if method_ptr else ""
        raw = ctypes.string_at(req_ptr, req_len) if req_ptr and req_len else b""
        try:
            req = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            req = {}

        print(f"[host] {method}", flush=True)
        if method in ("host.auth.list", "auth.list"):
            return send_response(
                out,
                ok(
                    {
                        "files": [
                            {
                                "id": "wb-e2e",
                                "auth_index": "idx-e2e",
                                "name": "workbuddy-e2e.json",
                                "path": "/tmp/workbuddy-e2e.json",
                                "type": "workbuddy-provider",
                                "provider": "workbuddy-provider",
                                "email": "",
                            }
                        ]
                    }
                ),
            )
        if method in ("host.auth.get", "auth.get"):
            return send_response(
                out,
                ok(
                    {
                        "auth_index": "idx-e2e",
                        "name": "workbuddy-e2e.json",
                        "path": "/tmp/workbuddy-e2e.json",
                        "json": auth_doc,
                    }
                ),
            )
        if method == "host.http.do":
            return send_response(out, ok(forward_http(req)))
        # 其余方法（usage / model 等）返回空成功信封，避免插件初始化中断
        return send_response(out, ok({}))

    return HOST_CALL_FN(host_call)


def forward_http(req: dict) -> dict:
    """把插件的 host.http.do 请求真实转发出去，模拟宿主网络桥。

    请求体在 wire 上是 base64（Go []byte），响应同样回 base64。
    """
    inner = req.get("request") or {}
    url = inner.get("url") or ""
    method = (inner.get("method") or "GET").upper()
    headers = inner.get("headers") or {}
    body_b64 = inner.get("body") or ""
    body = base64.b64decode(body_b64) if body_b64 else None

    request = urllib.request.Request(url, data=body, method=method, headers={k: v[0] if isinstance(v, list) and v else str(v) for k, v in headers.items()})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            status, raw = response.status, response.read()
            resp_headers = dict(response.headers)
    except urllib.error.HTTPError as exc:
        status, raw, resp_headers = exc.code, exc.read(), dict(exc.headers)
    except Exception as exc:
        print(f"[host.http.do] transport error {method} {url}: {exc}", flush=True)
        return {"status": 0, "headers": {}, "body": base64.b64encode(str(exc).encode()).decode()}

    print(f"[host.http.do] {method} {status} {url}", flush=True)
    return {
        "status": status,
        "headers": {k: [v] for k, v in resp_headers.items()},
        "body": base64.b64encode(raw).decode("ascii"),
    }


def call_plugin(lib, method: str, request: dict) -> dict:
    """调用插件导出的 cliproxyPluginCall 并解包返回 JSON。"""
    raw = json.dumps(request).encode("utf-8")
    buf = ctypes.create_string_buffer(raw, len(raw))
    out = CliproxyBuffer()
    rc = lib.cliproxyPluginCall(
        method.encode("utf-8"),
        ctypes.cast(buf, ctypes.POINTER(ctypes.c_uint8)),
        len(raw),
        ctypes.byref(out),
    )
    body = ctypes.string_at(out.ptr, out.len) if out.ptr and out.len else b"{}"
    lib.cliproxyPluginFree(out.ptr, out.len)
    if rc != 0:
        return {"_error": f"rc={rc}", "_body": body.decode("utf-8", errors="replace")}
    return json.loads(body)


def main() -> int:
    """命令行入口：加载插件 → 注册 → 触发 POST /growth → 打印结果。"""
    parser = argparse.ArgumentParser(description="驱动 workbuddy 插件跑一轮成长中心")
    parser.add_argument("--so", required=True, help="c-shared 插件路径")
    parser.add_argument("--auth-file", required=True, help="单账号 auth 文档 JSON")
    parser.add_argument("--config", default="checkin_auto: false\ngrowth_auto: true\n", help="config_yaml 内容")
    args = parser.parse_args()

    auth_doc = json.load(open(args.auth_file, encoding="utf-8"))
    lib = ctypes.CDLL(args.so)

    host_api = HostAPI()
    host_api.abi_version = 1
    host_api.call = make_host_call(auth_doc)
    LIBC.free.argtypes = [ctypes.c_void_p]
    host_api.free_buffer = HOST_FREE_FN(lambda ptr, _l: LIBC.free(ptr))

    plugin_api = PluginAPI()
    lib.cliproxy_plugin_init.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
    lib.cliproxy_plugin_init.restype = ctypes.c_int32
    rc = lib.cliproxy_plugin_init(ctypes.byref(host_api), ctypes.byref(plugin_api))
    print(f"[e2e] init rc={rc} plugin_abi={plugin_api.abi_version}")
    if rc != 0:
        return 1

    # 后续一律经宿主回调表调用，与生产宿主路径一致
    lib.cliproxyPluginCall = plugin_api.call
    lib.cliproxyPluginFree = plugin_api.free_buffer

    # 注册插件，让 configure() 解析 config_yaml 并起调度器
    encoded_cfg = base64.b64encode(args.config.encode("utf-8")).decode("ascii")
    reg = call_plugin(lib, "plugin.register", {"config_yaml": encoded_cfg})
    print(f"[e2e] register keys={sorted(reg.keys())[:6]}")

    # 触发管理路由：POST /plugins/workbuddy-provider/growth
    resp = call_plugin(
        lib,
        "management.handle",
        {
            "method": "POST",
            "path": "/v0/management/plugins/workbuddy-provider/growth",
            # management 请求的 Body 在 Go 侧是 []byte，JSON 传输同样走 base64
            "body": base64.b64encode(json.dumps({"auth_index": "idx-e2e"}).encode("utf-8")).decode("ascii"),
            "headers": {},
        },
    )
    print("[e2e] raw response:")
    print(json.dumps(resp, ensure_ascii=False, indent=2)[:4000])
    # 保持回调表引用到进程结束，防止后台 goroutine 访问已回收的 ctypes 闭包
    _KEEP.append((host_api, plugin_api, lib))
    sys.stdout.flush()
    os._exit(0)


if __name__ == "__main__":
    sys.exit(main())
