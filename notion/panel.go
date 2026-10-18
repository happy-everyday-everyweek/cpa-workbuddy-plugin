package main

// 本文件把面板 HTML 编译进插件二进制。
//
// 用 embed 而不是运行时读文件：插件以 .so 形式分发，部署目录里没有
// panel.html，运行时读取会 404。

import _ "embed"

//go:embed panel.html
var panelHTML []byte
