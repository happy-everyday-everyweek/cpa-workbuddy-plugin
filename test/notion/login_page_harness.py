"""为 notion 登录页做本地端到端验证的假服务端。

只做三件事：把 Go 测试导出的真实页面按 resource 路径吐出去、
在 /login/import 上记录收到的 Cookie 头并回成功。它不校验真实 Notion，
目的是验证浏览器里这段脚本自己能否跑通（DOM 取值、拼 Cookie、发请求）。

用法：python test/notion/login_page_harness.py .tmp_pages 8765
"""

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse

PAGE_DIR = sys.argv[1] if len(sys.argv) > 1 else ".tmp_pages"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 8765
CAPTURE = os.path.join(PAGE_DIR, "captured.json")


def read_page(name):
    with open(os.path.join(PAGE_DIR, name), encoding="utf-8") as f:
        return f.read().encode("utf-8")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urlparse(self.path)
        path = parsed.path

        if path.endswith("/login/import"):
            header = self.headers.get("X-Notion-Cookie", "")
            pairs = [p.strip().split("=")[0] for p in header.split(";") if "=" in p]
            with open(CAPTURE, "w", encoding="utf-8") as f:
                json.dump({"pair_keys": pairs, "len": len(header)}, f)
            body = json.dumps(
                {"ok": True, "labels": ["验证账号 · 订阅"], "auth_ids": ["notion-x"]}
            ).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        if path.endswith("/login/callback"):
            body = read_page("callback.html")
        elif path.endswith("/login"):
            body = read_page("bridge.html")
        else:
            self.send_response(404)
            self.end_headers()
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.command if hasattr(self, "command") else "", fmt % args))


if __name__ == "__main__":
    if os.path.exists(CAPTURE):
        os.remove(CAPTURE)
    HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
