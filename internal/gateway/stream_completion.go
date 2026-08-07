package gateway

import (
	"encoding/json"
	"fmt"
	"io"
)

// OpenAI chat.completions SSE ↔ 规范化流式事件。
// 解码端有状态:OpenAI 流是 data 块,需把 role/content/reasoning/tool_calls 增量
// 组织成规范化的 message_start / content_start / content_delta / content_stop /
// message_delta / message_stop;message_delta 缓存到 [DONE] 才发(去重 + 收齐 usage)。

const infiniteWhitespaceThreshold = 500

// completionStreamChunk 是 OpenAI chat.completions 流式块。
type completionStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// toolStreamState 跟踪 OpenAI 一个 tool_calls[index] 的规范化块状态。
type toolStreamState struct {
	index      int    // 规范化 content 块索引
	id         string
	name       string
	started    bool
	pending    string // id/name 到达前累积的参数
	whitespace int    // 连续空白计数(防御 Copilot 无限空白 bug)
	aborted    bool
}

// completionDecoder 把 OpenAI SSE 块转换为规范化流式事件。
type completionDecoder struct {
	emit       func(StreamEvent) error
	sentStart  bool
	nextIndex  int
	curType    string // 当前打开的 text/thinking 块类型("" = 无)
	curIndex   int
	tools      map[int]*toolStreamState
	openTools  map[int]bool
	hasDelta   bool
	pending    *StreamEvent // 缓存的 message_delta
	latestUsg  *Usage
	done       bool
	errClosed  bool
}

// DecodeCompletionSSE 把 OpenAI chat.completions SSE 流解码为规范化流式事件。
func DecodeCompletionSSE(r io.Reader, emit func(StreamEvent) error) error {
	d := &completionDecoder{emit: emit, tools: map[int]*toolStreamState{}, openTools: map[int]bool{}}
	err := ReadSSE(r, d.handle)
	if err != nil {
		d.errClosed = true
		_ = d.emit(StreamEvent{Type: StreamError, Error: err.Error()})
		return err
	}
	return d.finish()
}

func (d *completionDecoder) handle(blk SSEBlock) error {
	if string(blk.Data) == "[DONE]" {
		return d.onDone()
	}
	var chunk completionStreamChunk
	if err := json.Unmarshal(blk.Data, &chunk); err != nil {
		return nil // 非 JSON 块忽略
	}
	if !d.sentStart {
		var usg *Usage
		if chunk.Usage != nil {
			usg = usageFromOpenAI(chunk.Usage)
		}
		if err := d.emit(StreamEvent{Type: StreamMessageStart, ID: chunk.ID, Model: chunk.Model, Usage: usg}); err != nil {
			return err
		}
		d.sentStart = true
	}
	if chunk.Usage != nil {
		d.latestUsg = usageFromOpenAI(chunk.Usage)
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	c := chunk.Choices[0]
	delta := c.Delta

	reasoning := delta.Reasoning
	if reasoning == "" {
		reasoning = delta.ReasoningContent
	}
	if reasoning != "" {
		if err := d.ensureNonToolBlock("thinking"); err != nil {
			return err
		}
		if err := d.emit(StreamEvent{Type: StreamContentDelta, Index: d.curIndex, DeltaType: "thinking", DeltaText: reasoning}); err != nil {
			return err
		}
	}
	if delta.Content != "" {
		if err := d.ensureNonToolBlock("text"); err != nil {
			return err
		}
		if err := d.emit(StreamEvent{Type: StreamContentDelta, Index: d.curIndex, DeltaType: "text", DeltaText: delta.Content}); err != nil {
			return err
		}
	}
	if len(delta.ToolCalls) > 0 {
		if err := d.closeNonTool(); err != nil {
			return err
		}
		for _, tc := range delta.ToolCalls {
			if err := d.handleToolCall(tc); err != nil {
				return err
			}
		}
	}
	if c.FinishReason != nil {
		if d.hasDelta {
			// 后续带 finish_reason 的块(OpenRouter 多发):仅更新 usage,不重复收尾。
			if d.pending != nil && d.latestUsg != nil {
				d.pending.Usage = d.latestUsg
			}
			return nil
		}
		d.hasDelta = true
		if err := d.closeBlocks(); err != nil {
			return err
		}
		d.pending = &StreamEvent{Type: StreamMessageDelta, StopReason: mapStopReason(*c.FinishReason), Usage: d.latestUsg}
	}
	return nil
}

// ensureNonToolBlock 确保当前打开的块类型为 kind(text/thinking),必要时切换。
func (d *completionDecoder) ensureNonToolBlock(kind string) error {
	if d.curType == kind {
		return nil
	}
	if err := d.closeNonTool(); err != nil {
		return err
	}
	d.curType = kind
	d.curIndex = d.nextIndex
	d.nextIndex++
	bt := kind
	if err := d.emit(StreamEvent{Type: StreamContentStart, Index: d.curIndex, BlockType: bt}); err != nil {
		return err
	}
	return nil
}

func (d *completionDecoder) closeNonTool() error {
	if d.curType == "" {
		return nil
	}
	err := d.emit(StreamEvent{Type: StreamContentStop, Index: d.curIndex})
	d.curType, d.curIndex = "", 0
	return err
}

func (d *completionDecoder) handleToolCall(tc struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}) error {
	st := d.tools[tc.Index]
	if st == nil {
		st = &toolStreamState{index: d.nextIndex}
		d.nextIndex++
		d.tools[tc.Index] = st
	}
	if st.aborted {
		return nil
	}
	if tc.ID != "" {
		st.id = tc.ID
	}
	if tc.Function.Name != "" {
		st.name = tc.Function.Name
	}
	shouldStart := !st.started && st.id != "" && st.name != ""

	args := tc.Function.Arguments
	if args != "" {
		// 无限空白 bug 防御(Copilot):连续空白超阈值中止该工具流。
		for _, ch := range args {
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
				st.whitespace++
			} else {
				st.whitespace = 0
			}
		}
		if st.whitespace >= infiniteWhitespaceThreshold {
			st.aborted = true
			return nil
		}
	}

	if shouldStart {
		st.started = true
		if err := d.emit(StreamEvent{Type: StreamContentStart, Index: st.index, BlockType: "tool_use", BlockID: st.id, BlockName: st.name}); err != nil {
			return err
		}
		d.openTools[st.index] = true
		// 补发 start 之前累积的参数(id/name 晚到时)。
		if st.pending != "" {
			if err := d.emit(StreamEvent{Type: StreamContentDelta, Index: st.index, DeltaType: "input_json", PartialJSON: st.pending}); err != nil {
				return err
			}
			st.pending = ""
		}
	}
	if args != "" {
		if st.started {
			if err := d.emit(StreamEvent{Type: StreamContentDelta, Index: st.index, DeltaType: "input_json", PartialJSON: args}); err != nil {
				return err
			}
		} else {
			st.pending += args
		}
	}
	return nil
}

