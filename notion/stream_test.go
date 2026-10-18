package main

// 本文件用真实抓取的 NDJSON 样本回归 patch 状态机。
//
// 五份样本覆盖三个关键分支：
//   - plain / gpt：无 thinking，正文经 /value/- 追加到索引 1；
//   - thinking：思考与正文同段混排，正文必须与思考分离；
//   - search：流内有多个 patch-sync，快照会重发已存在段落。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseSampleStreams 校验每份样本都能还原出非空正文。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，真实样本回归。
func TestParseSampleStreams(t *testing.T) {
	cases := []struct {
		file        string
		wantContain string
		// wantThinking 表示该样本必须同时抽出思考内容。
		wantThinking bool
	}{
		{file: "plain.ndjson", wantContain: "OK"},
		{file: "gpt.ndjson", wantContain: "What would you like help with?"},
		{file: "glm.ndjson", wantContain: "What would you like me to help with?"},
		{file: "thinking.ndjson", wantContain: "OK", wantThinking: true},
		{file: "search.ndjson", wantContain: "I am Notion AI", wantThinking: true},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw := readSample(t, tc.file)
			parser := newStreamParser()
			events, threadID, err := parser.Parse(strings.NewReader(raw))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if threadID == "" {
				t.Fatal("未提取到线程 id")
			}

			text := parser.completionText()
			if !strings.Contains(text, tc.wantContain) {
				t.Fatalf("正文未包含 %q，实际: %q", tc.wantContain, truncate(text, 200))
			}

			reasoning := parser.reasoningText()
			if tc.wantThinking && reasoning == "" {
				t.Fatal("未提取到思考内容")
			}
			if !tc.wantThinking && reasoning != "" {
				t.Fatalf("该样本不应有思考内容，实际 %d 字符", len(reasoning))
			}

			// 正文绝不能混入思考内容。
			if reasoning != "" && strings.Contains(text, reasoning) {
				t.Fatal("正文混入了思考内容")
			}

			if len(events) == 0 {
				t.Fatal("未产出任何增量事件")
			}
			input, output := parser.usage()
			if output <= 0 {
				t.Fatalf("output tokens 异常: %d", output)
			}
			_ = input
		})
	}
}

// TestParseSampleThreadIDMatchesRecordMap 校验线程 id 来自 record-map。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，防止误用 traceId。
func TestParseSampleThreadIDMatchesRecordMap(t *testing.T) {
	raw := readSample(t, "plain.ndjson")
	parser := newStreamParser()
	if _, _, err := parser.Parse(strings.NewReader(raw)); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(raw, parser.threadID) {
		t.Fatalf("线程 id %s 不在原始流中", parser.threadID)
	}
	if len(parser.threadID) != 36 {
		t.Fatalf("线程 id 长度异常: %s", parser.threadID)
	}
}

// TestParseEmptyStreamReturnsNoEvents 校验空流不产生正文。
//
// 调用方据此判定为软失败并换号，而不是把空字符串当成功回复。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，空流回归。
func TestParseEmptyStreamReturnsNoEvents(t *testing.T) {
	parser := newStreamParser()
	events, threadID, err := parser.Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("空流不应报错: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("空流产出了 %d 个事件", len(events))
	}
	if threadID != "" {
		t.Fatalf("空流不应有线程 id: %s", threadID)
	}
	if got := parser.completionText(); got != "" {
		t.Fatalf("空流正文应为空，实际 %q", got)
	}
}

// TestParseThinkingOnlyStreamHasNoText 校验只有思考时正文为空。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，思考不应冒充正文。
func TestParseThinkingOnlyStreamHasNoText(t *testing.T) {
	raw := `{"type":"patch-start","data":{"s":[{"id":"s1","type":"agent-instruction-state"}]}}
{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"s2","type":"agent-inference","value":[{"type":"thinking","content":"内部推理"}]}}]}
`
	parser := newStreamParser()
	if _, _, err := parser.Parse(strings.NewReader(raw)); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := parser.completionText(); got != "" {
		t.Fatalf("思考不应计入正文: %q", got)
	}
	if got := parser.reasoningText(); got != "内部推理" {
		t.Fatalf("思考内容不正确: %q", got)
	}
}

// TestParsePatchSyncKeepsPriorSections 校验 patch-sync 不会打乱已有段落。
//
// 这是 Gemini 场景的回归：流内多次 patch-sync 时，若用累积计数器
// 分配索引，后续增量会写到错误的段落上。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，快照重发回归。
func TestParsePatchSyncKeepsPriorSections(t *testing.T) {
	raw := `{"type":"patch-start","data":{"s":[{"id":"s0","type":"agent-instruction-state"}]}}
{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"s1","type":"agent-inference","value":[{"type":"thinking","content":"想一想"}]}}]}
{"type":"patch","v":[{"o":"a","p":"/s/1/value/-","v":{"type":"text","content":"你"}}]}
{"type":"patch-sync","data":{"s":[
  {"id":"s1","type":"agent-inference","value":[{"type":"thinking","content":"想一想"}]},
  {"id":"s-late","type":"title","value":"标题"}
]}}
{"type":"patch","v":[{"o":"x","p":"/s/1/value/1/content","v":"好"}]}
{"type":"patch","v":[{"o":"p","p":"/s/1/value/1/content","v":"你好呀"}]}
`
	parser := newStreamParser()
	if _, _, err := parser.Parse(strings.NewReader(raw)); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := parser.completionText(); got != "你好呀" {
		t.Fatalf("正文 = %q, 期望 你好呀", got)
	}
	if got := parser.reasoningText(); got != "想一想" {
		t.Fatalf("思考 = %q, 期望 想一想", got)
	}
}

// TestParseSkipsMalformedLines 校验非法行被跳过而不中断整条流。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，容错回归。
func TestParseSkipsMalformedLines(t *testing.T) {
	raw := `not-json
{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"s1","type":"agent-inference","value":[{"type":"text","content":"正文"}]}}]}
{"truncated":`
	parser := newStreamParser()
	if _, _, err := parser.Parse(strings.NewReader(raw)); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := parser.completionText(); got != "正文" {
		t.Fatalf("正文 = %q, 期望 正文", got)
	}
}

// TestParseStreamCallbackStopsOnError 校验回调错误会中止读取。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，客户端断开回归。
func TestParseStreamCallbackStopsOnError(t *testing.T) {
	raw := `{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"s1","type":"agent-inference","value":[{"type":"text","content":"一"}]}}]}
{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"s2","type":"agent-inference","value":[{"type":"text","content":"二"}]}}]}
`
	parser := newStreamParser()
	count := 0
	stopErr := errStopTest{}
	_, err := parser.ParseStream(strings.NewReader(raw), func(streamEvent) error {
		count++
		return stopErr
	})
	if err == nil {
		t.Fatal("回调错误应向上传递")
	}
	if count != 1 {
		t.Fatalf("回调被调用 %d 次, 期望 1 次", count)
	}
}

type errStopTest struct{}

func (errStopTest) Error() string { return "stop" }

// readSample 读取 testdata 下的样本。
// [参数] t: 测试上下文；name: 文件名。
// [返回] 文件内容。
// 最近修改时间：2026-10-18；改动原因：新增，样本读取辅助。
func readSample(t *testing.T, name string) string {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		t.Fatalf("读取样本 %s 失败: %v", name, errRead)
	}
	return string(raw)
}

// truncate 截断字符串用于错误信息。
// [参数] s: 原文；n: 上限。
// [返回] 截断结果。
// 最近修改时间：2026-10-18；改动原因：新增，测试辅助。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
