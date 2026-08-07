package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// 根路径返回 index.html。
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>BSRouter") {
		t.Errorf("root = %d %q", resp.StatusCode, truncate(string(body)))
	}

	// 目录请求拒绝列表(/assets 与 /assets/)。
	for _, p := range []string{"/assets/", "/assets"} {
		r, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (no directory listing)", p, r.StatusCode)
		}
	}

	// 真实资源(由 index.html 引用)可访问。
	asset := firstAsset(t, string(body))
	r, err := http.Get(srv.URL + asset)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d, want 200", asset, r.StatusCode)
	}
}

func firstAsset(t *testing.T, html string) string {
	t.Helper()
	idx := strings.Index(html, "/assets/")
	if idx < 0 {
		t.Fatalf("no /assets/ reference in index.html: %q", truncate(html))
	}
	end := idx + len("/assets/")
	for end < len(html) && html[end] != '"' && html[end] != '\'' && html[end] != ' ' {
		end++
	}
	return html[idx:end]
}

func truncate(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
