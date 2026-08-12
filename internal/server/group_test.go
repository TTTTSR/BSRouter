package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/gateway"
	"BSRouter/internal/group"
	"BSRouter/internal/logger"
	"BSRouter/internal/provider"
)

// newGroupMgr 构造一个分组管理器(空配置)。
func newGroupMgr(t *testing.T) *group.Manager {
	t.Helper()
	gm, err := group.NewManager(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	return gm
}

// TestGroupCompletionForward:分组以 completion 格式对外,模型路由到真实供应商。
func TestGroupCompletionForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"openai@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithGroups(gm).Handler())
	defer srv.Close()

	resp, body := doJSON(t, srv, http.MethodPost, "/api/team-a/v1/chat/completions",
		`{"model":"openai@gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	var out gateway.CompletionResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "openai@gpt-4o" {
		t.Errorf("response model = %q, want group model id", out.Model)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "hi" {
		t.Errorf("choices = %+v", out.Choices)
	}
}

// 跨格式:completion 格式的分组,模型路由到 anthropic 上游。
func TestGroupCrossFormat(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-5",`+
			`"content":[{"type":"text","text":"hi-from-claude"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindAnthropic, Name: "an", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"an@claude-sonnet-4-5"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithGroups(gm).Handler())
	defer srv.Close()

	// 客户端以 completion 格式调用,网关负责转成 anthropic 上游格式。
	resp, body := doJSON(t, srv, http.MethodPost, "/api/team-a/v1/chat/completions",
		`{"model":"an@claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	var out gateway.CompletionResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "hi-from-claude" {
		t.Errorf("cross-format choices = %+v", out.Choices)
	}
}

func TestGroupAnthropicForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-5",`+
			`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindAnthropic, Name: "an", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "ta", Kind: provider.KindAnthropic, Models: []string{"an@claude-sonnet-4-5"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithGroups(gm).Handler())
	defer srv.Close()

	resp, body := doJSON(t, srv, http.MethodPost, "/api/ta/v1/messages",
		`{"model":"an@claude-sonnet-4-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
	}
	var out gateway.AnthropicResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "hello" {
		t.Errorf("content = %+v", out.Content)
	}
}

func TestGroupModelsEndpoint(t *testing.T) {
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"openai@gpt-5", "openai@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithGroups(gm).Handler())
	defer srv.Close()

	resp, body := doJSON(t, srv, http.MethodGet, "/api/team-a/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out modelList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 || out.Data[0].ID != "openai@gpt-4o" || out.Data[1].ID != "openai@gpt-5" {
		t.Errorf("data = %+v", out.Data)
	}
}

func TestGroupModelNotAssigned(t *testing.T) {
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"openai@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithGroups(gm).Handler())
	defer srv.Close()

	resp, _ := doJSON(t, srv, http.MethodPost, "/api/team-a/v1/chat/completions",
		`{"model":"other@gpt-4o","messages":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGroupWrongFormatEndpoint(t *testing.T) {
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"openai@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithGroups(gm).Handler())
	defer srv.Close()

	// completion 分组不接受 /api/v1/messages。
	resp, _ := doJSON(t, srv, http.MethodPost, "/api/team-a/v1/messages", `{"model":"x","messages":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("wrong-format status = %d, want 404", resp.StatusCode)
	}
}

func TestGroupMethodNotAllowed(t *testing.T) {
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"openai@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithGroups(gm).Handler())
	defer srv.Close()

	resp, _ := doJSON(t, srv, http.MethodGet, "/api/team-a/v1/chat/completions", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestGroupUnknownURL(t *testing.T) {
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithGroups(gm).Handler())
	defer srv.Close()

	resp, _ := doJSON(t, srv, http.MethodGet, "/api/nope/v1/models", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown group url status = %d, want 404", resp.StatusCode)
	}
}

func TestGroupCRUD(t *testing.T) {
	gm := newGroupMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithGroups(gm).Handler())
	defer srv.Close()

	// 新增 -> 201,返回归一化的默认 URL。
	resp, body := doJSON(t, srv, http.MethodPost, "/manage/v1/groups",
		`{"name":"team-a","kind":"completion","models":["openai@gpt-4o"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, body)
	}
	var created group.Config
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.URL != "/api/team-a" {
		t.Errorf("default url = %q, want /api/team-a", created.URL)
	}

	// 重复 -> 409;非法 -> 400。
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/groups", `{"name":"team-a","kind":"completion","models":["x"]}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", resp.StatusCode)
	}
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/groups", `{"name":"x","kind":"nope","models":[]}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid status = %d, want 400", resp.StatusCode)
	}

	// 列表与单查。
	if resp, _ := doJSON(t, srv, http.MethodGet, "/manage/v1/groups", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("list status = %d", resp.StatusCode)
	}
	if resp, _ := doJSON(t, srv, http.MethodGet, "/manage/v1/groups/team-a", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("get status = %d", resp.StatusCode)
	}
	if resp, _ := doJSON(t, srv, http.MethodGet, "/manage/v1/groups/missing", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing status = %d", resp.StatusCode)
	}

	// 修改 -> 200。
	if resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/groups/team-a",
		`{"kind":"anthropic","models":["an@claude-sonnet-4-5"]}`); resp.StatusCode != http.StatusOK {
		t.Errorf("put status = %d", resp.StatusCode)
	}
	if g, _ := gm.Get("team-a"); g.Kind != "anthropic" {
		t.Errorf("after update kind = %q", g.Kind)
	}

	// 删除 -> 204,再删 -> 404。
	if resp, _ := doJSON(t, srv, http.MethodDelete, "/manage/v1/groups/team-a", ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d", resp.StatusCode)
	}
	if resp, _ := doJSON(t, srv, http.MethodDelete, "/manage/v1/groups/team-a", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete again status = %d", resp.StatusCode)
	}
}

