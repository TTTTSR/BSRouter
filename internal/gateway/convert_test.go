package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func intPtr(i int) *int         { return &i }
func f64Ptr(f float64) *float64 { return &f }
func strPtr(s string) *string   { return &s }

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

// OpenAI 客户端(如 pi/OpenAI SDK)把 content 发成内容块数组时,解码必须归一化为纯文本
// (原 bug:content 字段仅支持 string,数组直接 400 拒绝)。
func TestCompletionRequestArrayContentDecodes(t *testing.T) {
	wire := `{
		"model": "gpt-4o",
		"stream": true,
		"messages": [
			{"role": "system", "content": "Be brief."},
			{"role": "user", "content": [{"type": "text", "text": "Hello"}, {"type": "text", "text": "world"}]},
			{"role": "tool", "tool_call_id": "call_1", "content": [{"type": "text", "text": "72F, sunny"}]},
			{"role": "assistant", "content": [{"type": "text", "text": "It is 72F."}]}
		]
	}`
	var req CompletionRequest
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatalf("decode array content: %v", err)
	}
	if req.Messages[1].Content != "Hello\nworld" {
		t.Errorf("array content = %q, want %q", req.Messages[1].Content, "Hello\nworld")
	}
	if req.Messages[2].Content != "72F, sunny" {
		t.Errorf("tool array content = %q, want %q", req.Messages[2].Content, "72F, sunny")
	}
	canon := req.ToInternal()
	if len(canon.Messages) != 3 {
		t.Fatalf("canonical messages = %d, want 3", len(canon.Messages))
	}
	if canon.Messages[0].Role != RoleUser || canon.Messages[0].Content != "Hello\nworld" {
		t.Errorf("canonical user = %+v, want content %q", canon.Messages[0], "Hello\nworld")
	}
	if canon.Messages[1].Role != RoleTool || canon.Messages[1].Content != "72F, sunny" {
		t.Errorf("canonical tool = %+v", canon.Messages[1])
	}
	// 字符串与空 content 形态不回归。
	var str CompletionRequest
	if err := json.Unmarshal([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"plain"}]}`), &str); err != nil {
		t.Fatalf("decode string content: %v", err)
	}
	if str.Messages[0].Content != "plain" {
		t.Errorf("string content = %q, want plain", str.Messages[0].Content)
	}
	var empty CompletionRequest
	if err := json.Unmarshal([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[]}]}`), &empty); err != nil {
		t.Fatalf("decode empty array content: %v", err)
	}
	if empty.Messages[0].Content != "" {
		t.Errorf("empty array content = %q, want empty", empty.Messages[0].Content)
	}
}

