package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 编译期接口断言:三种 Provider 都满足 Provider,三种请求体都满足 Requester。
var (
	_ Provider  = (*AnthropicProvider)(nil)
	_ Provider  = (*CompletionProvider)(nil)
	_ Provider  = (*ResponsesProvider)(nil)
	_ Requester = (*AnthropicRequest)(nil)
	_ Requester = (*CompletionRequest)(nil)
	_ Requester = (*ResponsesRequest)(nil)
)

type anthroRec struct {
	body AnthropicRequest
	hdrs http.Header
}

func TestAnthropicProviderComplete(t *testing.T) {
	recv := make(chan anthroRec, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode request: %v", err)
		}
		recv <- anthroRec{body: b, hdrs: r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5",`+
			`"content":[{"type":"text","text":"Hi there"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()

	p := NewAnthropicProvider(&Client{BaseURL: srv.URL, APIKey: "test-key"})
	resp, err := p.Complete(context.Background(), &Request{
		Model:     "claude-sonnet-4-5",
		MaxTokens: intPtr(100),
		Messages:  []Message{{Role: RoleUser, Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rec := <-recv
	if rec.hdrs.Get("x-api-key") != "test-key" {
		t.Errorf("x-api-key = %q", rec.hdrs.Get("x-api-key"))
	}
	if rec.hdrs.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("anthropic-version = %q", rec.hdrs.Get("anthropic-version"))
	}
	if rec.hdrs.Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q", rec.hdrs.Get("Content-Type"))
	}
	if rec.body.Model != "claude-sonnet-4-5" || rec.body.MaxTokens != 100 {
		t.Errorf("request body model/max_tokens = %q/%d", rec.body.Model, rec.body.MaxTokens)
	}
	if c := mustJSON(t, rec.body.Messages[0].Content); c != `"Hello"` {
		t.Errorf("message content = %s, want \"Hello\"", c)
	}
	if resp.Content != "Hi there" || resp.FinishReason != "stop" ||
		resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("response = %+v", resp)
	}
}

type compRec struct {
	body CompletionRequest
	auth string
}

func TestCompletionProviderComplete(t *testing.T) {
	recv := make(chan compRec, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode request: %v", err)
		}
		recv <- compRec{body: b, auth: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
	}))
	defer srv.Close()

	p := NewCompletionProvider(&Client{BaseURL: srv.URL, APIKey: "sk-test"})
	resp, err := p.Complete(context.Background(), &Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rec := <-recv
	if rec.auth != "Bearer sk-test" {
		t.Errorf("authorization = %q", rec.auth)
	}
	if rec.body.Model != "gpt-4o" || len(rec.body.Messages) != 1 ||
		rec.body.Messages[0].Role != "user" || rec.body.Messages[0].Content != "Hello" {
		t.Errorf("request body = %+v", rec.body)
	}
	if resp.Content != "Hi" || resp.FinishReason != "stop" ||
		resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 2 {
		t.Errorf("response = %+v", resp)
	}
}

type respRec struct {
	body ResponsesRequest
}

func TestResponsesProviderComplete(t *testing.T) {
	recv := make(chan respRec, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b ResponsesRequest
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode request: %v", err)
		}
		recv <- respRec{body: b}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_1","object":"response","model":"gpt-5","status":"completed",`+
			`"output":[{"type":"function_call","call_id":"fc_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}],`+
			`"usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}`)
	}))
	defer srv.Close()

	p := NewResponsesProvider(&Client{BaseURL: srv.URL, APIKey: "sk-test"})
	resp, err := p.Complete(context.Background(), &Request{
		Model:    "gpt-5",
		System:   "You are helpful.",
		Messages: []Message{{Role: RoleUser, Content: "Weather?"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rec := <-recv
	if rec.body.Instructions != "You are helpful." {
		t.Errorf("instructions = %q", rec.body.Instructions)
	}
	if len(rec.body.Input) != 1 || rec.body.Input[0].Type != "message" ||
		rec.body.Input[0].Role != "user" || rec.body.Input[0].Content[0].Text != "Weather?" {
		t.Errorf("input = %+v", rec.body.Input)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "get_weather" ||
		string(resp.ToolCalls[0].Arguments) != `{"city":"SF"}` {
		t.Errorf("tool_calls = %+v", resp.ToolCalls)
	}
	if resp.FinishReason != "stop" || resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 3 {
		t.Errorf("response = %+v", resp)
	}
}

func TestProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()

	p := NewCompletionProvider(&Client{BaseURL: srv.URL, APIKey: "k"})
	_, err := p.Complete(context.Background(), &Request{Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention status 429: %v", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should mention upstream body: %v", err)
	}
}

func TestNilRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	p := NewAnthropicProvider(&Client{BaseURL: srv.URL, APIKey: "k"})
	if _, err := p.Complete(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil request")
	}
}

// 统一入口:任意格式请求体都可通过 Do 发起请求,并返回规范化响应。
func TestRequesterDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","type":"message","role":"assistant","model":"m",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	req := &AnthropicRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages:  []AnthropicMessage{{Role: "user", Content: AnthropicContent{{Type: "text", Text: "hi"}}}},
	}
	resp, err := req.Do(context.Background(), &Client{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Content != "ok" || resp.Usage.OutputTokens != 1 {
		t.Errorf("response = %+v", resp)
	}
}
