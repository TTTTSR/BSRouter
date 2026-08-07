package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/gateway"
	"BSRouter/internal/provider"
)

// newTestServer 构造一个网关服务:注册指向假上游的供应商,返回 handler 与上游地址。
func newTestServer(t *testing.T, kind provider.Kind, name string) (*httptest.Server, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 上游按不同格式回显,model 与请求体一致(不带供应商前缀)。
		switch r.URL.Path {
		case "/v1/messages":
			fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"from-anthropic"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"from-completion"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		case "/v1/responses":
			fmt.Fprint(w, `{"id":"r1","object":"response","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"from-responses"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}))
	t.Cleanup(upstream.Close)

	mgr, err := provider.NewManager(filepath.Join(t.TempDir(), "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := provider.Config{Kind: kind, Name: name, BaseURL: upstream.URL, APIKey: "k"}
	if err := mgr.Add(cfg); err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(New(mgr).Handler()), upstream.URL
}

func TestForwardAnthropic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var body gateway.AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		if body.Model != "claude-sonnet-4-5" {
			t.Errorf("upstream received model = %q, want bare model", body.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindAnthropic, Name: "cli", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(New(mgr).Handler())
	defer gs.Close()

	body := `{"model":"cli@claude-sonnet-4-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gs.URL+"/api/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out gateway.AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "cli@claude-sonnet-4-5" {
		t.Errorf("response model = %q, want prefixed", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "hello" {
		t.Errorf("content = %+v", out.Content)
	}
	if out.Usage.InputTokens != 2 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

// Claude Code 有时把 system 发成 text 内容块数组(带 cache_control),此前网关只接受
// string 而报 400;现在应归一化为字符串正常转发。
func TestForwardAnthropicArraySystem(t *testing.T) {
	var gotSystem string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var body gateway.AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream: %v", err)
		}
		gotSystem = string(body.System)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindAnthropic, Name: "cli", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(New(mgr).Handler())
	defer gs.Close()

	body := `{"model":"cli@claude-sonnet-4-5","max_tokens":100,` +
		`"system":[{"type":"text","text":"sys one","cache_control":{"type":"ephemeral"}},{"type":"text","text":"sys two"}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gs.URL+"/api/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, rb)
	}
	if gotSystem != "sys onesys two" {
		t.Errorf("upstream system = %q, want concatenated text", gotSystem)
	}
}

func TestForwardCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var body gateway.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		if body.Model != "gpt-4o" {
			t.Errorf("upstream received model = %q, want bare model", body.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(New(mgr).Handler())
	defer gs.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gs.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out gateway.CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "oa@gpt-4o" {
		t.Errorf("response model = %q, want prefixed", out.Model)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "hi" {
		t.Errorf("choices = %+v", out.Choices)
	}
}

func TestForwardResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var body gateway.ResponsesRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		if body.Model != "gpt-5" {
			t.Errorf("upstream received model = %q, want bare model", body.Model)
		}
		if body.Instructions != "be helpful" {
			t.Errorf("instructions = %q", body.Instructions)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"r1","object":"response","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindResponses, Name: "rg", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(New(mgr).Handler())
	defer gs.Close()

	body := `{"model":"rg@gpt-5","instructions":"be helpful","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	resp, err := http.Post(gs.URL+"/api/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out gateway.ResponsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "rg@gpt-5" {
		t.Errorf("response model = %q, want prefixed", out.Model)
	}
	if len(out.Output) != 1 || out.Output[0].Content[0].Text != "hi" {
		t.Errorf("output = %+v", out.Output)
	}
}

func TestUnknownProvider(t *testing.T) {
	gs, _ := newTestServer(t, provider.KindCompletion, "oa")
	defer gs.Close()

	resp, err := http.Post(gs.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"nobody@gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestBadRequest(t *testing.T) {
	gs, _ := newTestServer(t, provider.KindAnthropic, "cli")
	defer gs.Close()

	resp, err := http.Post(gs.URL+"/api/v1/messages", "application/json", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":"upstream exploded"}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindAnthropic, Name: "cli", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(New(mgr).Handler())
	defer gs.Close()

	body := `{"model":"cli@claude-sonnet-4-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gs.URL+"/api/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestIsAPIPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/v1/models", true},
		{"/api/team-a/v1/models", true},
		{"/manage/v1/providers", true},
		{"/manage/v1/providers/oa/ping", true},
		{"/api", true},
		{"/manage", true},
		{"/", false},
		{"/index.html", false},
		{"/assets/index-abc.js", false},
		{"/../manage/v1/providers", true}, // 归一化后仍是 API 路径
		{"/foo/../manage/v1/providers", true},
		{"/api//v1/models", true},
	}
	for _, c := range cases {
		if got := isAPIPath(c.path); got != c.want {
			t.Errorf("isAPIPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
