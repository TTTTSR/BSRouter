package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/codex"
	"BSRouter/internal/group"
	"BSRouter/internal/provider"
)

// newNativeAliasEnv 构造网关:供应商 up(格式 kind、模型 target)+ codex 预设
// Models=["up@target"]。自动分配把该模型绑定到原生 id 池第 0 个 slug。
// 返回网关、上游收到的 model 捕获指针、自动分配的 slug。
func newNativeAliasEnv(t *testing.T, kind provider.Kind, target string) (*httptest.Server, *string, string) {
	t.Helper()
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream: %v", err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case "/v1/responses":
			fmt.Fprint(w, `{"id":"r1","object":"response","model":"gpt-4o","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}))
	t.Cleanup(upstream.Close)

	mgr := newMgr(t)
	if err := mgr.Add(provider.Config{
		Kind: kind, Name: "up", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: target}},
	}); err != nil {
		t.Fatal(err)
	}
	cm := newCodexMgr(t)
	if err := cm.Add(codex.Config{Name: "test", Models: []string{"up@" + target}}); err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(New(mgr).WithCodexPresets(cm).Handler())
	t.Cleanup(gs.Close)
	return gs, &gotModel, codex.NativeOpenAISlugs()[0]
}

// 裸原生 slug 同格式直通:上游收到绑定模型名,响应回填原生 slug。
func TestNativeAliasDirectResponses(t *testing.T) {
	gs, gotModel, slug := newNativeAliasEnv(t, provider.KindResponses, "gpt-4o")
	body := fmt.Sprintf(`{"model":%q,"input":"hi"}`, slug)
	resp, err := http.Post(gs.URL+"/api/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body=%s", resp.StatusCode, rb)
	}
	if *gotModel != "gpt-4o" {
		t.Errorf("upstream received model = %q, want gpt-4o (bound target)", *gotModel)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != slug {
		t.Errorf("response model = %q, want %q (echo native slug)", out.Model, slug)
	}
}

// 跨格式:responses 客户端请求原生 slug,绑定模型是 anthropic → 转换路径转发。
func TestNativeAliasConversion(t *testing.T) {
	gs, gotModel, slug := newNativeAliasEnv(t, provider.KindAnthropic, "claude-sonnet-4-5")
	body := fmt.Sprintf(`{"model":%q,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, slug)
	resp, err := http.Post(gs.URL+"/api/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body=%s", resp.StatusCode, rb)
	}
	if *gotModel != "claude-sonnet-4-5" {
		t.Errorf("upstream received model = %q, want claude-sonnet-4-5", *gotModel)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != slug {
		t.Errorf("response model = %q, want %q (echo native slug)", out.Model, slug)
	}
}

// 未绑定的原生 slug(池中其它 slug)不落入 native alias → 普通解析 404。
func TestNativeAliasUnbound404(t *testing.T) {
	gs, _, _ := newNativeAliasEnv(t, provider.KindResponses, "gpt-4o")
	// 单个模型只占用池[0];请求池[1](gpt-5.4)未绑定 → 404。
	unbound := codex.NativeOpenAISlugs()[1]
	body := fmt.Sprintf(`{"model":%q,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, unbound)
	resp, err := http.Post(gs.URL+"/api/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unbound slug status = %d, want 404", resp.StatusCode)
	}
}

// 流式:裸原生 slug 同格式直通透传 SSE,上游收到绑定模型名,响应模型回填原生 slug。
func TestNativeAliasStreamDirect(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream: %v", err)
		}
		gotModel = body.Model
		if !body.Stream {
			t.Errorf("upstream stream = %v, want true", body.Stream)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n"+
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-sonnet-4-5\",\"content\":[]}}\n\n"+
			"event: content_block_delta\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"+
			"event: message_stop\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(upstream.Close)

	mgr := newMgr(t)
	if err := mgr.Add(provider.Config{
		Kind: provider.KindAnthropic, Name: "up", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "claude-sonnet-4-5"}},
	}); err != nil {
		t.Fatal(err)
	}
	cm := newCodexMgr(t)
	slug := codex.NativeOpenAISlugs()[0]
	if err := cm.Add(codex.Config{Name: "test", Models: []string{"up@claude-sonnet-4-5"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(mgr).WithCodexPresets(cm).Handler())
	t.Cleanup(srv.Close)

	reqBody := fmt.Sprintf(`{"model":%q,"max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, slug)
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotModel != "claude-sonnet-4-5" {
		t.Errorf("upstream model = %q, want claude-sonnet-4-5", gotModel)
	}
	data, _ := io.ReadAll(resp.Body)
	got := string(data)
	if !strings.Contains(got, `"model":"`+slug+`"`) {
		t.Errorf("response model not backfilled to %q:\n%s", slug, got)
	}
	if !strings.Contains(got, "event: message_stop") {
		t.Errorf("response missing message_stop:\n%s", got)
	}
}

// 分组路由:组内配置原生 slug,请求经分组 → native alias → 绑定模型转发。
func TestNativeAliasGroup(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream: %v", err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"r1","object":"response","model":"gpt-4o","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr := newMgr(t)
	if err := mgr.Add(provider.Config{
		Kind: provider.KindResponses, Name: "up", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}},
	}); err != nil {
		t.Fatal(err)
	}
	slug := codex.NativeOpenAISlugs()[0]
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team", Kind: provider.KindResponses, Models: []string{slug}}); err != nil {
		t.Fatal(err)
	}
	cm := newCodexMgr(t)
	if err := cm.Add(codex.Config{Name: "test", Models: []string{"up@gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(mgr).WithGroups(gm).WithCodexPresets(cm).Handler())
	t.Cleanup(srv.Close)

	body := fmt.Sprintf(`{"model":%q,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, slug)
	resp, rb := doJSON(t, srv, http.MethodPost, "/api/team/v1/responses", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, rb)
	}
	if gotModel != "gpt-4o" {
		t.Errorf("upstream model = %q, want gpt-4o", gotModel)
	}
	if !strings.Contains(rb, `"`+slug+`"`) {
		t.Errorf("group response model not backfilled:\n%s", rb)
	}
	// 组外模型 → 404(成员校验用原始模型)。
	resp2, _ := doJSON(t, srv, http.MethodPost, "/api/team/v1/responses",
		`{"model":"deepseek@deepseek-v4-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("group non-member status = %d, want 404", resp2.StatusCode)
	}
}

// resolveAliasedModel 单测:绑定返回路由模型、上下文标记保留、未绑定原样。
func TestResolveAliasedModel(t *testing.T) {
	cm := newCodexMgr(t)
	if err := cm.Add(codex.Config{Name: "a", Models: []string{"deepseek@deepseek-v4-pro"}}); err != nil {
		t.Fatal(err)
	}
	s := New(newMgr(t)).WithCodexPresets(cm)
	slug := codex.NativeOpenAISlugs()[0]
	if got := s.resolveAliasedModel(slug); got != "deepseek@deepseek-v4-pro" {
		t.Errorf("bound = %q, want deepseek@deepseek-v4-pro", got)
	}
	if got := s.resolveAliasedModel(slug + "[1M]"); got != "deepseek@deepseek-v4-pro[1M]" {
		t.Errorf("bound+marker = %q", got)
	}
	// 未绑定 slug / 合成 id 原样返回。
	if got := s.resolveAliasedModel(codex.NativeOpenAISlugs()[1]); got != codex.NativeOpenAISlugs()[1] {
		t.Errorf("unbound slug = %q", got)
	}
	if got := s.resolveAliasedModel("opencode-go@gpt-5.6-luna"); got != "opencode-go@gpt-5.6-luna" {
		t.Errorf("composite id = %q", got)
	}
}

// 自动分配:预设 Models 按排序依次占用原生 slug 池(每 slug 一模型),display_name
// 用模型 id;多预设取模型并集;池用尽即停。
func TestNativeAliasAutoAssign(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Name: "deepseek", Kind: provider.KindResponses, BaseURL: "http://ds",
		Models: []provider.ModelConfig{{Name: "deepseek-v4-pro"}, {Name: "deepseek-v4-flash"}},
	}); err != nil {
		t.Fatal(err)
	}
	cm := newCodexMgr(t)
	if err := cm.Add(codex.Config{Name: "a", Models: []string{
		"deepseek@deepseek-v4-flash", "deepseek@deepseek-v4-pro",
	}}); err != nil {
		t.Fatal(err)
	}
	s := New(m).WithCodexPresets(cm)

	aliases := s.nativeAliases()
	// 两个模型 → 池前两个 slug。
	if len(aliases) != 2 {
		t.Fatalf("auto aliases = %d, want 2; %+v", len(aliases), aliases)
	}
	pool := codex.NativeOpenAISlugs()
	if aliases[pool[0]].Model != "deepseek@deepseek-v4-flash" || aliases[pool[1]].Model != "deepseek@deepseek-v4-pro" {
		t.Errorf("auto mapping wrong: %+v", aliases)
	}
	for _, a := range aliases {
		if a.DisplayName != a.Model {
			t.Errorf("auto display_name = %q, want model id", a.DisplayName)
		}
	}
	// 确定性。
	a2 := s.nativeAliases()
	for k, v := range aliases {
		if a2[k] != v {
			t.Errorf("auto-assign not deterministic at %s", k)
		}
	}
}

// 多预设取模型并集:两预设各列不同模型 → 各自占用池中不同 slug。
func TestNativeAliasUnionAcrossPresets(t *testing.T) {
	cm := newCodexMgr(t)
	if err := cm.Add(codex.Config{Name: "a", Models: []string{"deepseek@m1"}}); err != nil {
		t.Fatal(err)
	}
	if err := cm.Add(codex.Config{Name: "b", Models: []string{"opencode-go@m2"}}); err != nil {
		t.Fatal(err)
	}
	s := New(newMgr(t)).WithCodexPresets(cm)
	aliases := s.nativeAliases()
	if len(aliases) != 2 {
		t.Fatalf("aliases = %d, want 2 (union); %+v", len(aliases), aliases)
	}
	pool := codex.NativeOpenAISlugs()
	// 排序:deepseek@m1 < opencode-go@m2 → pool[0]=deepseek@m1, pool[1]=opencode-go@m2。
	if aliases[pool[0]].Model != "deepseek@m1" || aliases[pool[1]].Model != "opencode-go@m2" {
		t.Errorf("union mapping wrong: %+v", aliases)
	}
}

// 预设 Models 留空时回退网关全部可路由模型(自动分配前 7 个,向后兼容)。
func TestNativeAliasFallbackAllModels(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Name: "deepseek", Kind: provider.KindResponses, BaseURL: "http://ds",
		Models: []provider.ModelConfig{{Name: "deepseek-v4-flash"}, {Name: "deepseek-v4-pro"}},
	}); err != nil {
		t.Fatal(err)
	}
	cm := newCodexMgr(t)
	if err := cm.Add(codex.Config{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	s := New(m).WithCodexPresets(cm)
	aliases := s.nativeAliases()
	// 回退全部模型:deepseek@deepseek-v4-flash 与 deepseek@deepseek-v4-pro。
	models := map[string]bool{}
	for _, a := range aliases {
		models[a.Model] = true
	}
	if !models["deepseek@deepseek-v4-flash"] || !models["deepseek@deepseek-v4-pro"] {
		t.Errorf("fallback aliases missing models: %+v", aliases)
	}
}

// apply-local 目录生成:预设直接配置 Models 后,bsrouter-models.json 与
// models_cache.json 发布模型行 + 自动分配的裸原生 slug 行;base_url 留空用默认。
func TestApplyCodexPresetLocalNativeAlias(t *testing.T) {
	cm := newCodexMgr(t)
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Name: "openai", Kind: "responses", BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	catPath := filepath.Join(dir, ".codex", "bsrouter-models.json")
	cachePath := filepath.Join(dir, ".codex", "models_cache.json")
	configPath := filepath.Join(dir, ".codex", "config.toml")
	s := New(m).WithCodexPresets(cm).WithCodexModelCatalogPath(catPath).WithCodexModelsCachePath(cachePath).WithCodexConfigPath(configPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 非法:超过原生池大小(8)个模型 → 400。
	nine := `["p@1","p@2","p@3","p@4","p@5","p@6","p@7","p@8","p@9"]`
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"bad","models":`+nine+`}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf(">pool models status = %d, want 400; body=%s", resp.StatusCode, b)
	}

	// 合法:直接配置模型列表,base_url 留空。
	body := `{"name":"dev","models":["openai@gpt-4o"]}`
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets", body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, b)
	}
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets/dev/apply-local", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}

	// 目录:模型行 + 自动原生行(slug=池[0], display=openai@gpt-4o)。
	catData, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	cat := string(catData)
	slug := codex.NativeOpenAISlugs()[0]
	if !strings.Contains(cat, `"openai@gpt-4o"`) {
		t.Errorf("catalog missing model row:\n%s", cat)
	}
	if !strings.Contains(cat, `"`+slug+`"`) || !strings.Contains(cat, `"display_name": "openai@gpt-4o"`) {
		t.Errorf("catalog missing auto native row:\n%s", cat)
	}
	cacheData, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read models_cache: %v", err)
	}
	if !strings.Contains(string(cacheData), `"`+slug+`"`) {
		t.Errorf("models_cache missing native row:\n%s", cacheData)
	}
	// base_url 留空 → config.toml 用网关默认统一 API 入口(127.0.0.1:18154)。
	cfgData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if !strings.Contains(string(cfgData), `base_url = "http://127.0.0.1:18154/api/v1"`) {
		t.Errorf("default base_url missing:\n%s", cfgData)
	}
}
