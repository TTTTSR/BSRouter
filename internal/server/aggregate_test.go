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

	"BSRouter/internal/aggregate"
	"BSRouter/internal/group"
	"BSRouter/internal/provider"
)

func newAggMgr(t *testing.T, pm *provider.Manager) *aggregate.Manager {
	t.Helper()
	am, err := aggregate.NewManager(filepath.Join(t.TempDir(), "aggregates.json"), pm)
	if err != nil {
		t.Fatal(err)
	}
	return am
}

func addProvider(t *testing.T, pm *provider.Manager, name string, models ...string) {
	t.Helper()
	cfgs := make([]provider.ModelConfig, 0, len(models))
	for _, m := range models {
		cfgs = append(cfgs, provider.ModelConfig{Name: m})
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: name, BaseURL: "http://x", Models: cfgs}); err != nil {
		t.Fatal(err)
	}
}

func TestAggregatesEndpoints(t *testing.T) {
	pm := newMgr(t)
	addProvider(t, pm, "openai", "gpt-4o")
	addProvider(t, pm, "azure", "gpt-4o")
	addProvider(t, pm, "deepseek", "deepseek-chat")
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	// 列表。
	resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/aggregates", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d; body=%s", resp.StatusCode, b)
	}
	var list []aggregate.Model
	if err := json.Unmarshal([]byte(b), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 { // gpt-4o, deepseek-chat
		t.Fatalf("list = %+v", list)
	}
	g := list[0]
	if g.Name != "deepseek-chat" { // 排序:deepseek-chat < gpt-4o
		t.Fatalf("list[0] = %+v", list[0])
	}
	if len(g.Members) != 1 || g.Members[0] != "deepseek" {
		t.Errorf("deepseek-chat members = %v", g.Members)
	}

	// PUT 剔除 openai,只留 azure。
	if resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/aggregates/gpt-4o", `{"members":["azure"]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}
	resp, b = doJSON(t, srv, http.MethodGet, "/manage/v1/aggregates", "")
	var list2 []aggregate.Model
	if err := json.Unmarshal([]byte(b), &list2); err != nil {
		t.Fatal(err)
	}
	var g2 aggregate.Model
	for _, m := range list2 {
		if m.Name == "gpt-4o" {
			g2 = m
		}
	}
	if len(g2.Members) != 1 || g2.Members[0] != "azure" {
		t.Errorf("after exclude members = %v", g2.Members)
	}
	if len(g2.Available) != 1 || g2.Available[0] != "openai" {
		t.Errorf("after exclude available = %v, want [openai]", g2.Available)
	}

	// PUT 非法成员(deepseek 无 gpt-4o)→ 400。
	if resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/aggregates/gpt-4o", `{"members":["deepseek"]}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid member status = %d, want 400", resp.StatusCode)
	}
	// PUT 未知模型 → 404。
	if resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/aggregates/no-such", `{"members":["openai"]}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown model status = %d, want 404", resp.StatusCode)
	}
	// 非法 JSON 体 → 400。
	if resp, _ := doJSON(t, srv, http.MethodPut, "/manage/v1/aggregates/gpt-4o", `{"members":`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json status = %d, want 400", resp.StatusCode)
	}
}

func TestListModelsIncludesAggregates(t *testing.T) {
	pm := newMgr(t)
	addProvider(t, pm, "openai", "gpt-4o", "gpt-5")
	addProvider(t, pm, "azure", "gpt-4o")
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	for _, path := range []string{"/api/v1/models", "/manage/v1/models"} {
		resp, b := doJSON(t, srv, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, resp.StatusCode)
		}
		var out modelList
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(out.Data))
		byID := map[string]string{}
		for i, e := range out.Data {
			got[i] = e.ID
			byID[e.ID] = e.OwnedBy
		}
		want := []string{"azure@gpt-4o", "gpt-4o", "gpt-5", "openai@gpt-4o", "openai@gpt-5"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s models = %v, want %v", path, got, want)
		}
		if byID["gpt-4o"] != "unified" || byID["openai@gpt-4o"] != "openai" {
			t.Errorf("owned_by = %v", byID)
		}
	}
}

// 轮询端到端:两个上游各含 gpt-4o,连续请求 model=gpt-4o 应交替到达。
func TestAggregateRoundRobin(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	mkUp := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			counts[name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
	}
	up1, up2 := mkUp("openai"), mkUp("azure")
	defer up1.Close()
	defer up2.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: up1.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "azure", BaseURL: up2.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 4; i++ {
		resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d", i, resp.StatusCode)
		}
	}
	if counts["openai"] != 2 || counts["azure"] != 2 {
		t.Errorf("round-robin counts = %v, want openai=2 azure=2", counts)
	}
}

