package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func intPtr(i int) *int         { return &i }
func f64Ptr(f float64) *float64 { return &f }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// 一条含工具调用的完整对话流:user -> assistant(tool_use) -> tool -> assistant(text)。
func toolFlowAnthropic() *AnthropicRequest {
	return &AnthropicRequest{
		Model:       "claude-sonnet-4-5",
		MaxTokens:   2048,
		System:      "You are a helpful assistant.",
		Temperature: f64Ptr(0.5),
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "What's the weather in SF?"}}},
			{Role: "assistant", Content: AnthropicContent{
				{Type: "tool_use", ID: "toolu_01", Name: "get_weather", Input: json.RawMessage(`{"city":"SF"}`)},
			}},
			{Role: "user", Content: AnthropicContent{
				{Type: "tool_result", ToolUseID: "toolu_01", Content: AnthropicContent{{Type: "text", Text: "72F, sunny"}}},
			}},
			{Role: "assistant", Content: AnthropicContent{{Type: "text", Text: "It is 72F and sunny in SF."}}},
		},
		Tools: []AnthropicTool{{
			Name: "get_weather", Description: "Get weather for a city",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}
}

func TestAnthropicRequestRoundTrip(t *testing.T) {
	wire := toolFlowAnthropic()
	got := wire.ToInternal().ToAnthropic()
	if mustJSON(t, got) != mustJSON(t, wire) {
		t.Fatalf("anthropic round-trip mismatch:\n got  %s\n want %s", mustJSON(t, got), mustJSON(t, wire))
	}
}