// OpenAI 客户端(如 pi/OpenAI SDK)对 system/user 消息发简写形式 {role, content} 且
// 省略 type、content 为字符串/数组时,解码必须归一化 content 并补全 type=message
// (原 bug:content 为字符串直接 400,且省略 type 的消息被静默丢弃)。
func TestResponsesRequestShorthandItemsDecode(t *testing.T) {
	wire := `{
		"model": "gpt-5",
		"stream": true,
		"input": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "plain string"},
			{"role": "user", "content": [{"type": "input_text", "text": "Hello"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Hi"}]},
			{"type": "function_call", "call_id": "fc_1", "name": "get_weather", "arguments": "{\"city\":\"SF\"}"},
			{"type": "function_call_output", "call_id": "fc_1", "output": "72F"}
		],
		"tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatalf("decode shorthand items: %v", err)
	}
	// 简写 system/user 补全 type=message,字符串 content 归一化为单块。
	if req.Input[0].Type != "message" || req.Input[0].Role != "system" {
		t.Errorf("system shorthand = %+v, want type=message role=system", req.Input[0])
	}
	if len(req.Input[0].Content) != 1 || req.Input[0].Content[0].Text != "You are helpful." {
		t.Errorf("system content = %+v", req.Input[0].Content)
	}
	if req.Input[1].Type != "message" || len(req.Input[1].Content) != 1 || req.Input[1].Content[0].Text != "plain string" {
		t.Errorf("user string shorthand = %+v", req.Input[1])
	}
	if req.Input[2].Type != "message" || len(req.Input[2].Content) != 1 || req.Input[2].Content[0].Text != "Hello" {
		t.Errorf("user array shorthand = %+v", req.Input[2])
	}
	canon := req.ToInternal()
	if canon.System != "You are helpful." {
		t.Errorf("canonical system = %q, want %q", canon.System, "You are helpful.")
	}
	// user×2 + assistant(text) + assistant(tool call) + tool,无 reasoning 时 tool call 不并入文本。
	if len(canon.Messages) != 5 {
		t.Fatalf("canonical messages = %d, want 5", len(canon.Messages))
	}
	if canon.Messages[0].Role != RoleUser || canon.Messages[0].Content != "plain string" {
		t.Errorf("canonical messages[0] = %+v", canon.Messages[0])
	}
	if canon.Messages[1].Role != RoleUser || canon.Messages[1].Content != "Hello" {
		t.Errorf("canonical messages[1] = %+v", canon.Messages[1])
	}
	if canon.Messages[2].Role != RoleAssistant || canon.Messages[2].Content != "Hi" {
		t.Errorf("canonical messages[2] = %+v", canon.Messages[2])
	}
	if canon.Messages[3].Role != RoleAssistant || len(canon.Messages[3].ToolCalls) != 1 || canon.Messages[3].ToolCalls[0].Name != "get_weather" {
		t.Errorf("canonical messages[3] = %+v", canon.Messages[3])
	}
	if canon.Messages[4].Role != RoleTool || canon.Messages[4].ToolCallID != "fc_1" || canon.Messages[4].Content != "72F" {
		t.Errorf("canonical messages[4] = %+v", canon.Messages[4])
	}
	if len(canon.Tools) != 1 || canon.Tools[0].Function.Name != "get_weather" {
		t.Errorf("canonical tools = %+v", canon.Tools)
	}
}

// 思考启用参数跨格式转发:canonical ReasoningEffort ↔ 各格式的思考参数形态。
// (anthropic thinking.budget_tokens / completion reasoning_effort / responses reasoning.effort)
func TestReasoningEffortCrossFormat(t *testing.T) {
	// canonical → 各格式:emit 思考启用。
	canon := &Request{Model: "m", ReasoningEffort: "high", Messages: []Message{{Role: RoleUser, Content: "hi"}}}

	comp := canon.ToCompletion()
	if comp.ReasoningEffort != "high" {
		t.Errorf("completion reasoning_effort = %q, want high", comp.ReasoningEffort)
	}
	if b := mustJSON(t, comp); !strings.Contains(b, `"reasoning_effort":"high"`) {
		t.Errorf("completion wire missing reasoning_effort: %s", b)
	}

	an := canon.ToAnthropic()
	if an.Thinking == nil || an.Thinking.Type != "enabled" || an.Thinking.BudgetTokens != effortToBudget("high") {
		t.Errorf("anthropic thinking = %+v, want enabled budget=%d", an.Thinking, effortToBudget("high"))
	}

	resp := canon.ToResponses()
	if resp.Reasoning == nil || resp.Reasoning.Effort != "high" {
		t.Errorf("responses reasoning = %+v, want effort=high", resp.Reasoning)
	}

	// 各格式 → canonical:捕获思考启用。
	if got := (&CompletionRequest{Model: "m", ReasoningEffort: "high"}).ToInternal(); got.ReasoningEffort != "high" {
		t.Errorf("completion → canonical effort = %q, want high", got.ReasoningEffort)
	}
	if got := (&ResponsesRequest{Model: "m", Reasoning: &ResponsesReasoning{Effort: "high"}}).ToInternal(); got.ReasoningEffort != "high" {
		t.Errorf("responses → canonical effort = %q, want high", got.ReasoningEffort)
	}
	anReq := &AnthropicRequest{Model: "m", Thinking: &AnthropicThinking{Type: "enabled", BudgetTokens: 1024}}
	if got := anReq.ToInternal(); got.ReasoningEffort != budgetToEffort(1024) {
		t.Errorf("anthropic → canonical effort = %q, want %q", got.ReasoningEffort, budgetToEffort(1024))
	}
	// disabled / 缺省 → 不启用。
	if got := (&AnthropicRequest{Model: "m", Thinking: &AnthropicThinking{Type: "disabled"}}).ToInternal(); got.ReasoningEffort != "" {
		t.Errorf("disabled thinking → canonical effort = %q, want empty", got.ReasoningEffort)
	}
	// 跨格式:anthropic thinking(1024) → completion reasoning_effort。
	if comp2 := anReq.ToInternal().ToCompletion(); comp2.ReasoningEffort != budgetToEffort(1024) {
		t.Errorf("anthropic thinking → completion effort = %q, want %q", comp2.ReasoningEffort, budgetToEffort(1024))
	}
	// 跨格式:responses reasoning → anthropic thinking。
	an2 := (&ResponsesRequest{Model: "m", Reasoning: &ResponsesReasoning{Effort: "medium"}}).ToInternal().ToAnthropic()
	if an2.Thinking == nil || an2.Thinking.Type != "enabled" || an2.Thinking.BudgetTokens != effortToBudget("medium") {
		t.Errorf("responses reasoning → anthropic thinking = %+v", an2.Thinking)
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

// codex(Responses API 客户端)把技能/AGENTS 指令作为 role=developer 的 input item
// 发送。此前 ToInternal 未归一化 developer,导致该角色原样透传到 chat.completions
// 上游,被 opencode.ai 等严格校验上游以 400 拒绝
// ("unknown variant `developer`")。developer 应与 system 一样并入顶层 System。
func TestResponsesDeveloperRoleMergedIntoSystem(t *testing.T) {
	wire := &ResponsesRequest{
		Model:        "opencode-go@deepseek-v4-flash",
		Instructions: "You are Codex, a coding agent.",
		Input: []ResponsesItem{
			{Type: "message", Role: "developer", Content: []ResponsesContent{{Type: "input_text", Text: "<skills_instructions>use tools</skills_instructions>"}}},
			{Type: "message", Role: "user", Content: []ResponsesContent{{Type: "input_text", Text: "find files"}}},
		},
	}
	canonical := wire.ToInternal()
	if canonical.System != "You are Codex, a coding agent.\n\n<skills_instructions>use tools</skills_instructions>" {
		t.Fatalf("system = %q", canonical.System)
	}
	for _, m := range canonical.Messages {
		if m.Role == Role("developer") {
			t.Errorf("canonical still carries developer role: %+v", m)
		}
	}
	if len(canonical.Messages) != 1 || canonical.Messages[0].Role != RoleUser {
		t.Errorf("messages = %+v, want single user message", canonical.Messages)
	}
	// 转成 chat.completions 时不得再出现 developer 角色,且系统提示合并为一条 system。
	comp := canonical.ToCompletion()
	for _, m := range comp.Messages {
		if m.Role == "developer" {
			t.Errorf("completion wire still carries developer role: %+v", m)
		}
	}
	if comp.Messages[0].Role != "system" || comp.Messages[0].Content != canonical.System {
		t.Errorf("completion system message = %+v, want merged system", comp.Messages[0])
	}
	if len(comp.Messages) != 2 || comp.Messages[1].Role != "user" {
		t.Errorf("completion messages = %+v, want system + user", comp.Messages)
	}
	// 转成 anthropic 也不得出现 developer(Anthropic 无此角色)。
	ant := canonical.ToAnthropic()
	if string(ant.System) != canonical.System {
		t.Errorf("anthropic system = %q", ant.System)
	}
	for _, m := range ant.Messages {
		if m.Role == "developer" {
			t.Errorf("anthropic wire still carries developer role: %+v", m)
		}
	}
}

// chat.completions 客户端也可能发送 role=developer 消息,同样应并入 System。
func TestCompletionDeveloperRoleMergedIntoSystem(t *testing.T) {
	wire := &CompletionRequest{
		Model: "gpt-4o",
		Messages: []CompletionMessage{
			{Role: "developer", Content: "Be brief."},
			{Role: "user", Content: "Weather?"},
		},
	}
	canonical := wire.ToInternal()
	if canonical.System != "Be brief." {
		t.Fatalf("system = %q", canonical.System)
	}
	if len(canonical.Messages) != 1 || canonical.Messages[0].Role != RoleUser {
		t.Errorf("messages = %+v, want single user message", canonical.Messages)
	}
	back := canonical.ToCompletion()
	if mustJSON(t, back) != mustJSON(t, &CompletionRequest{
		Model: "gpt-4o",
		Messages: []CompletionMessage{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: "Weather?"},
		},
	}) {
		t.Fatalf("developer should normalize to system:\n got  %s", mustJSON(t, back))
	}
}

// codex(Responses API 客户端)会发送非 function 类型的工具(如内置的
// web_search_preview,无 name 字段)。此前转换无条件为每个工具创建 FunctionTool,
// 把这种工具转成空名 function(completion: {"name":"","parameters":null}),
// 被 opencode.ai 等严格上游以 400 拒绝("tools[i].function.name: empty string")。
// 非 function 工具应跳过,空名 function 兜底过滤。
func TestResponsesNonFunctionToolSkipped(t *testing.T) {
	wire := &ResponsesRequest{
		Model: "m",
		Input: []ResponsesItem{{Type: "message", Role: "user", Content: []ResponsesContent{{Type: "input_text", Text: "hi"}}}},
		Tools: []ResponsesTool{
			{Type: "function", Name: "shell_command", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Type: "web_search_preview"}, // 非 function 内置工具:跳过
			{Type: "", Name: "plan"},     // 空 Type 但带名:视为 function 保留
		},
	}
	canonical := wire.ToInternal()
	if len(canonical.Tools) != 2 {
		t.Fatalf("internal tools = %d, want 2 (non-function skipped)", len(canonical.Tools))
	}
	if canonical.Tools[0].Function.Name != "shell_command" || canonical.Tools[1].Function.Name != "plan" {
		t.Errorf("internal tools = %+v", canonical.Tools)
	}
	comp := canonical.ToCompletion()
	if len(comp.Tools) != 2 {
		t.Fatalf("completion tools = %d, want 2", len(comp.Tools))
	}
	for i, c := range comp.Tools {
		if c.Type != "function" || c.Function.Name == "" {
			t.Errorf("completion tools[%d] = %+v, want non-empty function name", i, c)
		}
	}
	// anthropic 输出同样无空名工具。
	ant := canonical.ToAnthropic()
	if len(ant.Tools) != 2 {
		t.Fatalf("anthropic tools = %d, want 2", len(ant.Tools))
	}
	for i, c := range ant.Tools {
		if c.Name == "" {
			t.Errorf("anthropic tools[%d] has empty name: %+v", i, c)
		}
	}
}

// 兜底:即使内部规范类型中混入空名工具(其它来源),输出转换也不应把它发给上游。
func TestEmptyNameToolFilteredOnOutput(t *testing.T) {
	canonical := &Request{
		Model: "m",
		Tools: []Tool{{Function: &FunctionTool{Name: "ok"}}, {Function: &FunctionTool{Name: ""}}},
	}
	if got := canonical.ToCompletion(); len(got.Tools) != 1 || got.Tools[0].Function.Name != "ok" {
		t.Errorf("completion output = %+v, want only ok", got.Tools)
	}
	if got := canonical.ToAnthropic(); len(got.Tools) != 1 || got.Tools[0].Name != "ok" {
		t.Errorf("anthropic output = %+v, want only ok", got.Tools)
	}
}

// codex(responses 客户端)回传历史 reasoning item(deepseek thinking 要求原样回传)。
// 此前 ToInternal 忽略 reasoning item,转 completion 时丢 reasoning_content,
// 被 opencode.ai 等上游以 400 拒绝("The 'reasoning_content' in the thinking mode
// must be passed back to the API.")。reasoning 应在 canonical 层保留并双向透传;
// 且 reasoning 与紧随其后的 assistant 消息应合并为同一条 assistant(思考 + 文本配对)。
func TestResponsesReasoningRoundTrip(t *testing.T) {
	// 请求侧:codex 回传 reasoning input item + 后续 assistant 消息(真实场景)。
	wire := &ResponsesRequest{
		Model: "m",
		Input: []ResponsesItem{
			{Type: "message", Role: "user", Content: []ResponsesContent{{Type: "input_text", Text: "find files"}}},
			{Type: "reasoning", Summary: []ResponsesContent{{Type: "summary_text", Text: "I need to list the directory first."}}},
			{Type: "message", Role: "assistant", Content: []ResponsesContent{{Type: "output_text", Text: "I will use shell."}}},
		},
	}
	canonical := wire.ToInternal()
	// reasoning + 后续 assistant 消息合并为一条 assistant(思考 + 文本)。
	if len(canonical.Messages) != 2 {
		t.Fatalf("canonical messages = %d, want 2 (user + merged assistant), got %+v", len(canonical.Messages), canonical.Messages)
	}
	merged := canonical.Messages[1]
	if merged.Role != RoleAssistant || merged.Reasoning != "I need to list the directory first." || merged.Content != "I will use shell." {
		t.Errorf("merged assistant = %+v, want reasoning+content on same message", merged)
	}
	// 转 completion 时该 assistant 消息同时带 reasoning_content 与 content。
	comp := canonical.ToCompletion()
	if len(comp.Messages) != 2 {
		t.Fatalf("completion messages = %d, want 2, got %+v", len(comp.Messages), comp.Messages)
	}
	cm := comp.Messages[1]
	if cm.Role != "assistant" || cm.ReasoningContent == nil || *cm.ReasoningContent != "I need to list the directory first." || cm.Content != "I will use shell." {
		t.Errorf("completion assistant = %+v, want reasoning_content + content on same message", cm)
	}

	// 响应侧:completion 上游带 reasoning_content → canonical → responses reasoning item。
	compResp := &CompletionRequest{
		Model: "m",
		Messages: []CompletionMessage{
			{Role: "assistant", Content: "I will use shell.", ReasoningContent: strPtr("I need to list the directory first.")},
		},
	}
	back := compResp.ToInternal().ToResponses()
	var sawReasoningItem bool
	for _, it := range back.Input {
		if it.Type == "reasoning" && len(it.Summary) == 1 && it.Summary[0].Text == "I need to list the directory first." {
			sawReasoningItem = true
		}
	}
	if !sawReasoningItem {
		t.Fatalf("responses lost reasoning item: %+v", back.Input)
	}
}

// codex 在 reasoning 后直接跟 function_call(无 assistant message):思考应并入
// 同一 assistant 的工具调用,避免纯思考 assistant 被 thinking 上游以 400 拒绝。
func TestResponsesReasoningThenToolCallMerged(t *testing.T) {
	wire := &ResponsesRequest{
		Model: "m",
		Input: []ResponsesItem{
			{Type: "message", Role: "user", Content: []ResponsesContent{{Type: "input_text", Text: "list files"}}},
			{Type: "reasoning", Summary: []ResponsesContent{{Type: "summary_text", Text: "I need to list the directory."}}},
			{Type: "function_call", CallID: "fc_1", Name: "shell_command", Arguments: `{"command":"ls"}`},
			{Type: "function_call", CallID: "fc_2", Name: "shell_command", Arguments: `{"command":"git status"}`},
		},
	}
	canonical := wire.ToInternal()
	// user + 一条合并后的 assistant(思考 + 两个工具调用)。
	if len(canonical.Messages) != 2 {
		t.Fatalf("canonical messages = %d, want 2 (user + merged assistant), got %+v", len(canonical.Messages), canonical.Messages)
	}
	merged := canonical.Messages[1]
	if merged.Role != RoleAssistant || merged.Reasoning != "I need to list the directory." || len(merged.ToolCalls) != 2 {
		t.Errorf("merged assistant = %+v, want reasoning + 2 tool_calls on same message", merged)
	}
	// 转 completion:同一条 assistant 带 reasoning_content + tool_calls。
	comp := canonical.ToCompletion()
	if len(comp.Messages) != 2 {
		t.Fatalf("completion messages = %d, want 2, got %+v", len(comp.Messages), comp.Messages)
	}
	cm := comp.Messages[1]
	if cm.Role != "assistant" || cm.ReasoningContent == nil || *cm.ReasoningContent != "I need to list the directory." || len(cm.ToolCalls) != 2 {
		t.Errorf("completion assistant = %+v, want reasoning_content + tool_calls on same message", cm)
	}
}

// codex 完整回合:reasoning → assistant(content) → function_call ×N,全部并入一条
// assistant(reasoning + content + tool_calls 配对)。这是真实日志里最常见的失败形态。
func TestResponsesReasoningContentAndToolCallsMerged(t *testing.T) {
	wire := &ResponsesRequest{
		Model: "m",
		Input: []ResponsesItem{
			{Type: "message", Role: "user", Content: []ResponsesContent{{Type: "input_text", Text: "hi"}}},
			{Type: "reasoning", Summary: []ResponsesContent{{Type: "summary_text", Text: "The README shows mojibake."}}},
			{Type: "message", Role: "assistant", Content: []ResponsesContent{{Type: "output_text", Text: "Let me check CLAUDE.md."}}},
			{Type: "function_call", CallID: "c1", Name: "shell_command", Arguments: `{"command":"a"}`},
			{Type: "function_call", CallID: "c2", Name: "shell_command", Arguments: `{"command":"b"}`},
			{Type: "function_call", CallID: "c3", Name: "shell_command", Arguments: `{"command":"c"}`},
		},
	}
	canonical := wire.ToInternal()
	// user + 一条完整合并的 assistant。
	if len(canonical.Messages) != 2 {
		t.Fatalf("canonical messages = %d, want 2, got %+v", len(canonical.Messages), canonical.Messages)
	}
	merged := canonical.Messages[1]
	if merged.Role != RoleAssistant || merged.Reasoning != "The README shows mojibake." ||
		merged.Content != "Let me check CLAUDE.md." || len(merged.ToolCalls) != 3 {
		t.Errorf("merged assistant = %+v, want reasoning + content + 3 tool_calls on same message", merged)
	}
	comp := canonical.ToCompletion()
	if len(comp.Messages) != 2 {
		t.Fatalf("completion messages = %d, want 2, got %+v", len(comp.Messages), comp.Messages)
	}
	cm := comp.Messages[1]
	if cm.Role != "assistant" || cm.ReasoningContent == nil || *cm.ReasoningContent != "The README shows mojibake." ||
		cm.Content != "Let me check CLAUDE.md." || len(cm.ToolCalls) != 3 {
		t.Errorf("completion assistant = %+v, want reasoning_content + content + 3 tool_calls on same message", cm)
	}
}

// completion 请求里的 reasoning_content 应解析进 canonical,再原样输出。
func TestCompletionReasoningContentParsed(t *testing.T) {
	wire := &CompletionRequest{
		Model: "m",
		Messages: []CompletionMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "ok", ReasoningContent: strPtr("think first")},
		},
	}
	canonical := wire.ToInternal()
	if canonical.Messages[1].Reasoning != "think first" {
		t.Errorf("reasoning_content not parsed: %+v", canonical.Messages[1])
	}
	out := canonical.ToCompletion()
	if out.Messages[1].ReasoningContent == nil || *out.Messages[1].ReasoningContent != "think first" {
		t.Errorf("completion round-trip lost reasoning_content: %+v", out.Messages[1])
	}
}

// deepseek thinking 模式:带 tools 的请求中,缺少思考块的 assistant 消息也必须携带
// reasoning_content 字段(空串),否则上游 400("The `reasoning_content` in the thinking
// mode must be passed back to the API.")——claude-cli 回传历史时可能对某条 assistant
// 消息不带 thinking 块,网关需补字段保证存在。
func TestCompletionFillsReasoningContentForThinkingConversation(t *testing.T) {
	canonical := &Request{
		Model: "m",
		Tools: []Tool{{Function: &FunctionTool{Name: "shell"}}},
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			// 思考轮:带 reasoning。
			{Role: RoleAssistant, Reasoning: "think first", Content: "Let me check.", ToolCalls: []ToolCall{{ID: "c1", Name: "shell", Arguments: json.RawMessage(`{"command":"ls"}`)}}},
			{Role: RoleTool, ToolCallID: "c1", Content: "a.txt"},
			// 无思考块的历史 assistant(客户端缺失),字段必须补上(空串)。
			{Role: RoleAssistant, Content: "I answered without thinking."},
		},
	}
	comp := canonical.ToCompletion()
	if len(comp.Messages) != 4 {
		t.Fatalf("completion messages = %d, want 4, got %+v", len(comp.Messages), comp.Messages)
	}
	last := comp.Messages[3]
	if last.Role != "assistant" || last.ReasoningContent == nil || *last.ReasoningContent != "" {
		t.Errorf("assistant without thinking should carry empty reasoning_content, got %+v", last)
	}
	b, err := json.Marshal(comp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"reasoning_content":""`) {
		t.Errorf("wire should contain \"reasoning_content\":\"\" for the no-thinking assistant, got %s", b)
	}
}

