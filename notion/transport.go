package main

// 本文件实现 Notion 专用的 HTTP 传输层。
//
// 为什么不能走宿主的 host.http.do：宿主只代理 method/URL/headers/body，
// 无法定制 TLS ClientHello。Notion 会比对 UA、sec-ch-ua 与 TLS 指纹三者
// 的 Chrome 主版本，标准库握手的特征与之不符，因此这里自建 http.Client，
// 用 uTLS 模拟 Chrome 的 ClientHello，并强制 HTTP/2 协商。
//
// 最近修改时间：2026-10-18；改动原因：新增，仓库首个 uTLS 传输层。

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

const (
	// notionDialTimeout 是 TCP 建连超时。
	notionDialTimeout = 20 * time.Second

	// notionHandshakeTimeout 是 TLS 握手超时。
	notionHandshakeTimeout = 20 * time.Second
)

// newNotionTransport 构造 uTLS + HTTP/2 的 RoundTripper。
//
// 1. 每个连接独立拨号，避免复用标准库默认 transport 的指纹缓存；
// 2. 用 utls.HelloChrome_Auto 生成与服务端匹配的 ClientHello；
// 3. NextProtos 显式声明 h2，交给 http2.Transport 复用而非走 HTTP/1.1。
// [参数] 无。
// [返回] 可复用的 RoundTripper。
// 最近修改时间：2026-10-18；改动原因：新增，uTLS 握手入口。
func newNotionTransport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: notionDialTimeout, KeepAlive: 30 * time.Second}

	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			raw, errDial := dialer.DialContext(ctx, network, addr)
			if errDial != nil {
				return nil, errDial
			}

			host, _, errSplit := net.SplitHostPort(addr)
			if errSplit != nil {
				_ = raw.Close()
				return nil, errSplit
			}

			cfg := &utls.Config{
				ServerName:         host,
				MinVersion:         tls.VersionTLS12,
				NextProtos:         []string{"h2", "http/1.1"},
				InsecureSkipVerify: false,
			}

			// 握手必须有独立超时：拨号超时只覆盖 TCP 建连，
			// 服务端接受连接却不应答时不会自动结束。
			handshakeCtx, cancel := context.WithTimeout(ctx, notionHandshakeTimeout)
			defer cancel()

			conn := utls.UClient(raw, cfg, utls.HelloChrome_Auto)
			if errHandshake := conn.HandshakeContext(handshakeCtx); errHandshake != nil {
				_ = raw.Close()
				return nil, errHandshake
			}
			return conn, nil
		},
		DisableCompression: true,
	}
}

// newNotionHTTPClient 构造带 uTLS 指纹的 HTTP 客户端。
// [参数] timeout: 整轮请求超时，<=0 时取 120 秒。
// [返回] HTTP 客户端。
// 最近修改时间：2026-10-18；改动原因：新增，对外唯一的 client 构造入口。
func newNotionHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &http.Client{
		Transport: newNotionTransport(),
		Timeout:   timeout,
	}
}
