package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"BSRouter/internal/gateway"
	"BSRouter/internal/group"
	"BSRouter/internal/provider"
)

// decodeBody 解码测试用上游请求体。
func decodeBody(r *http.Request, out any) error {
	return json.NewDecoder(r.Body).Decode(out)
}

// Claude Code 请求流式输出:网关同格式透传上游 SSE,并把响应模型回填为完整 id。
func TestStreamAnthropic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var body gateway.AnthropicRequest
		if err := decodeBody(r, &body); err != nil {
			t.Errorf("decode upstream: %v", err)
			return
		}
		if !body.Stream {
			t.Errorf("upstream stream = %v, want true", body.Stream)
		}
		if body.Model != "claude-sonnet-4-5" {
			t.Errorf("upstream model = %q, want bare", body.Model)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n"+
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-sonnet-4-5\",\"content\":[]}}\n\n"+
			"event: content_block_delta\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"+
			"event: message_stop\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindAnthropic, Name: "cli", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"cli@claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	data, _ := io.ReadAll(resp.Body)
	got := string(data)
	if !strings.Contains(got, `"model":"cli@claude-sonnet-4-5"`) {
		t.Errorf("response model not backfilled:\n%s", got)
	}
	if !strings.Contains(got, "event: content_block_delta") || !strings.Contains(got, `"text":"hi"`) {
		t.Errorf("response missing delta passthrough:\n%s", got)
	}
	if !strings.Contains(got, "event: message_stop") {
		t.Errorf("response missing message_stop:\n%s", got)
	}
}

// chat.completions 流式:data: 事件透传 + 模型回填 + 末尾 [DONE]。
func TestStreamCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body gateway.CompletionRequest
		if err := decodeBody(r, &body); err != nil {
			t.Errorf("decode upstream: %v", err)
			return
		}
		if !body.Stream {
			t.Errorf("upstream stream = %v, want true", body.Stream)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"oa@gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	got := string(data)
	if !strings.Contains(got, `"model":"oa@gpt-4o"`) {
		t.Errorf("response model not backfilled:\n%s", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("response missing [DONE]:\n%s", got)
	}
	if strings.Contains(got, "event:") {
		t.Errorf("completion stream should not carry event: lines:\n%s", got)
	}
}

// 跨格式流式:Claude Code(Anthropic 客户端)→ completion 上游,响应经规范化事件转回
// Anthropic SSE(模型回填、stop_reason 映射)。
func TestStreamCrossFormatAnthropicToCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var body gateway.CompletionRequest
		if err := decodeBody(r, &body); err != nil {
			t.Errorf("decode upstream: %v", err)
			return
		}
		if !body.Stream {
			t.Errorf("upstream stream = %v, want true", body.Stream)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "ds", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).Handler())
	t.Cleanup(srv.Close)

	// Claude Code 走 /api/v1/messages(Anthropic),模型路由到 completion 上游。
	reqBody := `{"model":"ds@deepseek-v4-flash","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	got := string(data)
	if !strings.Contains(got, "event: content_block_delta") || !strings.Contains(got, `"type":"text_delta"`) {
		t.Errorf("expected anthropic SSE delta:\n%s", got)
	}
	if !strings.Contains(got, `"model":"ds@deepseek-v4-flash"`) {
		t.Errorf("response model not backfilled:\n%s", got)
	}
	if !strings.Contains(got, `"stop_reason":"end_turn"`) {
		t.Errorf("response missing stop_reason:\n%s", got)
	}
	if !strings.Contains(got, "event: message_stop") {
		t.Errorf("response missing message_stop:\n%s", got)
	}
}

// 分组虚拟供应商同样支持流式透传。
func TestStreamGroupAnthropic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n"+
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-sonnet-4-5\",\"content\":[]}}\n\n"+
			"event: message_stop\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindAnthropic, Name: "cli", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindAnthropic, Models: []string{"cli@claude-sonnet-4-5"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithGroups(gm).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"cli@claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/team-a/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), `"model":"cli@claude-sonnet-4-5"`) {
		t.Errorf("group response model not backfilled:\n%s", data)
	}
}

// 上游非 2xx 时,流式请求应返回 502(不进入流式)。
func TestStreamUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindAnthropic, Name: "cli", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"cli@claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// 跨格式流式 + thinking:codex(responses 客户端)发流式请求 → completion 上游返回
// reasoning_content(deepseek thinking),网关应把思考内容以 responses reasoning item
// 输出给 codex;codex 回传 reasoning item 时,网关应转回 completion 的 reasoning_content
// 发给上游(否则上游 400 "reasoning_content must be passed back")。
func TestStreamResponsesToCompletionReasoning(t *testing.T) {
	var gotUpstreamModel string
	var gotUpstreamMsgs []gateway.CompletionMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body gateway.CompletionRequest
		if err := decodeBody(r, &body); err != nil {
			t.Errorf("decode upstream: %v", err)
			return
		}
		gotUpstreamModel = body.Model
		gotUpstreamMsgs = body.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":null,"reasoning_content":"The user wants files."},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"hi","reasoning_content":null},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
				"data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "deepseek-v4-flash"}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).Handler())
	t.Cleanup(srv.Close)

	// codex 第一轮:responses 流式请求,无历史 reasoning。
	reqBody := `{"model":"oa@deepseek-v4-flash","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}]}`
	resp, err := http.Post(srv.URL+"/api/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, got)
	}
	if !strings.Contains(string(got), `"type":"reasoning"`) || !strings.Contains(string(got), `"text":"The user wants files."`) {
		t.Errorf("response missing reasoning item:\n%s", got)
	}
	if !strings.Contains(string(got), `"type":"output_text"`) || !strings.Contains(string(got), `"text":"hi"`) {
		t.Errorf("response missing text output:\n%s", got)
	}

	// codex 第二轮:回传历史 reasoning item + assistant 消息,上游应收到 reasoning_content。
	gotUpstreamMsgs = nil
	reqBody2 := `{"model":"oa@deepseek-v4-flash","stream":true,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"The user wants files."}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`
	resp2, err := http.Post(srv.URL+"/api/v1/responses", "application/json", strings.NewReader(reqBody2))
	if err != nil {
		t.Fatalf("POST 2: %v", err)
	}
	got2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status 2 = %d, want 200; body=%s", resp2.StatusCode, got2)
	}
	// 上游收到裸模型名 + reasoning 与 assistant 合并为一条(思考 + 文本同消息,
	// thinking 上游要求 reasoning_content 与 content 配对)。
	if gotUpstreamModel != "deepseek-v4-flash" {
		t.Errorf("upstream model = %q, want deepseek-v4-flash", gotUpstreamModel)
	}
	var sawMerged bool
	for _, mm := range gotUpstreamMsgs {
		if mm.Role == "assistant" && mm.ReasoningContent != nil && *mm.ReasoningContent == "The user wants files." && mm.Content == "hi" {
			sawMerged = true
		}
	}
	if !sawMerged {
		t.Errorf("upstream should receive reasoning_content + content on SAME assistant message: %+v", gotUpstreamMsgs)
	}
}
