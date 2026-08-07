package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/apikey"
)

func newKeyMgr(t *testing.T) *apikey.Manager {
	t.Helper()
	km, err := apikey.NewManager(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	return km
}

// doAuthed 与 doJSON 相同,但携带 Bearer key 的鉴权头。
func doAuthed(t *testing.T, srv *httptest.Server, method, path, body, key string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestAPIKeysEndpoints(t *testing.T) {
	km := newKeyMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKeys(km).WithAPIKey("admin").Handler())
	defer srv.Close()

	// 生成 -> 201,返回完整 Key。
	resp, body := doAuthed(t, srv, http.MethodPost, "/manage/v1/keys", `{"name":"team-a"}`, "admin")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, body)
	}
	var k apikey.Config
	if err := json.Unmarshal([]byte(body), &k); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Key, "sk-") || len(k.Key) != 3+64 {
		t.Errorf("key format = %q", k.Key)
	}

	// 重复 -> 409;非法(空名)-> 400。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/keys", `{"name":"team-a"}`, "admin"); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", resp.StatusCode)
	}
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/keys", `{"name":""}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty name status = %d, want 400", resp.StatusCode)
	}

	// 列表 -> 200 且包含 team-a。
	if resp, body := doAuthed(t, srv, http.MethodGet, "/manage/v1/keys", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("list status = %d", resp.StatusCode)
	} else {
		var list []apikey.Config
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Name != "team-a" {
			t.Errorf("list = %+v", list)
		}
	}

	// 删除 -> 204;再删 -> 404。
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/keys/team-a", "", "admin"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", resp.StatusCode)
	}
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/keys/team-a", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete again status = %d, want 404", resp.StatusCode)
	}
}

// 受管 apikey 可访问 /api,但不能访问 /manage;网关 key 两者皆可。
func TestManagedKeyAuth(t *testing.T) {
	km := newKeyMgr(t)
	k, err := km.Generate("client-a")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKeys(km).WithAPIKey("admin").Handler())
	defer srv.Close()

	// 受管 key 访问 /api -> 鉴权通过(未注册供应商 -> 404)。
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/chat/completions",
		strings.NewReader(`{"model":"nope@gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("managed key /api status = %d, want 404 (auth passed)", resp.StatusCode)
	}

	// 受管 key 访问 /manage -> 401。
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/manage/v1/providers", nil)
	req2.Header.Set("Authorization", "Bearer "+k.Key)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("managed key /manage status = %d, want 401", resp2.StatusCode)
	}

	// 网关 key 访问 /api -> 鉴权通过。
	req3, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer admin")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("gateway key /api status = %d, want 200", resp3.StatusCode)
	}
}

func TestManagedKeyAuthNone(t *testing.T) {
	// 无受管 key、无网关 key -> /api 保持开放。
	km := newKeyMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKeys(km).Handler())
	defer srv.Close()
	resp, _ := doJSON(t, srv, http.MethodGet, "/api/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("no-auth /api status = %d, want 200", resp.StatusCode)
	}
}