// 聚合轮询收到带 [1M] 上下文标记的模型名时,先剥离再路由。
func TestAggregateRoundRobinWithMarker(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	mkUp := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			counts[name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
	}
	up1, up2 := mkUp("openai"), mkUp("azure")
	defer up1.Close()
	defer up2.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: up1.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "azure", BaseURL: up2.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	// model 带 [1M]:网关先剥标记,再按聚合轮询转发。
	body := `{"model":"gpt-4o[1M]","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 4; i++ {
		resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d", i, resp.StatusCode)
		}
	}
	if counts["openai"] != 2 || counts["azure"] != 2 {
		t.Errorf("round-robin counts = %v, want openai=2 azure=2", counts)
	}
}

// 故障转移:聚合首个成员返回 500,自动切换到其余成员;成功后失败成员被冷却,
// 后续请求不再轮到它。
func TestAggregateFailover(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	mkUp := func(name, resp string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			counts[name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, resp)
		}))
	}
	badUp := mkUp("aup", `{"error":{"message":"boom","type":"server_error"}}`, http.StatusInternalServerError)
	defer badUp.Close()
	goodUp := mkUp("bup",
		`{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		http.StatusOK)
	defer goodUp.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "aup", BaseURL: badUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "bup", BaseURL: goodUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 3; i++ {
		resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
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
	// 首个请求 aup 失败后转移到 bup,随后 aup 被冷却,不再被轮到。
	if counts["aup"] != 1 {
		t.Errorf("failed provider hits = %d, want 1 (banned after first failover)", counts["aup"])
	}
	if counts["bup"] != 3 {
		t.Errorf("healthy provider hits = %d, want 3", counts["bup"])
	}
}

// 全部成员失败:返回 502,且不冷却任何成员(无法区分供应商问题与请求问题),下次仍尝试全部。
func TestAggregateFailoverAllFail(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	mkUp := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			counts[name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom","type":"server_error"}}`)
		}))
	}
	up1, up2 := mkUp("aup"), mkUp("bup")
	defer up1.Close()
	defer up2.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "aup", BaseURL: up1.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "bup", BaseURL: up2.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
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
	// 两次请求各尝试两个成员,全部失败不冷却。
	if counts["aup"] != 2 || counts["bup"] != 2 {
		t.Errorf("counts = %v, want aup=2 bup=2 (no ban on total failure)", counts)
	}
}

// 流式故障转移:首个成员流式请求非 2xx,切换到其余成员;成功后失败成员被冷却。
func TestAggregateFailoverStream(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	badUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts["aup"]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom","type":"server_error"}}`)
	}))
	defer badUp.Close()
	goodUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts["bup"]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
				`data: [DONE]`+"\n\n")
	}))
	defer goodUp.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "aup", BaseURL: badUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "bup", BaseURL: goodUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, resp.StatusCode)
		}
		if !strings.Contains(string(data), `"content":"hi"`) {
			t.Errorf("request %d SSE missing delta passthrough:\n%s", i, data)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// 首个流式请求 aup 失败后转移到 bup,随后 aup 被冷却。
	if counts["aup"] != 1 {
		t.Errorf("failed provider stream hits = %d, want 1 (banned after failover)", counts["aup"])
	}
	if counts["bup"] != 2 {
		t.Errorf("healthy provider stream hits = %d, want 2", counts["bup"])
	}
}

// 分组内 models 含聚合裸名,请求走分组时轮询转发。
func TestGroupAggregateRouting(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "openai", BaseURL: up.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	gm, err := group.NewManager(filepath.Join(t.TempDir(), "g.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(pm).WithGroups(gm).WithAggregates(am).Handler())
	defer srv.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/team-a/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("group status = %d", resp.StatusCode)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "gpt-4o" {
		t.Errorf("response model = %q, want gpt-4o", out.Model)
	}
}

// 分组内聚合模型故障转移:组 model 为聚合裸名,首个成员 500,分组路由切换到其余成员。
func TestGroupAggregateFailover(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	mkUp := func(name, resp string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			counts[name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, resp)
		}))
	}
	badUp := mkUp("aup", `{"error":{"message":"boom","type":"server_error"}}`, http.StatusInternalServerError)
	defer badUp.Close()
	goodUp := mkUp("bup",
		`{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		http.StatusOK)
	defer goodUp.Close()

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "aup", BaseURL: badUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "bup", BaseURL: goodUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	gm, err := group.NewManager(filepath.Join(t.TempDir(), "g.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(pm).WithGroups(gm).WithAggregates(am).Handler())
	defer srv.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/team-a/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("group status = %d, want 200 (failover)", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["aup"] != 1 || counts["bup"] != 1 {
		t.Errorf("counts = %v, want aup=1 bup=1 (failover in group)", counts)
	}
}
