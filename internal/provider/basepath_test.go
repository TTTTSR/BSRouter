package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBasePathOverrideModelsURL 验证 base_path 覆盖后,模型列表接口命中自定义路径
// (如火山方舟 /api/v3/models)。
func TestBasePathOverrideModelsURL(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/models" {
			t.Errorf("path = %q, want /api/v3/models", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	p, err := New(Config{Kind: KindCompletion, Name: "a", BaseURL: up.URL, BasePath: "/api/v3", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestBasePathOverrideChatCompletions 验证 base_path 覆盖后,直通请求命中自定义路径
// (如百度千帆 /v2/chat/completions)。
func TestBasePathOverrideChatCompletions(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path = %q, want /v2/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer up.Close()

	p, err := New(Config{Kind: KindCompletion, Name: "a", BaseURL: up.URL, BasePath: "/v2", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.CompleteRaw(context.Background(), KindCompletion, json.RawMessage(`{"model":"x","messages":[]}`)); err != nil {
		t.Fatalf("CompleteRaw: %v", err)
	}
}

// TestBasePathRoot 验证 base_path="/"(根路径)时端点直接拼到 base_url 后,
// 无版本段(如 Gemini 的 .../v1beta/openai/chat/completions)。
func TestBasePathRoot(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer up.Close()

	p, err := New(Config{Kind: KindCompletion, Name: "a", BaseURL: up.URL, BasePath: "/", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.CompleteRaw(context.Background(), KindCompletion, json.RawMessage(`{"model":"x","messages":[]}`)); err != nil {
		t.Fatalf("CompleteRaw: %v", err)
	}
}

// TestBasePathInvalid 验证非法 base_path(不以 "/" 开头)被拒绝。
func TestBasePathInvalid(t *testing.T) {
	if _, err := New(Config{Kind: KindCompletion, Name: "a", BaseURL: "http://x", BasePath: "v3", APIKey: "k"}); err == nil {
		t.Fatal("expected error for base_path without leading '/'")
	}
}
