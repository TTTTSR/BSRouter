package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPingOK(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	p, err := New(Config{Kind: KindCompletion, Name: "a", BaseURL: up.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !res.OK || res.StatusCode != 200 {
		t.Errorf("Ping = %+v", res)
	}
}

func TestPingUnauthorized(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer up.Close()

	p, _ := New(Config{Kind: KindCompletion, Name: "a", BaseURL: up.URL, APIKey: "k"})
	res, err := p.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.OK || res.StatusCode != 401 || res.Error == "" {
		t.Errorf("Ping = %+v", res)
	}
}

func TestPingAnthropicAuth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("anthropic-version = %q", r.Header.Get("anthropic-version"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	p, _ := New(Config{Kind: KindAnthropic, Name: "a", BaseURL: up.URL, APIKey: "k"})
	res, _ := p.Ping(context.Background())
	if !res.OK {
		t.Errorf("Ping = %+v", res)
	}
}

func TestPingCustomModelsURL(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/models" {
			t.Errorf("path = %q, want /custom/models", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	p, _ := New(Config{Kind: KindCompletion, Name: "a", BaseURL: up.URL, APIKey: "k", ModelsURL: up.URL + "/custom/models"})
	res, _ := p.Ping(context.Background())
	if !res.OK {
		t.Errorf("Ping = %+v", res)
	}
}

func TestFetchModels(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-5","object":"model"}]}`)
	}))
	defer up.Close()

	p, _ := New(Config{Kind: KindCompletion, Name: "a", BaseURL: up.URL, APIKey: "k"})
	models, err := p.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "gpt-5" {
		t.Errorf("models = %v", models)
	}
}

func TestQueryUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total":100,"used":23}`)
	}))
	defer up.Close()

	p, _ := New(Config{Kind: KindCompletion, Name: "a", BaseURL: up.URL, APIKey: "k", UsageURL: up.URL + "/usage"})
	data, err := p.QueryUsage(context.Background())
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if string(data) != `{"total":100,"used":23}` {
		t.Errorf("data = %s", data)
	}
}

func TestQueryUsageNotConfigured(t *testing.T) {
	p, _ := New(Config{Kind: KindCompletion, Name: "a", BaseURL: "http://x", APIKey: "k"})
	if _, err := p.QueryUsage(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("QueryUsage err = %v, want ErrNotConfigured", err)
	}
}
