package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ReadSSE 应正确拆分事件:event 名、多行 data、末尾 [DONE] 与缺末尾空行的最后一条事件。
func TestReadSSE(t *testing.T) {
	input := "" +
		"event: message_start\r\n" +
		"data: {\"type\":\"message_start\"}\r\n" +
		"\r\n" +
		"event: content_block_delta\n" +
		"data: {\"a\":1}\n" +
		"data: {\"b\":2}\n" +
		"\n" +
		"data: [DONE]\n" +
		"\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n"
	var got []SSEBlock
	if err := ReadSSE(strings.NewReader(input), func(b SSEBlock) error {
		got = append(got, b)
		return nil
	}); err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("blocks = %d, want 4: %+v", len(got), got)
	}
	if got[0].Event != "message_start" || string(got[0].Data) != `{"type":"message_start"}` {
		t.Errorf("block[0] = %+v", got[0])
	}
	if got[1].Event != "content_block_delta" || string(got[1].Data) != "{\"a\":1}\n{\"b\":2}" {
		t.Errorf("block[1] = %+v", got[1])
	}
	if got[2].Event != "" || string(got[2].Data) != "[DONE]" {
		t.Errorf("block[2] = %+v", got[2])
	}
	if got[3].Event != "message_stop" || string(got[3].Data) != `{"type":"message_stop"}` {
		t.Errorf("block[3] = %+v", got[3])
	}
}

// 连续空行与纯注释行不应产生空事件。
func TestReadSSESkipsEmpty(t *testing.T) {
	input := "event: ping\ndata: {}\n\n\n\n: comment\n\ndata: [DONE]\n\n"
	var got []SSEBlock
	if err := ReadSSE(strings.NewReader(input), func(b SSEBlock) error {
		got = append(got, b)
		return nil
	}); err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("blocks = %d, want 2: %+v", len(got), got)
	}
}

func collectDecoded(t *testing.T, format string, input string) []StreamEvent {
	t.Helper()
	dec, ok := DecoderFor(format)
	if !ok {
		t.Fatalf("no decoder for %q", format)
	}
	var out []StreamEvent
	if err := dec(strings.NewReader(input), func(ev StreamEvent) error {
		out = append(out, ev)
		return nil
	}); err != nil {
		t.Fatalf("decode %s: %v", format, err)
	}
	return out
}

// OpenAI Chat SSE → 规范化:文本 + 工具调用 + 结束。
func TestDecodeCompletionSSE(t *testing.T) {
	input := "data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"SF\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	evs := collectDecoded(t, FormatCompletion, input)

	want := []StreamEvent{
		{Type: StreamMessageStart, ID: "c1", Model: "gpt-4o"},
		{Type: StreamContentStart, Index: 0, BlockType: "text"},
		{Type: StreamContentDelta, Index: 0, DeltaType: "text", DeltaText: "hi"},
		{Type: StreamContentStop, Index: 0},
		{Type: StreamContentStart, Index: 1, BlockType: "tool_use", BlockID: "call_1", BlockName: "get_weather"},
		{Type: StreamContentDelta, Index: 1, DeltaType: "input_json", PartialJSON: `{"city":"SF"}`},
		{Type: StreamContentStop, Index: 1},
		{Type: StreamMessageDelta, StopReason: "tool_use", Usage: &Usage{InputTokens: 8, OutputTokens: 4}},
		{Type: StreamMessageStop},
	}
	if len(evs) != len(want) {
		t.Fatalf("events = %d, want %d:\n%+v", len(evs), len(want), evs)
	}
	for i, w := range want {
		if sig(evs[i]) != sig(w) {
			t.Errorf("event[%d] = %+v, want %+v", i, evs[i], w)
		}
	}
}