func TestCompletionRequestRoundTrip(t *testing.T) {
	wire := &CompletionRequest{
		Model:     "gpt-4o",
		MaxTokens: intPtr(1024),
		Messages: []CompletionMessage{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: "Weather in SF?"},
			{Role: "assistant", ToolCalls: []CompletionToolCall{
				{ID: "call_1", Type: "function", Function: CompletionFunctionCall{Name: "get_weather", Arguments: `{"city":"SF"}`}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "72F"},
			{Role: "assistant", Content: "It is 72F."},
		},
		Tools: []CompletionTool{{
			Type: "function",
			Function: CompletionFunction{
				Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
	}
	got := wire.ToInternal().ToCompletion()
	if mustJSON(t, got) != mustJSON(t, wire) {
		t.Fatalf("completion round-trip mismatch:\n got  %s\n want %s", mustJSON(t, got), mustJSON(t, wire))
	}
}

func TestResponsesRequestRoundTrip(t *testing.T) {
	wire := &ResponsesRequest{
		Model:        "gpt-5",
		Instructions: "You are helpful.",
		Input: []ResponsesItem{
			{Type: "message", Role: "user", Content: []ResponsesContent{{Type: "input_text", Text: "Weather in SF?"}}},
			{Type: "function_call", CallID: "fc_1", Name: "get_weather", Arguments: `{"city":"SF"}`},
			{Type: "function_call_output", CallID: "fc_1", Output: "72F"},
			{Type: "message", Role: "assistant", Content: []ResponsesContent{{Type: "output_text", Text: "It is 72F."}}},
		},
		Tools: []ResponsesTool{{
			Type: "function", Name: "get_weather", Description: "weather",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}
	got := wire.ToInternal().ToResponses()
	if mustJSON(t, got) != mustJSON(t, wire) {
		t.Fatalf("responses round-trip mismatch:\n got  %s\n want %s", mustJSON(t, got), mustJSON(t, wire))
	}
}

// 请求侧跨格式转换:Anthropic 请求体经中间类型转成 chat.completions 与 responses。
func TestCrossFormatRequest(t *testing.T) {
	canonical := toolFlowAnthropic().ToInternal()

	comp := canonical.ToCompletion()
	if len(comp.Messages) != 5 {
		t.Fatalf("completion messages = %d, want 5", len(comp.Messages))
	}
	if comp.Messages[0].Role != "system" || comp.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("completion system message = %+v", comp.Messages[0])
	}
	if comp.Messages[1].Role != "user" || comp.Messages[1].Content != "What's the weather in SF?" {
		t.Errorf("completion user message = %+v", comp.Messages[1])
	}
	assistant := comp.Messages[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "toolu_01" ||
		assistant.ToolCalls[0].Function.Name != "get_weather" ||
		assistant.ToolCalls[0].Function.Arguments != `{"city":"SF"}` {
		t.Errorf("completion assistant tool_calls = %+v", assistant.ToolCalls)
	}
	if comp.Messages[3].Role != "tool" || comp.Messages[3].ToolCallID != "toolu_01" || comp.Messages[3].Content != "72F, sunny" {
		t.Errorf("completion tool message = %+v", comp.Messages[3])
	}
	if len(comp.Tools) != 1 || comp.Tools[0].Function.Name != "get_weather" {
		t.Errorf("completion tools = %+v", comp.Tools)
	}

	respReq := canonical.ToResponses()
	if respReq.Instructions != "You are a helpful assistant." {
		t.Errorf("responses instructions = %q", respReq.Instructions)
	}
	if len(respReq.Input) != 4 {
		t.Fatalf("responses input items = %d, want 4", len(respReq.Input))
	}
	want := []struct{ typ, role string }{
		{"message", "user"},
		{"function_call", ""},
		{"function_call_output", ""},
		{"message", "assistant"},
	}
	for i, w := range want {
		it := respReq.Input[i]
		if it.Type != w.typ || (w.role != "" && it.Role != w.role) {
			t.Errorf("input[%d] = %+v, want type=%s role=%s", i, it, w.typ, w.role)
		}
	}
	if respReq.Input[1].Name != "get_weather" || respReq.Input[1].CallID != "toolu_01" ||
		respReq.Input[1].Arguments != `{"city":"SF"}` {
		t.Errorf("input[1] function_call = %+v", respReq.Input[1])
	}
	if respReq.Input[2].CallID != "toolu_01" || respReq.Input[2].Output != "72F, sunny" {
		t.Errorf("input[2] function_call_output = %+v", respReq.Input[2])
	}
	if len(respReq.Tools) != 1 || respReq.Tools[0].Name != "get_weather" {
		t.Errorf("responses tools = %+v", respReq.Tools)
	}

	// 任意格式经中间类型再转回,语义一致。
	back := canonical.ToResponses().ToInternal().ToCompletion()
	if mustJSON(t, back) != mustJSON(t, comp) {
		t.Fatalf("completion -> responses -> completion mismatch:\n got  %s\n want %s", mustJSON(t, back), mustJSON(t, comp))
	}
}

// 响应侧跨格式转换:规范化响应 -> 各格式响应体 -> 规范化响应,可还原字段保持一致。
func TestResponseCrossFormat(t *testing.T) {
	canon := &Response{
		Model:        "gpt-5",
		Content:      "It is 72F.",
		ToolCalls:    []ToolCall{{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"SF"}`)}},
		FinishReason: "tool_use",
		Usage:        Usage{InputTokens: 10, OutputTokens: 4},
	}
	if got := canon.ToCompletion().ToInternal(); mustJSON(t, got) != mustJSON(t, canon) {
		t.Fatalf("completion response round-trip:\n got  %s\n want %s", mustJSON(t, got), mustJSON(t, canon))
	}
	if got := canon.ToAnthropic().ToInternal(); mustJSON(t, got) != mustJSON(t, canon) {
		t.Fatalf("anthropic response round-trip:\n got  %s\n want %s", mustJSON(t, got), mustJSON(t, canon))
	}
	// responses 的 wire 没有 finish_reason,工具调用时只能还原为 stop。
	got := canon.ToResponses().ToInternal()
	if got.Content != canon.Content || len(got.ToolCalls) != 1 || got.FinishReason != "stop" {
		t.Errorf("responses response round-trip = %+v, want content=%q tool_calls=1 finish=stop", got, canon.Content)
	}
	if got.Usage != canon.Usage {
		t.Errorf("responses usage = %+v, want %+v", got.Usage, canon.Usage)
	}

	// 纯文本 + stop 结束原因,三种格式都能完整还原。
	text := &Response{Model: "gpt-5", Content: "Hi", FinishReason: "stop", Usage: Usage{InputTokens: 3, OutputTokens: 1}}
	for name, roundTrip := range map[string]func() *Response{
		"completion": func() *Response { return text.ToCompletion().ToInternal() },
		"anthropic":  func() *Response { return text.ToAnthropic().ToInternal() },
		"responses":  func() *Response { return text.ToResponses().ToInternal() },
	} {
		if got := roundTrip(); mustJSON(t, got) != mustJSON(t, text) {
			t.Errorf("%s text round-trip:\n got  %s\n want %s", name, mustJSON(t, got), mustJSON(t, text))
		}
	}
}

// 不同格式的响应体之间也可以直接互转(经中间类型)。
func TestResponseBetweenFormats(t *testing.T) {
	compResp := &CompletionResponse{
		ID:      "chatcmpl-1",
		Model:   "gpt-4o",
		Choices: []CompletionChoice{{Index: 0, Message: CompletionMessage{Role: "assistant", Content: "Hello"}}},
		Usage:   CompletionUsage{PromptTokens: 4, CompletionTokens: 1},
	}
	anthropic := compResp.ToInternal().ToAnthropic()
	if anthropic.Model != "gpt-4o" || anthropic.Content[0].Text != "Hello" {
		t.Errorf("completion -> anthropic = %+v", anthropic)
	}
	if anthropic.Usage.InputTokens != 4 || anthropic.Usage.OutputTokens != 1 {
		t.Errorf("anthropic usage = %+v", anthropic.Usage)
	}
}

// 客户端(如 Claude Code 2.1.221)会把并行的多个工具结果批量合并进同一条 user
// 消息(多个 tool_result 块)。此前 ToInternal 只生成一条 tool 消息、ToolCallID
// 被逐块覆盖,导致先前的 tool_use 失去配对,被 DeepSeek 等严格校验上游以 400
// 拒绝("tool_use ids were found without tool_result blocks immediately after")。
func TestAnthropicBatchedToolResultsRoundTrip(t *testing.T) {
	wire := &AnthropicRequest{
		Model:     "deepseek-v4-flash",
		MaxTokens: 32000,
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "find files"}}},
			{Role: "assistant", Content: AnthropicContent{
				{Type: "tool_use", ID: "call_00_glob", Name: "Glob", Input: json.RawMessage(`{"pattern":"*.go"}`)},
				{Type: "tool_use", ID: "call_01_grep", Name: "Grep", Input: json.RawMessage(`{"pattern":"todo"}`)},
			}},
			{Role: "user", Content: AnthropicContent{
				{Type: "tool_result", ToolUseID: "call_00_glob", Content: AnthropicContent{{Type: "text", Text: "a.go\nb.go"}}},
				{Type: "tool_result", ToolUseID: "call_01_grep", Content: AnthropicContent{{Type: "text", Text: "todo in a.go:1"}}},
			}},
			{Role: "assistant", Content: AnthropicContent{{Type: "text", Text: "done"}}},
		},
	}
	canonical := wire.ToInternal()
	if len(canonical.Messages) != 5 {
		t.Fatalf("canonical messages = %d, want 5", len(canonical.Messages))
	}
	if m := canonical.Messages[2]; m.Role != RoleTool || m.ToolCallID != "call_00_glob" || m.Content != "a.go\nb.go" {
		t.Errorf("first tool result = %+v", m)
	}
	if m := canonical.Messages[3]; m.Role != RoleTool || m.ToolCallID != "call_01_grep" || m.Content != "todo in a.go:1" {
		t.Errorf("second tool result = %+v", m)
	}
	// 回程:两个结果保持各自 tool_use_id 与内容,与原始 wire 一致(逐块独立配对)。
	if got := canonical.ToAnthropic(); mustJSON(t, got) != mustJSON(t, wire) {
		t.Fatalf("batched tool_result round-trip mismatch:\n got  %s\n want %s", mustJSON(t, got), mustJSON(t, wire))
	}
}

// 孤儿 tool_use(调用后没有对应 tool_result,如工具调用被中断)应被丢弃,
// 否则 DeepSeek 等上游以 400 拒绝;纯孤儿的 assistant 消息连同其后
// 的孤儿 tool 结果一并删除。
func TestAnthropicOrphanToolUseDropped(t *testing.T) {
	wire := &AnthropicRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: AnthropicContent{
				{Type: "tool_use", ID: "call_00_lost", Name: "Glob", Input: json.RawMessage(`{}`)},
				{Type: "tool_use", ID: "call_01_ok", Name: "Grep", Input: json.RawMessage(`{}`)},
			}},
			{Role: "user", Content: AnthropicContent{
				{Type: "tool_result", ToolUseID: "call_01_ok", Content: AnthropicContent{{Type: "text", Text: "hit"}}},
			}},
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "next"}}},
		},
	}
	canonical := wire.ToInternal()
	if len(canonical.Messages) != 4 {
		t.Fatalf("canonical messages = %d, want 4", len(canonical.Messages))
	}
	if m := canonical.Messages[1]; m.Role != RoleAssistant || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_01_ok" {
		t.Errorf("assistant after repair = %+v", m)
	}
	if m := canonical.Messages[2]; m.Role != RoleTool || m.ToolCallID != "call_01_ok" {
		t.Errorf("tool result after repair = %+v", m)
	}
	forged := mustJSON(t, canonical.ToAnthropic())
	if strings.Contains(forged, "call_00_lost") {
		t.Errorf("orphan tool_use should be dropped, got %s", forged)
	}
	if !strings.Contains(forged, "call_01_ok") {
		t.Errorf("paired tool_use must be kept, got %s", forged)
	}
}

// 纯孤儿场景:assistant 消息仅含孤儿 tool_use 且后无结果,整条删除。
func TestAnthropicOrphanToolUseEmptyAssistant(t *testing.T) {
	wire := &AnthropicRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: AnthropicContent{
				{Type: "tool_use", ID: "call_00_lost", Name: "Glob", Input: json.RawMessage(`{}`)},
			}},
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "next"}}},
		},
	}
	canonical := wire.ToInternal()
	if len(canonical.Messages) != 2 {
		t.Fatalf("canonical messages = %d, want 2", len(canonical.Messages))
	}
	if canonical.Messages[0].Role != RoleUser || canonical.Messages[1].Role != RoleUser {
		t.Errorf("messages after repair = %+v", canonical.Messages)
	}
	if got := mustJSON(t, canonical.ToAnthropic()); strings.Contains(got, "tool_use") {
		t.Errorf("empty assistant should be removed, got %s", got)
	}
}
