package main

// 本文件实现 Notion NDJSON 流的 patch 状态机。
//
// 三个必须遵守的实测结论（2026-10-18 真实凭据验证）：
//
//  1. 段落索引不能用累积计数器。patch-start / patch-sync 携带的 data.s
//     是权威全量快照，且 patch-sync 会重发已存在的段落（Gemini 一次流
//     有 4 个 patch-sync）。用计数器会让后续 /s/N/... 全部错位。
//  2. agent-inference.value 是异构数组。元素可能是 thinking 或 text，
//     文本项不固定在索引 0：GPT / Gemini / GLM 的首元素是 thinking，
//     正文经 a /s/N/value/- 追加到索引 1。
//  3. agent-inference 段注册时可能没有 value，正文是后续追加的元素。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// maxStreamLineBytes 是单行 NDJSON 的上限。
	//
	// 搜索类回复会把整页内容塞进一个 patch，10MB 仍不足以覆盖时
	// 会被截断为解析错误，由调用方判定为空流。
	maxStreamLineBytes = 16 << 20

	// trustRuleDeniedMarker 是上游拒绝当前出口 IP 时的帧内标记。
	//
	// 该拒绝以 HTTP 200 下发，若不显式识别，整轮会被误判成空流，
	// 账号也不会进入冷却，下一轮仍会打到同一个被拒账号上。
	trustRuleDeniedMarker = "trust-rule-denied"
)

// errTrustRuleDenied 表示上游 trust rule 拒绝了当前账号+出口 IP。
var errTrustRuleDenied = errors.New("notion trust rule denied")

// valueItem 是 agent-inference.value 数组的一个元素。
type valueItem struct {
	// Type 是 text / thinking。
	Type string
	// Content 是该元素的文本。
	Content string
	// Emitted 是已下发给客户端的字节数。
	//
	// 快照（patch-start / patch-sync）会重发已存在段落的完整文本，
	// 记录已下发长度后只补发新增部分，避免客户端看到重复内容。
	Emitted int
}

// section 是流里的一个段落。
type section struct {
	// ID 是上游分配的段落 id。
	ID string
	// Type 是段落类型，如 agent-inference / title / agent-tool-result。
	Type string
	// Values 是段落的值数组。
	Values []*valueItem
	// InputTokens 与 OutputTokens 来自段落属性 patch。
	InputTokens  int
	OutputTokens int
	// Model 是上游回报的实际模型代号。
	Model string
}

// streamEvent 是解析出的一个语义事件。
type streamEvent struct {
	// Kind 是事件类型：delta / thinking / done / error。
	Kind string
	// Text 是该事件的文本增量。
	Text string
}

// streamParser 把 NDJSON 行流转换成事件序列。
type streamParser struct {
	// sections 按索引保存段落，索引与上游 /s/N 对齐。
	sections map[int]*section
	// byID 用于快照里按 id 找到已有段落。
	byID map[string]*section
	// order 是按注册顺序记录的段落索引。
	order []int
	// currentIDs 是当前快照的索引到 id 映射。
	currentIDs []string
	// threadID 是 record-map 回报的真实线程 id。
	threadID string
	// sawInference 标记是否出现过 agent-inference 段。
	sawInference bool
	// failure 记录流内显式错误。
	failure string
}

// newStreamParser 构造解析器。
// [参数] 无。
// [返回] 解析器实例。
// 最近修改时间：2026-10-18；改动原因：新增，流解析入口。
func newStreamParser() *streamParser {
	return &streamParser{
		sections: make(map[int]*section),
		byID:     make(map[string]*section),
	}
}