// 上游 completion 流被截断:工具调用参数中途 EOF,且未收到 finish_reason/[DONE]。
// 解码必须以 StreamError 收尾(并关闭未结束的块),不能假装正常 message_stop——
// 否则客户端拿到未闭合的 tool_use 块会把残缺工具调用当成功消息,对话静默提前中止。
func TestDecodeCompletionSSETruncatedToolCall(t *testing.T) {
	input := "data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"replace_all\\\":false,\\\"file_path\\\":\\\"G:\\\\x\"}}]}}]}\n\n"
	// 无 [DONE]、无 finish_reason —— 流在此 EOF。
	evs := collectDecoded(t, FormatCompletion, input)

	if len(evs) == 0 || evs[len(evs)-1].Type != StreamError {
		t.Fatalf("last event = %+v, want StreamError (truncated stream); events:\n%+v", lastOrNil(evs), evs)
	}
	if lastOrNil(evs).Error == "" {
		t.Errorf("StreamError should carry a message, got %+v", lastOrNil(evs))
	}
	// 不能出现正常结束的 message_stop。
	for _, e := range evs {
		if e.Type == StreamMessageStop {
			t.Errorf("truncated stream must not emit StreamMessageStop; got %+v", evs)
		}
	}
	// 已打开的 tool_use 块应被关闭(content_block_stop),结构完整。
	stopCount := 0
	for _, e := range evs {
		if e.Type == StreamContentStop {
			stopCount++
		}
	}
	if stopCount < 1 {
		t.Errorf("truncated stream should close open blocks, got events:\n%+v", evs)
	}
	// 端到端:经 anthropic 编码器(Claude Code 客户端格式)必须输出 error 事件,
	// 而非静默 message_stop——否则客户端把残缺工具调用当成功消息,对话提前中止。
	var sb strings.Builder
	for _, e := range evs {
		if err := EncodeAnthropicSSE(&sb, e); err != nil {
			t.Fatalf("encode anthropic: %v", err)
		}
	}
	out := sb.String()
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "upstream stream ended") {
		t.Errorf("converted anthropic output should surface truncation error, got:\n%s", out)
	}
	if strings.Contains(out, "message_stop") {
		t.Errorf("converted anthropic output must not emit message_stop for truncated stream:\n%s", out)
	}
}

func lastOrNil(evs []StreamEvent) StreamEvent {
	if len(evs) == 0 {
		return StreamEvent{}
	}
	return evs[len(evs)-1]
}

// 工具参数先于 id/name 到达、并行工具交错、OpenRouter 多发 finish_reason 的场景。
func TestDecodeCompletionSSEToolEdgeCases(t *testing.T) {
	input := "data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_0\",\"function\":{\"name\":\"t0\",\"arguments\":\"1}\"}},{\"index\":1,\"id\":\"call_1\",\"function\":{\"name\":\"t1\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"b\\\":2}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	evs := collectDecoded(t, FormatCompletion, input)

	// 只断言关键语义:两个工具块各收到完整参数、只发一条 message_delta、usage 取末尾完整值。
	var toolStarts, argDeltas, deltaCount int
	var deltaStop string
	var finalUsage *Usage
	for _, ev := range evs {
		switch ev.Type {
		case StreamContentStart:
			if ev.BlockType == "tool_use" {
				toolStarts++
			}
		case StreamContentDelta:
			if ev.DeltaType == "input_json" {
				argDeltas++
			}
		case StreamMessageDelta:
			deltaCount++
			deltaStop = ev.StopReason
			finalUsage = ev.Usage
		}
	}
	if toolStarts != 2 {
		t.Errorf("tool starts = %d, want 2", toolStarts)
	}
	if argDeltas != 3 {
		t.Errorf("arg deltas = %d, want 3", argDeltas)
	}
	if deltaCount != 1 || deltaStop != "tool_use" {
		t.Errorf("message_delta count = %d stop=%q, want 1 tool_use", deltaCount, deltaStop)
	}
	if finalUsage == nil || finalUsage.InputTokens != 10 || finalUsage.OutputTokens != 5 {
		t.Errorf("final usage = %+v, want {10,5}", finalUsage)
	}
}

// Anthropic SSE → 规范化(近似一一对应)。
func TestDecodeAnthropicSSE(t *testing.T) {
	input := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"m\",\"usage\":{\"input_tokens\":8,\"output_tokens\":4}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	evs := collectDecoded(t, FormatAnthropic, input)
	want := []StreamEvent{
		{Type: StreamMessageStart, ID: "m1", Model: "m", Usage: &Usage{InputTokens: 8, OutputTokens: 4}},
		{Type: StreamContentDelta, Index: 0, DeltaType: "text", DeltaText: "hi"},
		{Type: StreamMessageStop},
	}
	if len(evs) != len(want) {
		t.Fatalf("events = %d, want %d: %+v", len(evs), len(want), evs)
	}
	for i, w := range want {
		if sig(evs[i]) != sig(w) {
			t.Errorf("event[%d] = %+v, want %+v", i, evs[i], w)
		}
	}
}

