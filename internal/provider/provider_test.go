package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"BSRouter/internal/gateway"
)

// 编译期断言:BaseProvider 同时满足本包 Provider 与 gateway.Provider 接口。
var (
	_ Provider         = (*BaseProvider)(nil)
	_ gateway.Provider = (*BaseProvider)(nil)
)

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"ok", Config{Kind: KindAnthropic, Name: "a", BaseURL: "http://a"}, true},
		{"empty name", Config{Kind: KindAnthropic, Name: "", BaseURL: "http://a"}, false},
		{"empty base url", Config{Kind: KindAnthropic, Name: "a", BaseURL: ""}, false},
		{"unknown kind", Config{Kind: "nope", Name: "a", BaseURL: "http://a"}, false},
		{"empty model", Config{Kind: KindAnthropic, Name: "a", BaseURL: "http://a", Models: []ModelConfig{{Name: ""}}}, false},
		{"invalid model kind", Config{Kind: KindAnthropic, Name: "a", BaseURL: "http://a", Models: []ModelConfig{{Name: "x", Kind: "nope"}}}, false},
		{"duplicate model", Config{Kind: KindAnthropic, Name: "a", BaseURL: "http://a", Models: []ModelConfig{{Name: "x"}, {Name: "x"}}}, false},
		{"slash in name", Config{Kind: KindAnthropic, Name: "a/b", BaseURL: "http://a"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.cfg.Validate(); (err == nil) != c.ok {
				t.Errorf("Validate() err = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

func TestNewKind(t *testing.T) {
	if _, err := New(Config{Kind: "nope", Name: "a", BaseURL: "http://a"}); err == nil {
		t.Fatal("expected error for unknown kind")
	}
	if _, err := New(Config{Kind: KindAnthropic, Name: "a", BaseURL: "http://a"}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestAnthropicProviderComplete(t *testing.T) {
	recv := make(chan gateway.AnthropicRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b gateway.AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode: %v", err)
		}
		recv <- b
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5",`+
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer srv.Close()

	p, err := New(Config{Kind: KindAnthropic, Name: "anthro", BaseURL: srv.URL, APIKey: "k", Models: []ModelConfig{{Name: "claude-sonnet-4-5"}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "anthro" || p.Kind() != KindAnthropic || len(p.Models()) != 1 {
		t.Errorf("metadata = %q/%q/%v", p.Name(), p.Kind(), p.Models())
	}
	resp, err := p.Complete(context.Background(), &gateway.Request{
		Model:    "claude-sonnet-4-5",
		Messages: []gateway.Message{{Role: gateway.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	b := <-recv
	if b.Model != "claude-sonnet-4-5" {
		t.Errorf("upstream model = %q", b.Model)
	}
	if resp.Content != "hi" || resp.Usage.OutputTokens != 1 {
		t.Errorf("response = %+v", resp)
	}
}

// 同一供应商的不同模型可使用不同接口格式:按模型实际格式派发到对应上游端点。
func TestProviderPerModelKind(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"comp"},"finish_reason":"stop"}],"usage":{}}`)
		case "/v1/messages":
			fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"anth"}],"stop_reason":"end_turn","usage":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	// 供应商默认 completion;m1 用默认,m2 指定 anthropic。
	p, err := New(Config{
		Kind:    KindCompletion,
		Name:    "p",
		BaseURL: up.URL,
		APIKey:  "k",
		Models:  []ModelConfig{{Name: "m1"}, {Name: "m2", Kind: KindAnthropic}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ModelKind("m1") != KindCompletion {
		t.Errorf("m1 kind = %q, want completion (default)", p.ModelKind("m1"))
	}
	if p.ModelKind("m2") != KindAnthropic {
		t.Errorf("m2 kind = %q, want anthropic (model override)", p.ModelKind("m2"))
	}
	if p.ModelKind("unknown") != KindCompletion {
		t.Errorf("unknown model kind = %q, want fallback to provider default", p.ModelKind("unknown"))
	}

	// m1 走 completion 端点。
	resp, err := p.Complete(context.Background(), &gateway.Request{Model: "m1", Messages: []gateway.Message{{Role: gateway.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "comp" {
		t.Errorf("m1 content = %q, want comp", resp.Content)
	}
	// m2 走 anthropic 端点。
	resp2, err := p.Complete(context.Background(), &gateway.Request{Model: "m2", Messages: []gateway.Message{{Role: gateway.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.Content != "anth" {
		t.Errorf("m2 content = %q, want anth", resp2.Content)
	}
}

// 旧版 providers.json 的 "models":["gpt-4o"] 字符串形式应能兼容加载为新对象形式。
func TestConfigLegacyModelsFormat(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"kind":"completion","name":"a","base_url":"http://a","models":["gpt-4o","gpt-5"]}`), &c); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if len(c.Models) != 2 || c.Models[0].Name != "gpt-4o" || c.Models[1].Name != "gpt-5" || c.Models[0].Kind != "" {
		t.Errorf("models = %+v", c.Models)
	}
	// 新对象形式正常。
	var c2 Config
	if err := json.Unmarshal([]byte(`{"kind":"completion","name":"a","base_url":"http://a","models":[{"name":"gpt-4o","kind":"anthropic"}]}`), &c2); err != nil {
		t.Fatal(err)
	}
	if c2.Models[0].Kind != KindAnthropic {
		t.Errorf("kind = %q, want anthropic", c2.Models[0].Kind)
	}
}
