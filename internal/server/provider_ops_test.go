package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/logger"
	"BSRouter/internal/provider"
)

func TestPingEndpoint(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)

	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers/oa/ping", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Errorf("ping result = %s", body)
	}
}

func TestPingEndpointUnknownProvider(t *testing.T) {
	srv := newAPI(t, newMgr(t))
	resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/providers/nope/ping", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSyncModelsEndpoint(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-5","object":"model"}]}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)

	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers/oa/sync-models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	p, err := m.Get("oa")
	if err != nil {
		t.Fatal(err)
	}
	models := p.Models()
	if len(models) != 2 || models[0].Name != "gpt-4o" || models[1].Name != "gpt-5" {
		t.Errorf("models after sync = %v", models)
	}
}

func TestUsageEndpoint(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total":100,"used":23}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k", UsageURL: up.URL + "/usage"}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)

	resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/providers/oa/usage", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"total":100`) {
		t.Errorf("usage body = %s", body)
	}
}

func TestUsageEndpointNotConfigured(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: "http://x", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)
	resp, _ := doJSON(t, srv, http.MethodGet, "/manage/v1/providers/oa/usage", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// 管理请求与未解析的转发请求都应写入 JSONL 日志。
func TestRequestLogging(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler())
	defer srv.Close()

	if _, err := http.Get(srv.URL + "/api/v1/models"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"nope@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entries := readLogEntries(t, logPath)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Method != "GET" || entries[0].Path != "/api/v1/models" || entries[0].Status != 200 {
		t.Errorf("first entry = %+v", entries[0])
	}
	if entries[0].Timestamp == "" || entries[0].RequestID == "" {
		t.Errorf("missing timestamp/request_id: %+v", entries[0])
	}
	if entries[1].Path != "/api/v1/chat/completions" || entries[1].Status != 404 || entries[1].Model != "nope@gpt-4o" {
		t.Errorf("second entry = %+v", entries[1])
	}
}

// 解析成功的转发请求,日志应包含 model/provider/kind。
func TestRequestLoggingForwardResolved(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Model != "oa@gpt-4o" || entries[0].Provider != "oa" || entries[0].Kind != "completion" {
		t.Errorf("entry = %+v", entries[0])
	}
}

// 鉴权拒绝的请求也应记录(401)。
func TestRequestLoggingUnauthorized(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	srv := httptest.NewServer(New(m).WithAPIKey("secret").WithLogger(lg).Handler())
	defer srv.Close()

	// 用受保护的转发端点而非公开的 /api/v1/models(模型列表公开,免 key)。
	if _, err := http.Get(srv.URL + "/api/v1/messages"); err != nil {
		t.Fatal(err)
	}

	entries := readLogEntries(t, logPath)
	if len(entries) != 1 || entries[0].Status != http.StatusUnauthorized {
		t.Errorf("entries = %+v", entries)
	}
}

// 上游返回空模型列表时,sync-models 应拒绝覆盖已知模型。
func TestSyncModelsEmptyRefuses(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)

	resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/providers/oa/sync-models", "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (empty list refused)", resp.StatusCode)
	}
	// 原有模型未被清空
	if p, _ := m.Get("oa"); len(p.Models()) != 1 || p.Models()[0].Name != "gpt-4o" {
		t.Errorf("models after refused sync = %v", p.Models())
	}
}

// 上游在 2xx 响应中回显 api_key 时,usage 透传应抹除密钥。
func TestUsagePassthroughRedactsKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo":"%s","total":100}`, "SECRETKEY123")
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "SECRETKEY123", UsageURL: up.URL + "/usage"}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)

	resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/providers/oa/usage", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "SECRETKEY123") {
		t.Errorf("usage body leaked api_key: %s", body)
	}
	if !strings.Contains(body, "***") {
		t.Errorf("usage body should contain redaction marker: %s", body)
	}
}

// 上游 401 错误体回显 api_key 时,sync-models 的 502 应抹除密钥。
func TestSyncModelsRedactsKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"bad key: Bearer SECRETKEY123"}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "SECRETKEY123"}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)

	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers/oa/sync-models", "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(body, "SECRETKEY123") {
		t.Errorf("502 body leaked api_key: %s", body)
	}
}

