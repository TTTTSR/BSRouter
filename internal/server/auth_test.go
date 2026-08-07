package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		{"no key models", http.MethodGet, "/api/v1/models", nil, http.StatusUnauthorized},
		{"no key providers", http.MethodGet, "/manage/v1/providers", nil, http.StatusUnauthorized},
		{"empty bearer header", http.MethodGet, "/api/v1/models", map[string]string{"Authorization": "Bearer "}, http.StatusUnauthorized},
		{"wrong key", http.MethodGet, "/api/v1/models", map[string]string{"Authorization": "Bearer wrong"}, http.StatusUnauthorized},
		{"no key forward", http.MethodPost, "/api/v1/chat/completions", nil, http.StatusUnauthorized},
		{"no key management write", http.MethodPost, "/manage/v1/providers", nil, http.StatusUnauthorized},
		{"bearer ok", http.MethodGet, "/api/v1/models", map[string]string{"Authorization": "Bearer topsecret"}, http.StatusOK},
		{"x-api-key ok", http.MethodGet, "/api/v1/models", map[string]string{"x-api-key": "topsecret"}, http.StatusOK},
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