// 无 thinking 的普通会话(带 tools)不补 reasoning_content,避免污染非 thinking 上游。
func TestCompletionNoFillWithoutThinking(t *testing.T) {
	canonical := &Request{
		Model: "m",
		Tools: []Tool{{Function: &FunctionTool{Name: "shell"}}},
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, Content: "plain answer"},
		},
	}
	comp := canonical.ToCompletion()
	last := comp.Messages[1]
	if last.Role != "assistant" || last.ReasoningContent != nil {
		t.Errorf("non-thinking conversation should not add reasoning_content, got %+v", last)
	}
}

// claude-cli 等客户端回传 Anthropic thinking 块(deepseek thinking 模式的
// reasoning_content):应进入 canonical.Reasoning,转 completion 时输出
// reasoning_content,否则 thinking 上游以 400 拒绝。
func TestAnthropicThinkingToCompletion(t *testing.T) {
	wire := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: AnthropicContent{
				{Type: "thinking", Thinking: "I should use shell."},
				{Type: "text", Text: "Let me check."},
			}},
		},
	}
	canonical := wire.ToInternal()
	if len(canonical.Messages) != 2 {
		t.Fatalf("canonical messages = %d, want 2, got %+v", len(canonical.Messages), canonical.Messages)
	}
	m := canonical.Messages[1]
	if m.Role != RoleAssistant || m.Reasoning != "I should use shell." || m.Content != "Let me check." {
		t.Errorf("canonical assistant = %+v, want reasoning + content on same message", m)
	}
	comp := canonical.ToCompletion()
	if len(comp.Messages) != 2 {
		t.Fatalf("completion messages = %d, want 2, got %+v", len(comp.Messages), comp.Messages)
	}
	cm := comp.Messages[1]
	if cm.Role != "assistant" || cm.ReasoningContent == nil || *cm.ReasoningContent != "I should use shell." || cm.Content != "Let me check." {
		t.Errorf("completion assistant = %+v, want reasoning_content + content on same message", cm)
	}
}