// OpenAI Chat SSE → 规范化 → Anthropic SSE → 规范化:语义保持一致(跨格式往返)。
func TestCompletionToAnthropicRoundTrip(t *testing.T) {
	openai := "data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"SF\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"

	decC, _ := DecoderFor(FormatCompletion)
	var canonical []StreamEvent
	if err := decC(strings.NewReader(openai), func(ev StreamEvent) error {
		canonical = append(canonical, ev)
		return nil
	}); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	// 服务器在规范化层做模型回填。
	for i := range canonical {
		if canonical[i].Type == StreamMessageStart {
			canonical[i].Model = "oa@gpt-4o"
		}
	}

	encA, _ := EncoderFor(FormatAnthropic)
	var buf bytes.Buffer
	for _, ev := range canonical {
		if err := encA(&buf, ev); err != nil {
			t.Fatalf("encode anthropic: %v", err)
		}
	}
	if !strings.Contains(buf.String(), "event: content_block_delta") ||
		!strings.Contains(buf.String(), `"type":"text_delta"`) {
		t.Fatalf("anthropic SSE missing delta:\n%s", buf.String())
	}

	roundtrip := collectDecoded(t, FormatAnthropic, buf.String())
	var texts, argParts []string
	var model string
	var finalUsage *Usage
	var deltaCount int
	for _, ev := range roundtrip {
		switch ev.Type {
		case StreamMessageStart:
			model = ev.Model
		case StreamContentDelta:
			if ev.DeltaType == "text" {
				texts = append(texts, ev.DeltaText)
			} else if ev.DeltaType == "input_json" {
				argParts = append(argParts, ev.PartialJSON)
			}
		case StreamMessageDelta:
			deltaCount++
			finalUsage = ev.Usage
		}
	}
	if model != "oa@gpt-4o" {
		t.Errorf("backfilled model = %q", model)
	}
	if len(texts) != 1 || texts[0] != "hi" {
		t.Errorf("texts = %v", texts)
	}
	if len(argParts) != 1 || argParts[0] != `{"city":"SF"}` {
		t.Errorf("tool args = %v", argParts)
	}
	if deltaCount != 1 || finalUsage == nil || finalUsage.InputTokens != 8 || finalUsage.OutputTokens != 4 {
		t.Errorf("deltaCount=%d usage=%+v, want 1 {8,4}", deltaCount, finalUsage)
	}
}

