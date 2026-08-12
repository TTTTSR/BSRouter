package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"BSRouter/internal/group"
	"BSRouter/internal/provider"
)

// newAuthServer 构造启用 API Key 鉴权的网关测试服务。
func newAuthServer(t *testing.T, key string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKey(key).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// doAuthReq 发起带自定义请求头的请求;POST/PUT 附带一个合法的 JSON 体。
func doAuthReq(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost || method == http.MethodPut {
		body = strings.NewReader(`{"model":"x","max_tokens":10,"messages":[]}`)
	}
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestAPIKeyAuthRequired(t *testing.T) {
	srv := newAuthServer(t, "topsecret")
	cases := []struct {
		name, method, path string
		hdrs               map[string]string
		want               int
	}{
		// 模型列表端点已公开(见 TestPublicModelLists),鉴权用例改用其余受保护端点。
		{"no key providers", http.MethodGet, "/manage/v1/providers", nil, http.StatusUnauthorized},
		{"empty bearer header", http.MethodGet, "/manage/v1/providers", map[string]string{"Authorization": "Bearer "}, http.StatusUnauthorized},
		{"wrong key", http.MethodGet, "/manage/v1/providers", map[string]string{"Authorization": "Bearer wrong"}, http.StatusUnauthorized},
		{"no key forward", http.MethodPost, "/api/v1/chat/completions", nil, http.StatusUnauthorized},
		{"no key management write", http.MethodPost, "/manage/v1/providers", nil, http.StatusUnauthorized},
		{"bearer ok", http.MethodGet, "/manage/v1/providers", map[string]string{"Authorization": "Bearer topsecret"}, http.StatusOK},
		{"x-api-key ok", http.MethodGet, "/manage/v1/providers", map[string]string{"x-api-key": "topsecret"}, http.StatusOK},
		// 鉴权通过后按业务逻辑继续:未注册供应商 -> 404。
		{"bearer ok forward", http.MethodPost, "/api/v1/chat/completions", map[string]string{"Authorization": "Bearer topsecret"}, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := doAuthReq(t, srv, c.method, c.path, c.hdrs)
			if got.StatusCode != c.want {
				t.Errorf("%s %s status = %d, want %d", c.method, c.path, got.StatusCode, c.want)
			}
			if got.StatusCode == http.StatusUnauthorized {
				if h := got.Header.Get("Www-Authenticate"); h == "" {
					t.Errorf("401 response missing WWW-Authenticate header")
				}
			}
		})
	}
}

// 模型列表端点公开:统一 API(/api/v1/models)、管理端同源(/manage/v1/models)
// 与分组虚拟供应商({分组URL}/v1/models)均无需鉴权,无 key 或错误 key 都返回
// 200;其余 /api 与 /manage 端点仍强制鉴权。
func TestPublicModelLists(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: "http://o", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"oa@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithAPIKey("topsecret").WithGroups(gm).Handler())
	t.Cleanup(srv.Close)

	for _, path := range []string{"/api/v1/models", "/manage/v1/models", "/api/team-a/v1/models"} {
		// 无 key。
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("public %s without key status = %d, want 200", path, resp.StatusCode)
		}
		// 错误 key 同样放行(公开端点不校验)。
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer wrong")
		resp2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Errorf("public %s with wrong key status = %d, want 200", path, resp2.StatusCode)
		}
	}

	// 其余端点仍强制鉴权:管理读接口、统一 API 与分组转发写接口。
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/manage/v1/providers"},
		{http.MethodPost, "/api/v1/chat/completions"},
		{http.MethodPost, "/api/team-a/v1/chat/completions"},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(`{"model":"x","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without key status = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
}

// 鉴权必须先于请求体解码:未鉴权的非法/超大请求体应返回 401 而非 400/413。
func TestAuthBeforeBodyDecode(t *testing.T) {
	srv := newAuthServer(t, "topsecret")

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/chat/completions", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("malformed body without key status = %d, want 401 (auth before decode)", resp.StatusCode)
	}
}

// 配置带首尾空白的 key 会被裁剪,请求侧用干净 key 即可通过。
func TestAPIKeyTrimmed(t *testing.T) {
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKey("  topsecret\n").Handler())
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer topsecret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("trimmed key status = %d, want 200", resp.StatusCode)
	}
}

func TestAPIKeyNotConfiguredAllowsAll(t *testing.T) {
	// 未配置 key 时保持无鉴权(测试/嵌入场景)。
	srv := httptest.NewServer(New(newMgr(t)).Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("no-key server status = %d, want 200", resp.StatusCode)
	}
}

func TestAPIKeyFromRequest(t *testing.T) {
	cases := []struct {
		name, auth, xkey, want string
	}{
		{"bearer", "Bearer abc", "", "abc"},
		{"bearer lowercase", "bearer abc", "", "abc"},
		{"bearer extra spaces", "Bearer   abc  ", "", "abc"},
		{"bearer no token", "Bearer", "", ""},
		{"bearer empty token", "Bearer ", "", ""},
		{"basic ignored then xkey", "Basic dXNlcjpwYXNz", "xyz", "xyz"},
		{"xkey only", "", "xyz", "xyz"},
		{"neither", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			if c.xkey != "" {
				r.Header.Set("x-api-key", c.xkey)
			}
			if got := apiKeyFromRequest(r); got != c.want {
				t.Errorf("apiKeyFromRequest = %q, want %q", got, c.want)
			}
		})
	}
}