func TestGroupAuthRequired(t *testing.T) {
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKey("secret").WithGroups(gm).Handler())
	defer srv.Close()

	// 无 key 访问分组转发端点(受保护,非公开模型列表)-> 401。
	resp, _ := doJSON(t, srv, http.MethodPost, "/api/team-a/v1/chat/completions", `{"model":"x"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-key group status = %d, want 401", resp.StatusCode)
	}
}

func TestGroupLogging(t *testing.T) {
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
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: up.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"openai@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithGroups(gm).WithLogger(lg).WithLogDetail(LogDetailFull).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/team-a/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"openai@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Model != "openai@gpt-4o" || entries[0].Provider != "team-a→openai" || entries[0].Kind != "completion" {
		t.Errorf("entry = %+v", entries[0])
	}
	if !strings.Contains(entries[0].ForwardURL, "/v1/chat/completions") {
		t.Errorf("group forward_url = %q", entries[0].ForwardURL)
	}
	if !strings.Contains(entries[0].ForwardRequest, `"model":"gpt-4o"`) {
		t.Errorf("group forward_request = %q", entries[0].ForwardRequest)
	}
	if !strings.Contains(entries[0].ForwardResponse, `"content":"hi"`) {
		t.Errorf("group forward_response = %q", entries[0].ForwardResponse)
	}
}

// 部分更新省略 url 时,应保留自定义 url。
func TestGroupUpdatePreservesURL(t *testing.T) {
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, URL: "/api/internal/team-a", Models: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithGroups(gm).Handler())
	defer srv.Close()

	resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/groups/team-a",
		`{"kind":"completion","models":["openai@gpt-5"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}
	g, _ := gm.Get("team-a")
	if g.URL != "/api/internal/team-a" {
		t.Errorf("url after partial PUT = %q, want preserved /api/internal/team-a", g.URL)
	}
	if len(g.Models) != 1 || g.Models[0] != "openai@gpt-5" {
		t.Errorf("models after PUT = %v", g.Models)
	}
}

// 名称为保留前缀的分组应被拒绝(默认 URL 也走校验)。
func TestGroupNameReservedRejected(t *testing.T) {
	gm := newGroupMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithGroups(gm).Handler())
	defer srv.Close()

	resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/groups",
		`{"name":"v1","kind":"completion","models":["x"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST name=v1 status = %d, want 400 (reserved url)", resp.StatusCode)
	}
}

// 上游错误体回显 api_key 时,JSONL 日志也不应包含密钥。
func TestGroupLogRedaction(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"bad key: Bearer SECRETKEY123"}`)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: up.URL, APIKey: "SECRETKEY123"}); err != nil {
		t.Fatal(err)
	}
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"openai@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithGroups(gm).WithLogger(lg).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/team-a/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"openai@gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 响应应 502 且不含密钥。
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	// 日志条目应含已抹除密钥的错误。
	entries := readLogEntries(t, logPath)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if strings.Contains(entries[0].Error, "SECRETKEY123") {
		t.Errorf("log leaked api_key: %s", entries[0].Error)
	}
	if !strings.Contains(entries[0].Error, "***") {
		t.Errorf("log error should contain redaction marker: %s", entries[0].Error)
	}
	if entries[0].UpstreamStatus != 401 {
		t.Errorf("upstream_status = %d, want 401", entries[0].UpstreamStatus)
	}
}
