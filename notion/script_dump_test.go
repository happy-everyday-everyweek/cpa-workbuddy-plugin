package main

// 本文件把登录页的真实渲染产物写到磁盘，供浏览器与语法验证使用。
//
// 之所以用 Go 产物而不是解析源码：桥接页与回调页的 HTML/JS 由大量
// 表达式拼接而成，源码正则提取会漏掉拼接片段，验证的就不是真正上线的
// 那一份。仅在设置 NOTION_DUMP_DIR 时写文件，常规 go test 不受影响。
//
// 运行：NOTION_DUMP_DIR=/tmp/x go test -run TestDumpLoginPages ./notion
// 最近修改时间：2026-10-19；改动原因：新增，登录页真实产物外带。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDumpLoginPages 把桥接页与回调页写成 HTML 文件。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，登录页产物外带。
func TestDumpLoginPages(t *testing.T) {
	dir := os.Getenv("NOTION_DUMP_DIR")
	if dir == "" {
		t.Skip("未设置 NOTION_DUMP_DIR，跳过页面导出")
	}

	resetLoginSessions()
	resp := startLoginForTest(t)

	pages := map[string][]byte{
		"bridge.html":   handleLoginBridge(resp.State).Body,
		"callback.html": handleLoginCallback(resp.State).Body,
	}
	for name, body := range pages {
		path := filepath.Join(dir, name)
		if errWrite := os.WriteFile(path, body, 0o644); errWrite != nil {
			t.Fatalf("写出 %s 失败：%v", name, errWrite)
		}
		t.Logf("已写出 %d 字节到 %s", len(body), path)
	}
}
