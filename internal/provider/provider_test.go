package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		{"negative context window", Config{Kind: KindAnthropic, Name: "a", BaseURL: "http://a", Models: []ModelConfig{{Name: "x", ContextWindow: -1}}}, false},
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

// 模型的 context_window 字段(k 为单位)应随对象形式反序列化并被写回。
func TestModelConfigContextWindow(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"kind":"completion","name":"a","base_url":"http://a",`+
		`"models":[{"name":"deepseek-v4-flash","context_window":128},{"name":"plain"}]}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(c.Models) != 2 || c.Models[0].ContextWindow != 128 || c.Models[1].ContextWindow != 0 {
		t.Errorf("models = %+v", c.Models)
	}
	// 写回 JSON 应带出 context_window(omitempty,0 不输出)。
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"context_window":128`) || strings.Count(string(out), "context_window") != 1 {
		t.Errorf("marshal = %s", out)
	}
	// 旧字符串形式仍兼容且窗口为零。
	var legacy Config
	if err := json.Unmarshal([]byte(`{"kind":"completion","name":"a","base_url":"http://a","models":["gpt-4o"]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Models[0].Name != "gpt-4o" || legacy.Models[0].ContextWindow != 0 {
		t.Errorf("legacy models = %+v", legacy.Models)
	}
}

// 模型可配置多个支持的接口格式(kinds);Kinds 优先于旧 Kind 字段,并做去重。
func TestModelKinds(t *testing.T) {
	p, err := New(Config{
		Kind:    KindCompletion,
		Name:    "p",
		BaseURL: "http://up",
		APIKey:  "k",
		Models: []ModelConfig{
			{Name: "multi", Kinds: []Kind{KindAnthropic, KindResponses, KindAnthropic}}, // 去重
			{Name: "single", Kind: KindAnthropic},                                        // 旧单格式
			{Name: "inherit"},                                                            // 无覆盖
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ModelKinds("multi"); len(got) != 2 || got[0] != KindAnthropic || got[1] != KindResponses {
		t.Errorf("multi kinds = %v, want [anthropic responses] (dedup)", got)
	}
	if got := p.ModelKinds("single"); len(got) != 1 || got[0] != KindAnthropic {
		t.Errorf("single kinds = %v, want [anthropic]", got)
	}
	if got := p.ModelKinds("inherit"); len(got) != 1 || got[0] != KindCompletion {
		t.Errorf("inherit kinds = %v, want [completion] (provider default)", got)
	}
	// ModelKind = ModelKinds 首位(转换路径选上游格式)。
	if p.ModelKind("multi") != KindAnthropic {
		t.Errorf("ModelKind(multi) = %q, want anthropic (first)", p.ModelKind("multi"))
	}
	// Supports 判定。
	if !p.Supports("multi", KindAnthropic) || !p.Supports("multi", KindResponses) {
		t.Error("multi should support anthropic and responses")
	}
	if p.Supports("multi", KindCompletion) {
		t.Error("multi should NOT support completion")
	}
	if !p.Supports("single", KindAnthropic) || p.Supports("single", KindCompletion) {
		t.Error("single should support only anthropic")
	}
	if !p.Supports("inherit", KindCompletion) || p.Supports("inherit", KindAnthropic) {
		t.Error("inherit should support only completion (default)")
	}
}

// 配置校验:kinds 中非法格式应被拒绝;Kinds 与 Kind 可共存(Kinds 优先)。
func TestConfigValidateKinds(t *testing.T) {
	ok := Config{Kind: KindCompletion, Name: "a", BaseURL: "http://a", Models: []ModelConfig{
		{Name: "x", Kinds: []Kind{KindAnthropic, KindCompletion}},
		{Name: "y", Kind: KindAnthropic, Kinds: []Kind{KindCompletion}}, // Kinds 优先,Kind 被忽略
	}}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate(ok) err = %v, want nil", err)
	}
	bad := Config{Kind: KindCompletion, Name: "a", BaseURL: "http://a", Models: []ModelConfig{
		{Name: "x", Kinds: []Kind{KindAnthropic, "nope"}},
	}}
	if err := bad.Validate(); err == nil {
		t.Error("Validate(bad kinds) err = nil, want error")
	}
}

// 直通原始转发:completion 格式按原样转发,上游收到的请求体与客户端一致(除 model 改写由上层做)。
func TestProviderCompleteRaw(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"raw"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	p, err := New(Config{Kind: KindCompletion, Name: "p", BaseURL: up.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	status, body, err := p.CompleteRaw(context.Background(), KindCompletion, raw)
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if string(gotBody) != string(raw) {
		t.Errorf("upstream got body = %s, want %s (raw passthrough)", gotBody, raw)
	}
	if string(body) == "" || !bytes.Contains(body, []byte(`"raw"`)) {
		t.Errorf("response body = %s", body)
	}
}