// Parse 读取整个流并返回事件列表。
//
// 解析是增量的：每读到一行就立即产出对应事件，调用方可以按需转发。
// [参数] r: NDJSON 流。
// [返回] 事件列表、线程 id 与错误。
// 最近修改时间：2026-10-18；改动原因：新增，同步收集路径。
func (p *streamParser) Parse(r io.Reader) ([]streamEvent, string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLineBytes)

	var events []streamEvent
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineEvents, errLine := p.consumeLine([]byte(line))
		if errLine != nil {
			// 单行解析失败不终止整条流：上游偶发夹带非 JSON 心跳行，
			// 跳过它比丢掉整轮回复更划算。
			continue
		}
		events = append(events, lineEvents...)
	}
	if errScan := scanner.Err(); errScan != nil {
		return events, p.threadID, errScan
	}
	if p.failure != "" {
		return events, p.threadID, fmt.Errorf("%s", p.failure)
	}
	return events, p.threadID, nil
}

// ParseStream 逐行解析并把增量交给回调。
//
// 回调返回错误时立即停止读取：客户端断开后继续拉上游只会浪费额度。
// [参数] r: NDJSON 流；onEvent: 事件回调。
// [返回] 线程 id 与错误。
// 最近修改时间：2026-10-18；改动原因：新增，流式增量路径。
func (p *streamParser) ParseStream(r io.Reader, onEvent func(streamEvent) error) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLineBytes)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineEvents, errLine := p.consumeLine([]byte(line))
		if errLine != nil {
			continue
		}
		for _, event := range lineEvents {
			if errCallback := onEvent(event); errCallback != nil {
				// 回调错误必须原样上抛：调用方据此区分「客户端断开」
				// 与「上游解析失败」，前者不该触发换号。
				return p.threadID, errCallback
			}
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return p.threadID, errScan
	}
	if p.failure != "" {
		return p.threadID, fmt.Errorf("%s", p.failure)
	}
	return p.threadID, nil
}

// consumeLine 处理单行 NDJSON 并产出增量事件。
// [参数] line: 单行字节。
// [返回] 该行产生的事件与解析错误。
// 最近修改时间：2026-10-18；改动原因：新增单行状态机；补 trust-rule-denied 帧识别。
func (p *streamParser) consumeLine(line []byte) ([]streamEvent, error) {
	// 拒绝帧不是合法 patch：先按标记识别，避免它被当成普通行跳过。
	// 该拒绝以 HTTP 200 下发，只有这里能拦住。
	if bytes.Contains(line, []byte(trustRuleDeniedMarker)) {
		p.failure = "notion trust rule denied：当前出口 IP 与账号登录 IP 不一致，请在账号上配置出口代理或重新登录"
		return nil, nil
	}

	var raw map[string]any
	if errUnmarshal := json.Unmarshal(line, &raw); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	kind, _ := raw["type"].(string)
	switch kind {
	case "patch-start", "patch-sync":
		return p.applySnapshot(raw), nil
	case "patch":
		return p.applyPatch(raw), nil
	case "record-map":
		p.applyRecordMap(raw)
		return nil, nil
	default:
		return nil, nil
	}
}

// applySnapshot 用全量快照重建段落索引。
//
// 快照是权威来源：它既覆盖 patch-start，也会在流中途以 patch-sync
// 反复出现。必须按 id 对齐到已有段落，而不是清空重建——清空会丢掉
// 快照里没带的历史文本片段。快照可能重发已下发的文本，这里只补发
// 超出已下发长度的部分。
// [参数] raw: 快照行。
// [返回] 该快照带来的新增文本事件。
// 最近修改时间：2026-10-18；改动原因：快照重发时避免正文重复下发。
func (p *streamParser) applySnapshot(raw map[string]any) []streamEvent {
	data, _ := raw["data"].(map[string]any)
	list, _ := data["s"].([]any)

	var events []streamEvent
	p.currentIDs = p.currentIDs[:0]
	for idx, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := obj["id"].(string)
		if id == "" {
			continue
		}

		sec := p.byID[id]
		if sec == nil {
			sec = &section{ID: id}
			p.byID[id] = sec
			p.order = append(p.order, idx)
		}
		if typ, _ := obj["type"].(string); typ != "" {
			sec.Type = typ
		}
		if values, okValue := obj["value"].([]any); okValue {
			events = append(events, syncValues(sec, toValueItems(values))...)
		}
		if sec.Type == "agent-inference" {
			p.sawInference = true
		}

		p.sections[idx] = sec
		p.currentIDs = append(p.currentIDs, id)
	}
	return events
}

