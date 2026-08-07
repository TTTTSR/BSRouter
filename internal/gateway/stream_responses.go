package gateway

import (
	"encoding/json"
	"fmt"
	"io"
)

// OpenAI Responses SSE ↔ 规范化流式事件。
// 解码端把 response.* 事件映射为规范化事件;编码端重建 Responses 输出项生命周期
// (output_item.added → 增量 → output_item.done → response.completed)。

// responsesSSEData 是 Responses SSE 事件(取用所需字段)。
type responsesSSEData struct {
	Type        string `json:"type"`
	OutputIndex *int   `json:"output_index"`
	Delta       string `json:"delta"`
	Item        *struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		CallID  string `json:"call_id"`
		Name    string `json:"name"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`
	Response *struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Usage  *struct {
			InputTokens     int `json:"input_tokens"`
			OutputTokens    int `json:"output_tokens"`
			TotalTokens     int `json:"total_tokens"`
			InputDetails    *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// DecodeResponsesSSE 把 OpenAI Responses SSE 流解码为规范化流式事件。
func DecodeResponsesSSE(r io.Reader, emit func(StreamEvent) error) error {
	return ReadSSE(r, func(blk SSEBlock) error {
		if len(blk.Data) == 0 {
			return nil
		}
		var d responsesSSEData
		if err := json.Unmarshal(blk.Data, &d); err != nil {
			return nil
		}
		idx := -1
		if d.OutputIndex != nil {
			idx = *d.OutputIndex
		}
		var ev StreamEvent
		switch d.Type {
		case "response.created":
			ev = StreamEvent{Type: StreamMessageStart}
			if d.Response != nil {
				ev.ID, ev.Model = d.Response.ID, d.Response.Model
			}
		case "response.output_item.added":
			if d.Item == nil {
				return nil
			}
			ev = StreamEvent{Type: StreamContentStart, Index: idx}
			switch d.Item.Type {
			case "message":
				ev.BlockType = "text"
			case "function_call":
				ev.BlockType = "tool_use"
				ev.BlockID, ev.BlockName = d.Item.CallID, d.Item.Name
			case "reasoning":
				ev.BlockType = "thinking"
			default:
				return nil // 其它输出项(web_search_call 等)跳过
			}
		case "response.output_text.delta":
			ev = StreamEvent{Type: StreamContentDelta, Index: idx, DeltaType: "text", DeltaText: d.Delta}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			ev = StreamEvent{Type: StreamContentDelta, Index: idx, DeltaType: "thinking", DeltaText: d.Delta}
		case "response.function_call_arguments.delta":
			ev = StreamEvent{Type: StreamContentDelta, Index: idx, DeltaType: "input_json", PartialJSON: d.Delta}
		case "response.output_item.done":
			ev = StreamEvent{Type: StreamContentStop, Index: idx}
		case "response.completed":
			ev = StreamEvent{Type: StreamMessageDelta}
			if d.Response != nil {
				ev.StopReason = statusToStopReason(d.Response.Status)
				if d.Response.Usage != nil {
					u := d.Response.Usage
					cached := 0
					if u.InputDetails != nil {
						cached = u.InputDetails.CachedTokens
					}
					ev.Usage = usageFromParts(u.InputTokens, u.OutputTokens, cached, 0)
				}
			}
			if err := emit(ev); err != nil {
				return err
			}
			return emit(StreamEvent{Type: StreamMessageStop})
		case "response.failed", "response.incomplete":
			ev = StreamEvent{Type: StreamMessageDelta}
			if d.Response != nil {
				ev.StopReason = statusToStopReason(d.Response.Status)
			}
			if err := emit(ev); err != nil {
				return err
			}
			return emit(StreamEvent{Type: StreamMessageStop})
		case "error":
			msg := ""
			if d.Error != nil {
				msg = d.Error.Message
			}
			ev = StreamEvent{Type: StreamError, Error: msg}
		default:
			return nil
		}
		return emit(ev)
	})
}

func statusToStopReason(status string) string {
	switch status {
	case "incomplete":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// ---- 编码器:规范化 → OpenAI Responses SSE ----

// respItemState 跟踪一个规范化内容块在 Responses 输出项中的状态。
type respItemState struct {
	id   string
	typ  string // message / function_call / reasoning
	call string // function_call 的 call_id
	name string
	text string // 累积文本(message / reasoning summary)
	args string // 累积参数(function_call)
}

// responsesEncoder 是有状态的规范化 → Responses 编码器。
type responsesEncoder struct {
	responseID string
	model      string
	items      map[int]*respItemState
	seq        int
}

// EncodeResponsesSSE 编码一条规范化事件为 Responses SSE 写入 w。
func EncodeResponsesSSE(w io.Writer, ev StreamEvent) error {
	return (&responsesEncoder{items: map[int]*respItemState{}}).Encode(w, ev)
}

func (e *responsesEncoder) Encode(w io.Writer, ev StreamEvent) error {
	switch ev.Type {
	case StreamMessageStart:
		e.responseID = ev.ID
		if e.responseID == "" {
			e.responseID = "resp_bsrouter"
		}
		e.model = ev.Model
		return e.writeEvent(w, "response.created", map[string]any{
			"response": map[string]any{
				"id": e.responseID, "object": "response", "created_at": 0,
				"status": "in_progress", "model": e.model, "output": []any{}, "usage": nil,
			},
		})
	case StreamContentStart:
		st := e.item(ev.Index)
		var item map[string]any
		switch ev.BlockType {
		case "text":
			st.typ = "message"
			item = map[string]any{"type": "message", "id": st.id, "status": "in_progress", "role": "assistant", "content": []any{}}
		case "thinking":
			st.typ = "reasoning"
			item = map[string]any{"type": "reasoning", "id": st.id, "status": "in_progress", "summary": []any{}, "content": []any{}}
		case "tool_use":
			st.typ = "function_call"
			st.call, st.name = ev.BlockID, ev.BlockName
			item = map[string]any{"type": "function_call", "id": st.id, "call_id": ev.BlockID, "name": ev.BlockName, "arguments": ""}
		default:
			return nil
		}
		return e.writeEvent(w, "response.output_item.added", map[string]any{"output_index": ev.Index, "item": item})
	case StreamContentDelta:
		st := e.item(ev.Index)
		switch ev.DeltaType {
		case "text":
			st.text += ev.DeltaText
			return e.writeEvent(w, "response.output_text.delta", map[string]any{
				"item_id": st.id, "output_index": ev.Index, "content_index": 0, "delta": ev.DeltaText,
			})
		case "thinking":
			st.text += ev.DeltaText
			return e.writeEvent(w, "response.reasoning_summary_text.delta", map[string]any{
				"item_id": st.id, "output_index": ev.Index, "delta": ev.DeltaText,
			})
		case "input_json":
			st.args += ev.PartialJSON
			return e.writeEvent(w, "response.function_call_arguments.delta", map[string]any{
				"item_id": st.id, "output_index": ev.Index, "delta": ev.PartialJSON,
			})
		}
		return nil
	case StreamContentStop:
		st := e.item(ev.Index)
		var item map[string]any
		switch st.typ {
		case "message":
			item = map[string]any{"type": "message", "id": st.id, "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": st.text, "annotations": []any{}}}}
		case "function_call":
			item = map[string]any{"type": "function_call", "id": st.id, "call_id": st.call, "name": st.name, "arguments": st.args}
		case "reasoning":
			item = map[string]any{"type": "reasoning", "id": st.id, "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": st.text}},
				"content": []any{}}
		default:
			item = map[string]any{"type": st.typ, "id": st.id}
		}
		return e.writeEvent(w, "response.output_item.done", map[string]any{"output_index": ev.Index, "item": item})
	case StreamMessageDelta:
		status := "completed"
		if ev.StopReason == "max_tokens" {
			status = "incomplete"
		}
		usage := usageToResponses(ev.Usage)
		return e.writeEvent(w, "response.completed", map[string]any{
			"response": map[string]any{
				"id": e.responseID, "object": "response", "created_at": 0,
				"status": status, "model": e.model, "output": []any{}, "usage": usage,
			},
		})
	case StreamMessageStop:
		return nil // response.completed 已是终态
	case StreamError:
		return e.writeEvent(w, "error", map[string]any{"code": "stream_error", "message": ev.Error})
	}
	return nil
}

func (e *responsesEncoder) item(index int) *respItemState {
	if st, ok := e.items[index]; ok {
		return st
	}
	e.seq++
	st := &respItemState{id: fmt.Sprintf("item_%d", e.seq)}
	e.items[index] = st
	return st
}

func (e *responsesEncoder) writeEvent(w io.Writer, typ string, payload map[string]any) error {
	obj := map[string]any{"type": typ}
	for k, v := range payload {
		obj[k] = v
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func usageToResponses(u *Usage) map[string]any {
	out := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if u != nil {
		out["input_tokens"] = u.InputTokens
		out["output_tokens"] = u.OutputTokens
		out["total_tokens"] = u.InputTokens + u.OutputTokens
		if u.CacheReadInputTokens > 0 {
			out["input_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadInputTokens}
		}
	}
	return out
}