// 上游失败时,日志应记录 upstream_status 与错误信息。
func TestRequestLoggingUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", entries[0].Status)
	}
	if entries[0].UpstreamStatus != 429 {
		t.Errorf("upstream_status = %d, want 429", entries[0].UpstreamStatus)
	}
	if entries[0].Error == "" {
		t.Errorf("error field should be populated")
	}
}

// 转发请求的日志应记录:转发地址、转发请求体、转发响应体与上游状态码(均抹除 api_key)。
func TestRequestLoggingForwardDetail(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithLogger(lg).WithLogDetail(LogDetailFull).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if !strings.Contains(e.ForwardURL, "/v1/chat/completions") {
		t.Errorf("forward_url = %q", e.ForwardURL)
	}
	if !strings.Contains(e.ForwardRequest, `"model":"gpt-4o"`) {
		t.Errorf("forward_request = %q", e.ForwardRequest)
	}
	if !strings.Contains(e.ForwardResponse, `"content":"hello"`) {
		t.Errorf("forward_response = %q", e.ForwardResponse)
	}
	if e.UpstreamStatus != 200 {
		t.Errorf("upstream_status = %d, want 200", e.UpstreamStatus)
	}
	for _, v := range []string{e.ForwardURL, e.ForwardRequest, e.ForwardResponse} {
		if strings.Contains(v, "sk-test") {
			t.Errorf("api_key leaked in forward field: %s", v)
		}
	}
}

// 上游在响应中回显 api_key 时,转发响应日志应被抹除。
func TestRequestLoggingForwardRedactsKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo":"%s","total":1}`, "SUPERSECRET")
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "SUPERSECRET"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithLogger(lg).WithLogDetail(LogDetailFull).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if strings.Contains(entries[0].ForwardResponse, "SUPERSECRET") {
		t.Errorf("forward_response leaked api_key: %s", entries[0].ForwardResponse)
	}
	if !strings.Contains(entries[0].ForwardResponse, "***") {
		t.Errorf("forward_response should contain redaction marker: %s", entries[0].ForwardResponse)
	}
}

func TestCaptureBody(t *testing.T) {
	// 短 key 不做替换,避免破坏正文。
	if got := captureBody("kick start", "k"); got != "kick start" {
		t.Errorf("short key should not mangle content: %q", got)
	}
	// 长 key 抹除(含 URL 编码形态)。
	if got := captureBody("Bearer SECRETKEY123 end", "SECRETKEY123"); strings.Contains(got, "SECRETKEY123") || !strings.Contains(got, "***") {
		t.Errorf("redaction failed: %q", got)
	}
	if got := captureBody("key=verysecretkey%2Fabc", "verysecretkey/abc"); strings.Contains(got, "verysecretkey%2Fabc") {
		t.Errorf("URL-encoded key not redacted: %q", got)
	}
	// 超长内容按 rune 边界截断,不产生 U+FFFD。
	long := strings.Repeat("a", maxForwardBody-1) + "你" // 截断点落在这个 3 字节字符中间
	got := captureBody(long, "")
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("not truncated")
	}
	if strings.Contains(got, "�") {
		t.Errorf("truncation produced U+FFFD: tail=%q", got[len(got)-20:])
	}
}

// 上游不可达(传输层失败)时,日志仍应记录转发目标地址。
func TestRequestLoggingTransportFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: "http://127.0.0.1:9", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", e.Status)
	}
	if !strings.Contains(e.ForwardURL, "http://127.0.0.1:9") {
		t.Errorf("forward_url missing on transport failure: %q", e.ForwardURL)
	}
}