// syncValues 用最新值覆盖段落内容，只返回尚未下发的文本。
//
// 快照里的文本可能比已下发的短（上游按块提交），此时保留更长的旧值，
// 避免正文回退变短。
// [参数] sec: 目标段落；incoming: 快照值。
// [返回] 新增文本事件。
// 最近修改时间：2026-10-18；改动原因：新增，快照与增量合并。
func syncValues(sec *section, incoming []*valueItem) []streamEvent {
	var events []streamEvent
	for i, item := range incoming {
		if i < len(sec.Values) {
			existing := sec.Values[i]
			existing.Type = firstNonEmpty(item.Type, existing.Type)
			if len(item.Content) >= existing.Emitted {
				// 快照文本是全新版本：补发超出已下发长度的部分。
				delta := item.Content[existing.Emitted:]
				existing.Content = item.Content
				if sec.Type == "agent-inference" && delta != "" {
					events = append(events, eventFor(existing.Type, delta))
				}
				existing.Emitted = len(item.Content)
			}
			continue
		}
		// 新元素：整段都是新增内容。
		sec.Values = append(sec.Values, item)
		if sec.Type == "agent-inference" && item.Content != "" {
			events = append(events, eventFor(item.Type, item.Content))
			item.Emitted = len(item.Content)
		}
	}
	return events
}

// eventFor 按元素类型构造事件。
// [参数] itemType: 元素类型；text: 文本。
// [返回] 事件。
// 最近修改时间：2026-10-18；改动原因：新增，事件构造统一入口。
func eventFor(itemType, text string) streamEvent {
	if itemType == "thinking" {
		return streamEvent{Kind: "thinking", Text: text}
	}
	return streamEvent{Kind: "delta", Text: text}
}

// applyPatch 应用一行增量 patch。
// [参数] raw: patch 行。
// [返回] 产生的增量事件。
// 最近修改时间：2026-10-18；改动原因：新增，a/x/p 三类操作。
func (p *streamParser) applyPatch(raw map[string]any) []streamEvent {
	ops, _ := raw["v"].([]any)
	var events []streamEvent

	for _, item := range ops {
		op, ok := item.(map[string]any)
		if !ok {
			continue
		}
		action, _ := op["o"].(string)
		path, _ := op["p"].(string)
		value := op["v"]

		// 1. 注册新段落：/s/- 后带整段对象。
		if action == "a" && path == "/s/-" {
			obj, _ := value.(map[string]any)
			events = append(events, p.appendSection(obj)...)
			continue
		}

		idx, field, okIndex := parseSectionPath(path)
		if !okIndex {
			continue
		}
		sec := p.sections[idx]
		if sec == nil {
			continue
		}

		switch {
		// 2. 追加 value 元素：/s/N/value/-
		case action == "a" && field == "value/-":
			obj, _ := value.(map[string]any)
			item := toValueItem(obj)
			sec.Values = append(sec.Values, item)
			if sec.Type == "agent-inference" {
				p.sawInference = true
				// 追加元素时也可能直接带正文（GPT / Gemini 场景首段文本
				// 就是这样到达的），必须作为增量下发。
				if item.Content != "" {
					events = append(events, eventFor(item.Type, item.Content))
					item.Emitted = len(item.Content)
				}
			}

		// 3. 增量或替换 value 文本：/s/N/value/M/content
		case (action == "x" || action == "p") && strings.HasPrefix(field, "value/"):
			event, okEvent := p.applyValueContent(sec, action, field, value)
			if okEvent {
				events = append(events, event)
			}

		// 4. 段落属性：outputTokens / inputTokens / model
		case action == "a" && field == "outputTokens":
			sec.OutputTokens = toInt(value)
		case action == "a" && field == "inputTokens":
			sec.InputTokens = toInt(value)
		case action == "a" && field == "model":
			if s, okStr := value.(string); okStr {
				sec.Model = s
			}
		}
	}
	return events
}

