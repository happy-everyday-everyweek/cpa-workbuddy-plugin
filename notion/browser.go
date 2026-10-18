package main

// 本文件集中维护浏览器指纹常量。
//
// Notion 的网页端会用 UA、sec-ch-ua 与 TLS ClientHello 三者交叉比对，
// 任意一项的 Chrome 主版本落后都会被视为非浏览器流量。三个版本号必须
// 同步升级：browserChromeMajor 是唯一真源，其余常量由它拼装。
//
// 最近修改时间：2026-10-18；改动原因：新增，指纹常量集中管理。

import "fmt"

const (
	// browserChromeMajor 是当前模拟的 Chrome 主版本，也是三处指纹的唯一真源。
	browserChromeMajor = "146"

	// browserPlatform 是 sec-ch-ua-platform 的取值，需与 UA 中的系统描述一致。
	// 之所以固定 Windows：本仓库的发布产物主要跑在 Linux 容器，
	// 用 Windows 文案可以避免与宿主机的 Linux 特征冲突（上游只做一致性校验）。
	browserPlatform = `"Windows"`

	// browserClientVersion 是 notion-client-version 请求头。
	// 该值随 Notion 前端发版滚动，做成配置项后由这里提供默认值。
	browserClientVersion = "23.13.20260313.1423"

	// notionAPIBase 是 Notion 应用层 API 前缀。
	notionAPIBase = "https://www.notion.so/api/v3"

	// notionOrigin 与 notionReferer 必须指向网页端 chat 页面。
	// 上游对 Referer 做同源校验，用裸域名会缺少 chat 上下文。
	notionOrigin  = "https://www.notion.so"
	notionReferer = "https://www.notion.so/chat"

	// notionAuditPlatform 固定为 web，表示操作来自网页端。
	notionAuditPlatform = "web"
)

// browserUserAgent 返回与 browserChromeMajor 对齐的桌面 Chrome UA。
// [参数] 无。
// [返回] User-Agent 字符串。
// 最近修改时间：2026-10-18；改动原因：新增，UA 由主版本号派生。
func browserUserAgent() string {
	return fmt.Sprintf(
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36",
		browserChromeMajor,
	)
}

// browserSecCHUA 返回与 UA 主版本一致的 sec-ch-ua。
// [参数] 无。
// [返回] sec-ch-ua 头取值。
// 最近修改时间：2026-10-18；改动原因：新增，sec-ch-ua 由主版本号派生。
func browserSecCHUA() string {
	return fmt.Sprintf(`"Chromium";v="%s", "Not?A_Brand";v="24", "Google Chrome";v="%s"`,
		browserChromeMajor, browserChromeMajor)
}
