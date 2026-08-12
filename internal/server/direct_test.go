package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"BSRouter/internal/group"
	"BSRouter/internal/logger"
	"BSRouter/internal/provider"
)

// 单供应商直通:模型支持客户端格式时,请求体原样转发(仅 model 改为裸名),
// 响应 model 回填为客户端请求的完整 id。
func TestDirectSingleProvider(t *testing.T) {
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o", Kinds: []provider.Kind{provider.KindCompletion}}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(pm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// 上游收到裸模型名;其余字段与客户端原样一致(直通,未经转换)。
	if gotBody["model"] != "gpt-4o" {
		t.Errorf("upstream model = %v, want gpt-4o (bare)", gotBody["model"])
	}
	if gotBody["temperature"] != 0.5 {
		t.Errorf("upstream temperature = %v, want 0.5 (raw passthrough)", gotBody["temperature"])
	}
	// 响应 model 回填为完整 id。
	var out struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "oa@gpt-4o" {
		t.Errorf("response model = %q, want oa@gpt-4o", out.Model)
	}
}

// 格式不匹配时回退转换路径:模型仅支持 anthropic,客户端发 completion → 网关转换后
// 打到 anthropic 端点(/v1/messages, x-api-key)。
func TestDirectFormatMismatchFallsBack(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages (conversion to anthropic)", r.URL.Path)
		}
		gotAuth = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"converted"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindAnthropic, Name: "oa", BaseURL: up.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "m"}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(pm).Handler())
	defer srv.Close()

	// completion 格式 → 模型只支持 anthropic,不走直通,转换后转发。
	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (conversion path)", resp.StatusCode)
	}
	if gotAuth == "" {
		t.Error("anthropic upstream should get x-api-key")
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), `"converted"`) {
		t.Errorf("response = %s", data)
	}
}

// 聚合直通:所有成员支持客户端格式,裸名整体直通;故障转移按优先级流转并冷却失败成员。
func TestDirectAggregateFailover(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	badUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts["azure"]++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer badUp.Close()
	goodUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts["openai"]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer goodUp.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "azure", BaseURL: badUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: goodUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	// 负载均衡默认关闭:固定优先级 [azure openai];azure 失败后切到 openai 并冷却 azure。
	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (failover)", i, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["azure"] != 1 {
		t.Errorf("azure hits = %d, want 1 (banned after first failover)", counts["azure"])
	}
	if counts["openai"] != 2 {
		t.Errorf("openai hits = %d, want 2", counts["openai"])
	}
}

// 直通聚合全部成员失败:返回 502,且不冷却任何成员(下次仍尝试全部)。
func TestDirectAggregateAllFailNoBan(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	mkUp := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			counts[name]++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"boom"}`)
		}))
	}
	up1, up2 := mkUp("azure"), mkUp("openai")
	defer up1.Close()
	defer up2.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "azure", BaseURL: up1.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: up2.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("request %d status = %d, want 502", i, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["azure"] != 2 || counts["openai"] != 2 {
		t.Errorf("counts = %v, want azure=2 openai=2 (no ban on total failure)", counts)
	}
}

// 直通流式:上游 SSE 逐事件透传,模型字段改写为客户端完整 id。
func TestDirectStreamModelRewrite(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
				`data: [DONE]`+"\n\n")
	}))
	defer up.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(pm).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(data), `"content":"hi"`) {
		t.Errorf("SSE missing delta passthrough:\n%s", data)
	}
	// 模型改写为完整 id;上游的裸名不再出现。
	if !strings.Contains(string(data), `"model":"oa@gpt-4o"`) {
		t.Errorf("SSE model not rewritten to full id:\n%s", data)
	}
}

// 聚合直通 + 上下文标记:裸名带 [1M],上游收到剥离标记的模型名。
func TestDirectAggregateStripsContextMarker(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotModel = b.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4o[1M]","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotModel != "gpt-4o" {
		t.Errorf("upstream model = %q, want gpt-4o (marker stripped)", gotModel)
	}
}

// 分组直通:组内模型支持组格式时走直通,响应 model 回填为组模型 id。
func TestDirectGroup(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotModel = b.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"oa@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(pm).WithGroups(gm).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/team-a/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotModel != "gpt-4o" {
		t.Errorf("upstream model = %q, want gpt-4o (bare)", gotModel)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "oa@gpt-4o" {
		t.Errorf("response model = %q, want oa@gpt-4o", out.Model)
	}
}

// 直通日志:转发详情(URL/请求/响应)与上游状态码都记录,且抹除 api_key。
func TestDirectLogging(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"bad key: SECRETKEY123"}`)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "SECRETKEY123", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(pm).WithLogger(lg).Handler())
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
	if e.UpstreamStatus != 401 {
		t.Errorf("upstream_status = %d, want 401", e.UpstreamStatus)
	}
	if e.Kind != "completion" {
		t.Errorf("kind = %q, want completion (client format)", e.Kind)
	}
	if e.ForwardURL == "" || !strings.Contains(e.ForwardURL, up.URL) {
		t.Errorf("forward_url = %q", e.ForwardURL)
	}
	if e.ForwardRequest == "" || !strings.Contains(e.ForwardRequest, `"model":"gpt-4o"`) {
		t.Errorf("forward_request = %q (should be bare model)", e.ForwardRequest)
	}
	if e.ForwardResponse == "" || strings.Contains(e.ForwardResponse, "SECRETKEY123") {
		t.Errorf("forward_response leaked key: %q", e.ForwardResponse)
	}
	if e.Error == "" || strings.Contains(e.Error, "SECRETKEY123") {
		t.Errorf("error should be populated and redacted: %q", e.Error)
	}
}