// Anthropic thinking ??? tool_use ???:?????? assistant(reasoning + tool_calls),
// ? completion ?????? reasoning_content + tool_calls(?? 502 ?????)?
func TestAnthropicThinkingThenToolCallMerged(t *testing.T) {
	wire := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "list files"}}},
			{Role: "assistant", Content: AnthropicContent{
				{Type: "thinking", Thinking: "I need to list the directory."},
				{Type: "tool_use", ID: "toolu_01", Name: "shell_command", Input: json.RawMessage(`{"command":"ls"}`)},
				{Type: "tool_use", ID: "toolu_02", Name: "shell_command", Input: json.RawMessage(`{"command":"git status"}`)},
			}},
			{Role: "user", Content: AnthropicContent{
				{Type: "tool_result", ToolUseID: "toolu_01", Content: AnthropicContent{{Type: "text", Text: "a.txt"}}},
				{Type: "tool_result", ToolUseID: "toolu_02", Content: AnthropicContent{{Type: "text", Text: "clean"}}},
			}},
		},
	}
	canonical := wire.ToInternal()
	// user + ???? assistant(reasoning + 2 tool_calls) + 2 ? tool ???
	if len(canonical.Messages) != 4 {
		t.Fatalf("canonical messages = %d, want 4, got %+v", len(canonical.Messages), canonical.Messages)
	}
	merged := canonical.Messages[1]
	if merged.Role != RoleAssistant || merged.Reasoning != "I need to list the directory." || len(merged.ToolCalls) != 2 {
		t.Errorf("merged assistant = %+v, want reasoning + 2 tool_calls on same message", merged)
	}
	comp := canonical.ToCompletion()
	cm := comp.Messages[1]
	if cm.Role != "assistant" || cm.ReasoningContent == nil || *cm.ReasoningContent != "I need to list the directory." || len(cm.ToolCalls) != 2 {
		t.Errorf("completion assistant = %+v, want reasoning_content + 2 tool_calls on same message", cm)
	}
}