// closeBlocks 收尾:关闭当前非工具块、补发 id/name 迟到的工具块、关闭所有打开的工具块。
func (d *completionDecoder) closeBlocks() error {
	if err := d.closeNonTool(); err != nil {
		return err
	}
	// 有载荷但从未 start 的工具块(OpenAI 部分上游把 arguments 先于 id/name 发出)。
	var late [][]int
	for idx, st := range d.tools {
		if st.started {
			continue
		}
		if st.id == "" && st.name == "" && st.pending == "" {
			continue
		}
		st.started = true
		late = append(late, []int{st.index, idx})
	}
	// 按规范化索引排序补发
	for i := 0; i < len(late); i++ {
		for j := i + 1; j < len(late); j++ {
			if late[j][0] < late[i][0] {
				late[i], late[j] = late[j], late[i]
			}
		}
	}
	for _, pair := range late {
		st := d.tools[pair[1]]
		if err := d.emit(StreamEvent{Type: StreamContentStart, Index: st.index, BlockType: "tool_use", BlockID: st.id, BlockName: st.name}); err != nil {
			return err
		}
		d.openTools[st.index] = true
		if st.pending != "" {
			if err := d.emit(StreamEvent{Type: StreamContentDelta, Index: st.index, DeltaType: "input_json", PartialJSON: st.pending}); err != nil {
				return err
			}
		}
	}
	// 按规范化索引升序关闭所有打开的工具块。
	indices := make([]int, 0, len(d.openTools))
	for idx := range d.openTools {
		indices = append(indices, idx)
	}
	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if indices[j] < indices[i] {
				indices[i], indices[j] = indices[j], indices[i]
			}
		}
	}
	for _, idx := range indices {
		if err := d.emit(StreamEvent{Type: StreamContentStop, Index: idx}); err != nil {
			return err
		}
	}
	d.openTools = map[int]bool{}
	return nil
}