// Responses SSE → 规范化 → Responses SSE → 规范化:语义保持一致。
func TestResponsesRoundTrip(t *testing.T) {
	responses := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\",\"status\":\"in_progress\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"completed\",\"content\":[]}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":8,\"output_tokens\":4,\"total_tokens\":12}}}\n\n"

	decR, _ := DecoderFor(FormatResponses)
	var canonical []StreamEvent
	if err := decR(strings.NewReader(responses), func(ev StreamEvent) error {
		canonical = append(canonical, ev)
		return nil
	}); err != nil {
		t.Fatalf("decode responses: %v", err)
	}
	if len(canonical) != 6 {
		t.Fatalf("canonical events = %d, want 6:\n%+v", len(canonical), canonical)
	}
	if canonical[0].Type != StreamMessageStart || canonical[0].ID != "resp_1" || canonical[0].Model != "gpt-5" {
		t.Errorf("start = %+v", canonical[0])
	}
	if canonical[2].Type != StreamContentDelta || canonical[2].DeltaType != "text" || canonical[2].DeltaText != "hi" {
		t.Errorf("delta = %+v", canonical[2])
	}
	if canonical[4].Type != StreamMessageDelta || canonical[4].StopReason != "end_turn" {
		t.Errorf("message_delta = %+v", canonical[4])
	}
	if canonical[5].Type != StreamMessageStop {
		t.Errorf("end = %+v", canonical[5])
	}

	encR, _ := EncoderFor(FormatResponses)
	var buf bytes.Buffer
	for _, ev := range canonical {
		if err := encR(&buf, ev); err != nil {
			t.Fatalf("encode responses: %v", err)
		}
	}
	if !strings.Contains(buf.String(), "response.created") || !strings.Contains(buf.String(), "response.output_text.delta") {
		t.Fatalf("responses SSE missing events:\n%s", buf.String())
	}

	roundtrip := collectDecoded(t, FormatResponses, buf.String())
	var texts []string
	var model string
	var finalUsage *Usage
	for _, ev := range roundtrip {
		switch ev.Type {
		case StreamMessageStart:
			model = ev.Model
		case StreamContentDelta:
			if ev.DeltaType == "text" {
				texts = append(texts, ev.DeltaText)
			}
		case StreamMessageDelta:
			finalUsage = ev.Usage
		}
	}
	if model != "gpt-5" {
		t.Errorf("model = %q", model)
	}
	if len(texts) != 1 || texts[0] != "hi" {
		t.Errorf("texts = %v", texts)
	}
	if finalUsage == nil || finalUsage.InputTokens != 8 || finalUsage.OutputTokens != 4 {
		t.Errorf("usage = %+v, want {8,4}", finalUsage)
	}
}

// sig 生成规范化事件的语义签名(用于比较;Usage 仅在非零时计入)。
func sig(ev StreamEvent) string {
	var u string
	if ev.Usage != nil && (ev.Usage.InputTokens != 0 || ev.Usage.OutputTokens != 0) {
		u = strconv.Itoa(ev.Usage.InputTokens) + "/" + strconv.Itoa(ev.Usage.OutputTokens)
	}
	return string(ev.Type) + "|" + ev.Model + "|" + strconv.Itoa(ev.Index) + "|" + ev.BlockType + "|" +
		ev.BlockID + "|" + ev.BlockName + "|" + ev.DeltaType + "|" + ev.DeltaText + "|" +
		ev.PartialJSON + "|" + ev.StopReason + "|" + u + "|" + ev.Error
}

// AnthropicProvider.Stream 应转发 stream:true 并返回上游 SSE 流。
func TestAnthropicProviderStream(t *testing.T) {
	var gotStream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var req AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode upstream: %v", err)
		}
		gotStream = req.Stream
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	p := NewAnthropicProvider(&Client{BaseURL: srv.URL, APIKey: "k"})
	body, err := p.Stream(context.Background(), &Request{
		Model:    "claude-sonnet-4-5",
		Stream:   true,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer body.Close()

	var events []SSEBlock
	if err := ReadSSE(body, func(b SSEBlock) error {
		events = append(events, b)
		return nil
	}); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !gotStream {
		t.Error("upstream stream = false, want true")
	}
	if len(events) != 2 || events[0].Event != "message_start" || events[1].Event != "message_stop" {
		t.Errorf("events = %+v", events)
	}
}

// 流式不受 http.Client 总超时约束(会话时长不受限,靠 context 取消终止)。
func TestStreamBypassesClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		flusher.Flush()
		time.Sleep(150 * time.Millisecond) // 超过客户端 50ms 总超时
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	p := NewAnthropicProvider(&Client{
		BaseURL: srv.URL,
		APIKey:  "k",
		HTTP:    &http.Client{Timeout: 50 * time.Millisecond},
	})
	body, err := p.Stream(context.Background(), &Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer body.Close()

	var events []SSEBlock
	if err := ReadSSE(body, func(b SSEBlock) error {
		events = append(events, b)
		return nil
	}); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(events) != 2 || events[0].Event != "message_start" || events[1].Event != "message_stop" {
		t.Errorf("events = %+v, want both despite client timeout", events)
	}
}

// 上游非 2xx 时 Stream 应返回 apiError,而非开始流式。
func TestStreamUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	p := NewAnthropicProvider(&Client{BaseURL: srv.URL, APIKey: "k"})
	_, err := p.Stream(context.Background(), &Request{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("expected error for 401 upstream")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want 401 mention", err)
	}
}