// ??????? kind=responses:Anthropic thinking ? canonical ? responses ??? reasoning item?
func TestAnthropicThinkingToResponses(t *testing.T) {
	wire := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: AnthropicContent{
				{Type: "thinking", Thinking: "The README shows mojibake."},
				{Type: "text", Text: "Let me check CLAUDE.md."},
			}},
		},
	}
	back := wire.ToInternal().ToResponses()
	var sawReasoning, sawMessage bool
	for _, it := range back.Input {
		if it.Type == "reasoning" && len(it.Summary) == 1 && it.Summary[0].Text == "The README shows mojibake." {
			sawReasoning = true
		}
		if it.Type == "message" && it.Role == "assistant" && len(it.Content) == 1 && it.Content[0].Text == "Let me check CLAUDE.md." {
			sawMessage = true
		}
	}
	if !sawReasoning || !sawMessage {
		t.Fatalf("responses lost reasoning/message item: %+v", back.Input)
	}
}

// ???(? content?? tool_calls)? assistant ??? repairOrphanToolUse ???
func TestAnthropicThinkingOnlyPreserved(t *testing.T) {
	wire := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicContent{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: AnthropicContent{{Type: "thinking", Thinking: "deep thought"}}},
		},
	}
	canonical := wire.ToInternal()
	if len(canonical.Messages) != 2 {
		t.Fatalf("canonical messages = %d, want 2 (pure-thinking assistant preserved), got %+v", len(canonical.Messages), canonical.Messages)
	}
	if canonical.Messages[1].Reasoning != "deep thought" {
		t.Errorf("assistant reasoning = %q, want %q", canonical.Messages[1].Reasoning, "deep thought")
	}
}

// canonical Reasoning -> ToAnthropic ?? thinking ? -> ? ToInternal ??(????)?
func TestAnthropicThinkingRoundTrip(t *testing.T) {
	req := &Request{
		Model: "m",
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, Reasoning: "think first", Content: "ok"},
		},
	}
	wire := req.ToAnthropic()
	if len(wire.Messages) != 2 || len(wire.Messages[1].Content) != 2 {
		t.Fatalf("anthropic messages = %+v, want [thinking, text] blocks", wire.Messages)
	}
	blocks := wire.Messages[1].Content
	if blocks[0].Type != "thinking" || blocks[0].Thinking != "think first" ||
		blocks[1].Type != "text" || blocks[1].Text != "ok" {
		t.Errorf("anthropic assistant blocks = %+v, want thinking then text", blocks)
	}
	back := wire.ToInternal()
	m := back.Messages[1]
	if m.Role != RoleAssistant || m.Reasoning != "think first" || m.Content != "ok" {
		t.Errorf("round-trip assistant = %+v, want reasoning + content preserved", m)
	}
}