// appendSection 注册新段落。
//
// 段落注册帧可能自带正文（实测 Claude 一次返回完整 "OK" 而不发增量），
// 因此这里必须把初始 value 里的文本也作为事件产出，否则流式路径会
// 丢掉整段回复。
// [参数] obj: 段落对象。
// [返回] 初始正文与思考事件。
// 最近修改时间：2026-10-18；改动原因：修复注册帧正文被吞导致流式空回复。
func (p *streamParser) appendSection(obj map[string]any) []streamEvent {
	if obj == nil {
		return nil
	}
	id, _ := obj["id"].(string)
	if id == "" {
		return nil
	}

	sec := p.byID[id]
	if sec == nil {
		sec = &section{ID: id}
		p.byID[id] = sec
	}
	typ, _ := obj["type"].(string)
	if typ != "" {
		sec.Type = typ
	}

	var events []streamEvent
	if values, okValue := obj["value"].([]any); okValue {
		sec.Values = toValueItems(values)
		// 注册帧里的文本是「已经生成完」的整段内容，按顺序一次性下发，
		// 并标记已下发长度，避免后续快照重复发送。
		if sec.Type == "agent-inference" {
			for _, item := range sec.Values {
				if item.Content == "" {
					continue
				}
				events = append(events, eventFor(item.Type, item.Content))
				item.Emitted = len(item.Content)
			}
		}
	}
	if sec.Type == "agent-inference" {
		p.sawInference = true
	}

	idx := len(p.currentIDs)
	p.sections[idx] = sec
	p.currentIDs = append(p.currentIDs, id)
	p.order = append(p.order, idx)
	return events
}

// applyValueContent 处理 value 数组里的文本变化。
// [参数] sec: 目标段落；action: x 或 p；field: 路径后缀；value: 新值。
// [返回] 增量事件与是否产出。
// 最近修改时间：2026-10-18；改动原因：新增，异构数组按真实索引写入。
func (p *streamParser) applyValueContent(sec *section, action, field string, value any) (streamEvent, bool) {
	rest := strings.TrimPrefix(field, "value/")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return streamEvent{}, false
	}
	itemIndex, errAtoi := strconv.Atoi(rest[:slash])
	if errAtoi != nil || rest[slash+1:] != "content" {
		return streamEvent{}, false
	}
	text, okStr := value.(string)
	if !okStr {
		return streamEvent{}, false
	}

	// 索引可能超出当前数组：上游先追加文本元素、后发增量时，
	// values 里已经由 /value/- 建好元素，这里只是兜底补齐。
	for len(sec.Values) <= itemIndex {
		sec.Values = append(sec.Values, &valueItem{})
	}
	target := sec.Values[itemIndex]
	if action == "p" {
		target.Content = text
	} else {
		target.Content += text
	}
	// p 是整段替换，已下发长度要跟着重置；x 是追加，长度自然增长。
	if action == "p" && len(text) < target.Emitted {
		target.Emitted = len(text)
	}

	if sec.Type != "agent-inference" || text == "" {
		return streamEvent{}, false
	}
	target.Emitted = len(target.Content)
	return eventFor(target.Type, text), true
}

// applyRecordMap 从落库回执里提取线程 id 与兜底正文。
// [参数] raw: record-map 行。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，线程 id 与兜底路径。
func (p *streamParser) applyRecordMap(raw map[string]any) {
	recordMap, _ := raw["recordMap"].(map[string]any)
	if recordMap == nil {
		return
	}
	if threads, okThread := recordMap["thread"].(map[string]any); okThread {
		for id := range threads {
			p.threadID = id
		}
	}
}

