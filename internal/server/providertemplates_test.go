package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestListProviderTemplates 验证内置供应商模板列表端点返回非空目录,且每个模板
// 携带接入所需的字段(不含任何密钥)。
func TestListProviderTemplates(t *testing.T) {
	srv := newAPI(t, newMgr(t))

	resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/provider-templates", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var templates []struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Kind        string `json:"kind"`
		BaseURL     string `json:"base_url"`
	}
	if err := json.Unmarshal([]byte(body), &templates); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("expected non-empty template catalog")
	}
	for _, tmpl := range templates {
		if tmpl.Name == "" || tmpl.Kind == "" || tmpl.BaseURL == "" {
			t.Errorf("template %q missing required field (kind=%q base_url=%q)", tmpl.Name, tmpl.Kind, tmpl.BaseURL)
		}
	}
}