func readLogEntries(t *testing.T, path string) []logger.Entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries []logger.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e logger.Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("invalid JSON line: %v", err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestListLogsEndpoint(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	// 写入日志:两条 /api(转发)+ 一条 /manage(管理操作)。
	lg.Log(logger.Entry{Timestamp: "2026-01-01T00:00:00Z", Method: "GET", Path: "/api/v1/models", Status: 200})
	lg.Log(logger.Entry{Timestamp: "2026-01-01T00:00:01Z", Method: "POST", Path: "/api/v1/chat/completions", Status: 502, UpstreamStatus: 429})
	lg.Log(logger.Entry{Timestamp: "2026-01-01T00:00:02Z", Method: "PUT", Path: "/manage/v1/providers/oa", Status: 200})

	m := newMgr(t)
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler())
	defer srv.Close()

	// 默认只返回 /api 转发日志,不含 /manage。
	resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/logs?limit=10", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	var entries []logger.Entry
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (api only)", len(entries))
	}
	// 最新的在前,且都是 /api 路径。
	if entries[0].Path != "/api/v1/chat/completions" || entries[0].UpstreamStatus != 429 {
		t.Errorf("entries = %+v", entries)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, "/api") {
			t.Errorf("entry leaked manage path: %s", e.Path)
		}
	}

	// scope=all 返回全部(含 /manage):3 条原始日志 + 上一次 /manage/v1/logs 请求自身的日志。
	resp, body = doJSON(t, srv, http.MethodGet, "/manage/v1/logs?limit=10&scope=all", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("scope=all entries = %d, want 4 (3 written + 1 logs-request itself)", len(entries))
	}
	// 最新的一条是上一次 /manage/v1/logs 请求自身的日志(管理端点也会记录)。
	if entries[0].Path != "/manage/v1/logs" {
		t.Errorf("scope=all newest = %q, want /manage/v1/logs", entries[0].Path)
	}
}

func TestFetchModelsEndpoint(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/models" {
			t.Errorf("upstream path = %q, want /custom/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-5"}]}`)
	}))
	defer up.Close()

	srv := newAPI(t, newMgr(t))
	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/fetch-models",
		fmt.Sprintf(`{"kind":"completion","base_url":%q,"api_key":"k","models_url":%q}`, up.URL, up.URL+"/custom/models"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	var out struct {
		Models []string `json:"models"`
		Count  int      `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 || len(out.Models) != 2 || out.Models[0] != "gpt-4o" || out.Models[1] != "gpt-5" {
		t.Errorf("result = %+v", out)
	}
}

func TestFetchModelsEndpointErrors(t *testing.T) {
	srv := newAPI(t, newMgr(t))
	// 缺 base_url -> 400。
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/fetch-models",
		`{"kind":"completion","base_url":"","api_key":"k"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing base_url status = %d, want 400", resp.StatusCode)
	}
	// 上游错误 -> 502 且抹除 key。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"bad key: Bearer SECRET123"}`)
	}))
	defer up.Close()
	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/fetch-models",
		fmt.Sprintf(`{"kind":"completion","base_url":%q,"api_key":"SECRET123"}`, up.URL))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(body, "SECRET123") {
		t.Errorf("502 body leaked api_key: %s", body)
	}
}

// 编辑场景:api_key 留空 + 提供 name 时,应复用已注册供应商的存储密钥拉取模型。
func TestFetchModelsUsesStoredKeyOnEdit(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"}]}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "REAL-SECRET"}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)
	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/fetch-models",
		fmt.Sprintf(`{"name":"oa","kind":"completion","base_url":%q,"api_key":"","models_url":%q}`, up.URL, up.URL+"/v1/models"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	if gotAuth != "Bearer REAL-SECRET" {
		t.Errorf("upstream auth = %q, want stored key REAL-SECRET", gotAuth)
	}
}

// 同步模型时,已在列表中的模型应保留其模型级接口格式,新增模型用供应商默认。
func TestSyncModelsPreservesKind(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-5"}]}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{
		Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o", Kind: provider.KindResponses}},
	}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)
	if resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/providers/oa/sync-models", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("sync status = %d; body=%s", resp.StatusCode, body)
	}

	p, _ := m.Get("oa")
	for _, mc := range p.Models() {
		switch mc.Name {
		case "gpt-4o":
			if mc.Kind != provider.KindResponses {
				t.Errorf("gpt-4o kind = %q, want preserved responses", mc.Kind)
			}
		case "gpt-5":
			if mc.Kind != "" {
				t.Errorf("gpt-5 kind = %q, want empty (provider default)", mc.Kind)
			}
		}
	}
}