// parseSectionPath 解析 /s/N/field 形态的路径。
// [参数] path: patch 路径。
// [返回] 段落索引、字段后缀与是否解析成功。
// 最近修改时间：2026-10-18；改动原因：新增，路径解析。
func parseSectionPath(path string) (int, string, bool) {
	if !strings.HasPrefix(path, "/s/") {
		return 0, "", false
	}
	rest := path[len("/s/"):]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return 0, "", false
	}
	idx, errAtoi := strconv.Atoi(rest[:slash])
	if errAtoi != nil {
		return 0, "", false
	}
	return idx, rest[slash+1:], true
}

// toValueItems 把原始数组转成 valueItem 列表。
// [参数] values: 原始数组。
// [返回] 转换结果。
// 最近修改时间：2026-10-18；改动原因：新增，数组转换。
func toValueItems(values []any) []*valueItem {
	out := make([]*valueItem, 0, len(values))
	for _, item := range values {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, toValueItem(obj))
	}
	return out
}

// toValueItem 把单个对象转成 valueItem。
// [参数] obj: 原始对象。
// [返回] 转换结果。
// 最近修改时间：2026-10-18；改动原因：新增，单元素转换。
func toValueItem(obj map[string]any) *valueItem {
	item := &valueItem{}
	if obj == nil {
		return item
	}
	item.Type, _ = obj["type"].(string)
	item.Content, _ = obj["content"].(string)
	return item
}

// toInt 把 JSON 数字转成 int。
// [参数] value: 原始值。
// [返回] 整数值；非数字时返回 0。
// 最近修改时间：2026-10-18；改动原因：新增，数值解析。
func toInt(value any) int {
	if f, ok := value.(float64); ok {
		return int(f)
	}
	return 0
}

// completionText 汇总全部正文增量。
//
// 只取 text 元素：thinking 属于推理过程，混进正文会让客户端看到
// 模型的内心独白。
// [参数] 无。
// [返回] 完整正文。
// 最近修改时间：2026-10-18；改动原因：新增，非流式响应汇总。
func (p *streamParser) completionText() string {
	var builder strings.Builder
	for _, idx := range p.order {
		sec := p.sections[idx]
		if sec == nil || sec.Type != "agent-inference" {
			continue
		}
		for _, item := range sec.Values {
			if item.Type == "text" {
				builder.WriteString(item.Content)
			}
		}
	}
	return builder.String()
}

// reasoningText 汇总全部思考增量。
// [参数] 无。
// [返回] 思考内容。
// 最近修改时间：2026-10-18；改动原因：新增，reasoning 输出。
func (p *streamParser) reasoningText() string {
	var builder strings.Builder
	for _, idx := range p.order {
		sec := p.sections[idx]
		if sec == nil || sec.Type != "agent-inference" {
			continue
		}
		for _, item := range sec.Values {
			if item.Type == "thinking" {
				builder.WriteString(item.Content)
			}
		}
	}
	return builder.String()
}

// usage 返回整条流的 token 统计。
// [参数] 无。
// [返回] 输入与输出 token（取各段落累计）。
// 最近修改时间：2026-10-18；改动原因：新增，用量上报。
func (p *streamParser) usage() (int, int) {
	input, output := 0, 0
	for _, idx := range p.order {
		sec := p.sections[idx]
		if sec == nil || sec.Type != "agent-inference" {
			continue
		}
		input += sec.InputTokens
		output += sec.OutputTokens
	}
	return input, output
}

// finalModel 返回上游实际使用的模型代号。
// [参数] 无。
// [返回] 模型代号；未回报时返回空串。
// 最近修改时间：2026-10-18；改动原因：新增，实际模型回填。
func (p *streamParser) finalModel() string {
	for i := len(p.order) - 1; i >= 0; i-- {
		sec := p.sections[p.order[i]]
		if sec != nil && sec.Model != "" {
			return sec.Model
		}
	}
	return ""
}
