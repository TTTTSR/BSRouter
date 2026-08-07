package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Anthropic Messages SSE ↔ 规范化流式事件。
// Anthropic 本身已是事件结构,解码近乎一一对应;编码端重建其事件帧。

// anthropicSSEData 是 Anthropic SSE 事件的 data 载荷(按事件类型取用部分字段)。
type anthropicSSEData struct {
	Type   string `json:"type"`
	Index  int    `json:"index"`
	ID     string `json:"id"`
	Model  string `json:"model"`
	Role   string `json:"role"`
	Usage  *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// DecodeAnthropicSSE 把 Anthropic SSE 流解码为规范化流式事件。
func DecodeAnthropicSSE(r io.Reader, emit func(StreamEvent) error) error {
	return ReadSSE(r, func(blk SSEBlock) error {
		if blk.Event == "ping" || len(blk.Data) == 0 {
			return nil
		}
		var d anthropicSSEData
		if err := json.Unmarshal(blk.Data, &d); err != nil {
			return nil // 非 JSON 载荷忽略(上游异常)
		}
		var ev StreamEvent
		switch d.Type {
		case "message_start":
			ev = StreamEvent{Type: StreamMessageStart, ID: d.ID, Model: d.Model}
			if d.Message != nil {
				ev.ID = d.Message.ID
				ev.Model = d.Message.Model
				if d.Message.Usage != nil {
					ev.Usage = usageFromParts(
						d.Message.Usage.InputTokens, d.Message.Usage.OutputTokens,
						d.Message.Usage.CacheReadInputTokens, d.Message.Usage.CacheCreationInputTokens)
				}
			}
		case "content_block_start":
			if d.ContentBlock == nil {
				return nil
			}
			ev = StreamEvent{Type: StreamContentStart, Index: d.Index, BlockType: d.ContentBlock.Type,
				BlockID: d.ContentBlock.ID, BlockName: d.ContentBlock.Name}
			if ev.BlockType == "" {
				return nil
			}
		case "content_block_delta":
			if d.Delta == nil {
				return nil
			}
			ev = StreamEvent{Type: StreamContentDelta, Index: d.Index}
			switch d.Delta.Type {
			case "text_delta":
				ev.DeltaType, ev.DeltaText = "text", d.Delta.Text
			case "thinking_delta":
				ev.DeltaType, ev.DeltaText = "thinking", d.Delta.Thinking
			case "input_json_delta":
				ev.DeltaType, ev.PartialJSON = "input_json", d.Delta.PartialJSON
			default:
				return nil // 未知 delta 类型跳过
			}
		case "content_block_stop":
			ev = StreamEvent{Type: StreamContentStop, Index: d.Index}
		case "message_delta":
			ev = StreamEvent{Type: StreamMessageDelta}
			if d.Delta != nil {
				ev.StopReason = d.Delta.StopReason
			}
			if d.Usage != nil {
				ev.Usage = usageFromParts(d.Usage.InputTokens, d.Usage.OutputTokens,
					d.Usage.CacheReadInputTokens, d.Usage.CacheCreationInputTokens)
			}
		case "message_stop":
			ev = StreamEvent{Type: StreamMessageStop}
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

// EncodeAnthropicSSE 把一条规范化流式事件编码为 Anthropic SSE 写入 w。
func EncodeAnthropicSSE(w io.Writer, ev StreamEvent) error {
	var sb strings.Builder
	switch ev.Type {
	case StreamMessageStart:
		msg := map[string]any{
			"id": ev.ID, "type": "message", "role": "assistant",
			"model": ev.Model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil, "usage": usageToAnthropic(ev.Usage),
		}
		writeAnthropicEvent(&sb, "message_start", map[string]any{"type": "message_start", "message": msg})
	case StreamContentStart:
		var block map[string]any
		switch ev.BlockType {
		case "text":
			block = map[string]any{"type": "text", "text": ""}
		case "thinking":
			block = map[string]any{"type": "thinking", "thinking": ""}
		case "tool_use":
			block = map[string]any{"type": "tool_use", "id": ev.BlockID, "name": ev.BlockName, "input": map[string]any{}}
		default:
			return fmt.Errorf("encode anthropic: unknown block type %q", ev.BlockType)
		}
		writeAnthropicEvent(&sb, "content_block_start", map[string]any{"type": "content_block_start", "index": ev.Index, "content_block": block})
	case StreamContentDelta:
		var delta map[string]any
		switch ev.DeltaType {
		case "text":
			delta = map[string]any{"type": "text_delta", "text": ev.DeltaText}
		case "thinking":
			delta = map[string]any{"type": "thinking_delta", "thinking": ev.DeltaText}
		case "input_json":
			delta = map[string]any{"type": "input_json_delta", "partial_json": ev.PartialJSON}
		default:
			return fmt.Errorf("encode anthropic: unknown delta type %q", ev.DeltaType)
		}
		writeAnthropicEvent(&sb, "content_block_delta", map[string]any{"type": "content_block_delta", "index": ev.Index, "delta": delta})
	case StreamContentStop:
		writeAnthropicEvent(&sb, "content_block_stop", map[string]any{"type": "content_block_stop", "index": ev.Index})
	case StreamMessageDelta:
		writeAnthropicEvent(&sb, "message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{"stop_reason": nilIfEmpty(ev.StopReason), "stop_sequence": nil},
			"usage": usageToAnthropic(ev.Usage),
		})
	case StreamMessageStop:
		writeAnthropicEvent(&sb, "message_stop", map[string]any{"type": "message_stop"})
	case StreamError:
		writeAnthropicEvent(&sb, "error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": ev.Error},
		})
	default:
		return fmt.Errorf("encode anthropic: unknown event type %q", ev.Type)
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

func writeAnthropicEvent(sb *strings.Builder, event string, data map[string]any) {
	b, _ := json.Marshal(data)
	sb.WriteString("event: ")
	sb.WriteString(event)
	sb.WriteString("\ndata: ")
	sb.Write(b)
	sb.WriteString("\n\n")
}

func usageFromParts(in, out, cacheRead, cacheCreate int) *Usage {
	u := &Usage{InputTokens: in, OutputTokens: out}
	if cacheRead > 0 {
		u.CacheReadInputTokens = cacheRead
	}
	if cacheCreate > 0 {
		u.CacheCreationInputTokens = cacheCreate
	}
	return u
}

func usageToAnthropic(u *Usage) map[string]any {
	out := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if u != nil {
		out["input_tokens"] = u.InputTokens
		out["output_tokens"] = u.OutputTokens
		if u.CacheReadInputTokens > 0 {
			out["cache_read_input_tokens"] = u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens > 0 {
			out["cache_creation_input_tokens"] = u.CacheCreationInputTokens
		}
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