func (d *completionDecoder) onDone() error {
	if !d.hasDelta {
		if err := d.closeBlocks(); err != nil {
			return err
		}
		d.pending = &StreamEvent{Type: StreamMessageDelta, Usage: d.latestUsg}
	}
	if d.pending != nil {
		if err := d.emit(*d.pending); err != nil {
			return err
		}
		d.pending = nil
	}
	d.done = true
	return d.emit(StreamEvent{Type: StreamMessageStop})
}

// finish 流自然结束(未收到 [DONE])时收尾;出错时不补发 message_delta/message_stop。
func (d *completionDecoder) finish() error {
	if d.errClosed || d.done {
		return nil
	}
	if !d.sentStart {
		// 空流:补一个最小 message_start + message_delta + message_stop,避免客户端挂起。
		if err := d.emit(StreamEvent{Type: StreamMessageStart}); err != nil {
			return err
		}
		if !d.hasDelta {
			if err := d.closeBlocks(); err != nil {
				return err
			}
			d.pending = &StreamEvent{Type: StreamMessageDelta, Usage: d.latestUsg}
		}
	}
	if d.pending != nil {
		if err := d.emit(*d.pending); err != nil {
			return err
		}
		d.pending = nil
	}
	return d.emit(StreamEvent{Type: StreamMessageStop})
}

func usageFromOpenAI(u *struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
}) *Usage {
	cached, cacheWrite := 0, 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
		cacheWrite = u.PromptTokensDetails.CacheWriteTokens
	}
	in := u.PromptTokens - cached - cacheWrite
	if in < 0 {
		in = 0
	}
	return usageFromParts(in, u.CompletionTokens, cached, cacheWrite)
}

// mapStopReason 映射 OpenAI finish_reason → 规范化 stop_reason。
func mapStopReason(f string) string {
	switch f {
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// ---- 编码器:规范化 → OpenAI chat.completions SSE ----

// completionEncoder 是有状态的规范化 → OpenAI 编码器(记录 id/model、工具块索引映射)。
type completionEncoder struct {
	id          string
	model       string
	nextToolIdx int
	toolIndex   map[int]int // 规范化 content 索引 → OpenAI tool_calls 索引
}

// EncodeCompletionSSE 编码一条规范化事件为 OpenAI SSE 写入 w。
func EncodeCompletionSSE(w io.Writer, ev StreamEvent) error {
	return (&completionEncoder{}).Encode(w, ev)
}

func (e *completionEncoder) Encode(w io.Writer, ev StreamEvent) error {
	switch ev.Type {
	case StreamMessageStart:
		e.id, e.model = ev.ID, ev.Model
		return e.write(w, map[string]any{"role": "assistant"}, nil)
	case StreamContentStart:
		if ev.BlockType == "tool_use" {
			idx := e.nextToolIdx
			e.nextToolIdx++
			if e.toolIndex == nil {
				e.toolIndex = map[int]int{}
			}
			e.toolIndex[ev.Index] = idx
			return e.write(w, map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "id": ev.BlockID, "type": "function",
				"function": map[string]any{"name": ev.BlockName},
			}}}, nil)
		}
		return nil // text/thinking 块在 OpenAI 无 start,delta 自带内容
	case StreamContentDelta:
		switch ev.DeltaType {
		case "text":
			return e.write(w, map[string]any{"content": ev.DeltaText}, nil)
		case "thinking":
			return e.write(w, map[string]any{"reasoning_content": ev.DeltaText}, nil)
		case "input_json":
			idx, ok := e.toolIndex[ev.Index]
			if !ok {
				idx = e.nextToolIdx
				e.nextToolIdx++
				if e.toolIndex == nil {
					e.toolIndex = map[int]int{}
				}
				e.toolIndex[ev.Index] = idx
			}
			return e.write(w, map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "function": map[string]any{"arguments": ev.PartialJSON},
			}}}, nil)
		}
		return nil
	case StreamContentStop:
		return nil // OpenAI 无块 stop
	case StreamMessageDelta:
		return e.write(w, map[string]any{}, finishFromStop(ev.StopReason))
	case StreamMessageStop:
		_, err := io.WriteString(w, "data: [DONE]\n\n")
		return err
	case StreamError:
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Error}})
		_, err := fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}
	return nil
}

// write 写一条 OpenAI 流式块。
func (e *completionEncoder) write(w io.Writer, delta any, finish any) error {
	chunk := map[string]any{
		"id": e.id, "object": "chat.completion.chunk", "created": 0, "model": e.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// finishFromStop 映射规范化 stop_reason → OpenAI finish_reason。
func finishFromStop(s string) any {
	switch s {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "", "end_turn":
		return "stop"
	default:
		return s
	}
}