// 日志完整度分级:default 模式下成功请求只记基础字段(不含 forward_*),
// 出错请求记录完整转发详情。
func TestLogDetailDefaultFiltersSuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler()) // 默认 default
	defer srv.Close()

	// 成功请求:只记基础字段,无 forward_*。
	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Status != 200 {
		t.Errorf("status = %d, want 200", entries[0].Status)
	}
	if entries[0].ForwardURL != "" || entries[0].ForwardRequest != "" || entries[0].ForwardResponse != "" {
		t.Errorf("default success should not record forward detail: %+v", entries[0])
	}
	if entries[0].RequestBody != "" || entries[0].ConvertedResponseBody != "" {
		t.Errorf("default success should not record bodies: %+v", entries[0])
	}

	// 出错请求:记录完整转发详情。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer bad.Close()
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "bad", BaseURL: bad.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	// 注意:bad 供应商模型 gpt-4o 支持 completion → 直通;直通清空 request_body,
	// 只记 forward_*(发给上游/上游返回)。这里验证 default 下出错时直通也记 forward_*。
	resp2, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"bad@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	entries2 := readLogEntries(t, logPath)
	if len(entries2) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries2))
	}
	e := entries2[1] // readLogEntries 顺序读,最新在末尾
	if e.Status != 502 || e.Error == "" {
		t.Errorf("error entry = %+v", e)
	}
	if e.ForwardURL == "" || e.ForwardRequest == "" || e.ForwardResponse == "" {
		t.Errorf("default error should record forward detail: %+v", e)
	}
	// 直通不记 request_body(用户确认直通只记 forward_*)。
	if e.RequestBody != "" {
		t.Errorf("direct path should NOT record request_body: %+v", e.RequestBody)
	}
}

// 完整模式(full):成功请求也记录 request_body / forward_* / converted_response_body。
// 用跨格式转换路径(anthropic 客户端 → completion 上游)验证 4 个字段都记录。
func TestLogDetailFullRecordsEverything(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithLogger(lg).WithLogDetail(LogDetailFull).Handler())
	defer srv.Close()

	// anthropic 格式 → completion 上游:模型只支持 completion,走转换路径。
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.RequestBody == "" || !strings.Contains(e.RequestBody, `"model":"oa@gpt-4o"`) {
		t.Errorf("full should record request_body: %q", e.RequestBody)
	}
	if e.ForwardURL == "" || e.ForwardRequest == "" || e.ForwardResponse == "" {
		t.Errorf("full should record forward detail: %+v", e)
	}
	// 转换后回客户端的是 anthropic 格式(客户端发 anthropic 请求)。
	if e.ConvertedResponseBody == "" || !strings.Contains(e.ConvertedResponseBody, `"text":"hi"`) {
		t.Errorf("full should record converted_response_body: %q", e.ConvertedResponseBody)
	}
}

// 日志完整度管理端点:GET 返回当前级别,PUT 设置并持久化,重启后读持久化值。
func TestLogDetailEndpoint(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	detailFile := filepath.Join(t.TempDir(), "logdetail.json")

	m := newMgr(t)
	srv := httptest.NewServer(New(m).WithLogger(lg).WithLogDetailPath(detailFile).Handler())
	defer srv.Close()

	// 默认 default。
	resp, body := doJSON(t, srv, http.MethodGet, "/manage/v1/logs/detail", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"detail":"default"`) {
		t.Errorf("GET detail = %s", body)
	}
	// 非法值拒绝。
	resp, _ = doJSON(t, srv, http.MethodPut, "/manage/v1/logs/detail", `{"detail":"nope"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT invalid status = %d, want 400", resp.StatusCode)
	}
	// 设置 full。
	resp, body = doJSON(t, srv, http.MethodPut, "/manage/v1/logs/detail", `{"detail":"full"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"detail":"full"`) {
		t.Fatalf("PUT full = %d; %s", resp.StatusCode, body)
	}
	// 持久化文件已写。
	if data, err := os.ReadFile(detailFile); err != nil || !strings.Contains(string(data), `"full"`) {
		t.Errorf("persisted file = %q, err=%v", data, err)
	}
	// 新 Server 读持久化文件(经 WithLogDetailPath + 重新构造需手动加载——这里验证持久化文件内容即可)。
}
