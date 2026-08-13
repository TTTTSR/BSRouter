package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/codex"
	"BSRouter/internal/fault"
	"BSRouter/internal/group"
	"BSRouter/internal/provider"
)

// addWindowedProvider 添加一个含多模型(名字 → 上下文窗口 k)的供应商,窗口 0 表示未配置。
func addWindowedProvider(t *testing.T, pm *provider.Manager, name string, models map[string]int) {
	t.Helper()
	cfgs := make([]provider.ModelConfig, 0, len(models))
	for m, k := range models {
		cfgs = append(cfgs, provider.ModelConfig{Name: m, ContextWindow: k})
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: name, BaseURL: "http://x", Models: cfgs}); err != nil {
		t.Fatal(err)
	}
}

// 聚合裸名的上下文窗口 = 有效成员配置窗口的最小值;未配置成员按默认 200k 参与取小;
// 全部未配置返回 0;剔除成员不参与;非聚合/聚合未启用返回 0。
func TestModelContextWindowKAggregate(t *testing.T) {
	pm := newMgr(t)
	addWindowedProvider(t, pm, "openai", map[string]int{"gpt-4o": 128})
	addWindowedProvider(t, pm, "azure", map[string]int{"gpt-4o": 64}) // 更小,应被取到
	addWindowedProvider(t, pm, "big", map[string]int{"big-model": 1000})
	addWindowedProvider(t, pm, "plain", map[string]int{"big-model": 0}) // 未配置 → 200k 上界
	am := newAggMgr(t, pm)
	s := New(pm).WithAggregates(am)

	cases := []struct {
		in   string
		want int
	}{
		{"gpt-4o", 64},         // min(128, 64)
		{"gpt-4o[1M]", 64},     // 先剥标记再解析
		{"big-model", 200},     // min(1000, 未配置→200) = 200
		{"nope", 0},            // 非聚合裸名
		{"", 0},
	}
	for _, c := range cases {
		if got := s.modelContextWindowK(c.in); got != c.want {
			t.Errorf("modelContextWindowK(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	// 剔除 azure(64)后仅 openai(128) 为有效成员 → 128(剔除成员不拖低窗口)。
	if err := am.SetMembers("gpt-4o", []string{"openai"}); err != nil {
		t.Fatal(err)
	}
	if got := s.modelContextWindowK("gpt-4o"); got != 128 {
		t.Errorf("after exclude modelContextWindowK(gpt-4o) = %d, want 128", got)
	}

	// 全部成员未配置 → 0(调用方按默认 200k 处理,与单模型一致)。
	pm2 := newMgr(t)
	addWindowedProvider(t, pm2, "a", map[string]int{"flash": 0})
	addWindowedProvider(t, pm2, "b", map[string]int{"flash": 0})
	am2 := newAggMgr(t, pm2)
	s2 := New(pm2).WithAggregates(am2)
	if got := s2.modelContextWindowK("flash"); got != 0 {
		t.Errorf("all-unconfigured modelContextWindowK(flash) = %d, want 0", got)
	}

	// 聚合未启用(无 WithAggregates)→ 0,兼容旧行为。
	s3 := New(pm)
	if got := s3.modelContextWindowK("gpt-4o"); got != 0 {
		t.Errorf("no-aggregates modelContextWindowK(gpt-4o) = %d, want 0", got)
	}
}

// 故障被禁用的成员当前不可路由,不参与取小(与 faultFilteredOrder 的路由可及集合一致)。
func TestModelContextWindowKBlockedMemberExcluded(t *testing.T) {
	pm := newMgr(t)
	addWindowedProvider(t, pm, "openai", map[string]int{"gpt-4o": 64})
	addWindowedProvider(t, pm, "azure", map[string]int{"gpt-4o": 1000})
	am := newAggMgr(t, pm)
	fm, err := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	if err != nil {
		t.Fatal(err)
	}
	s := New(pm).WithAggregates(am).WithFaults(fm)

	// 未阻塞:min(64, 1000) = 64。
	if got := s.modelContextWindowK("gpt-4o"); got != 64 {
		t.Errorf("pre-block modelContextWindowK(gpt-4o) = %d, want 64", got)
	}
	// 阻塞 openai(64,余额不足):只剩 azure(1000) 可路由 → 1000,小窗口成员不再拖低窗口。
	fm.Record(fault.Input{Error: "Insufficient Balance", Provider: "openai", Upstream: true})
	if _, blocked := fm.Block("openai"); !blocked {
		t.Fatal("openai should be blocked")
	}
	if got := s.modelContextWindowK("gpt-4o"); got != 1000 {
		t.Errorf("post-block modelContextWindowK(gpt-4o) = %d, want 1000", got)
	}
}

// 分组 /v1/models 列表与统一列表一致:聚合裸名条目携带 context_window = 成员最小值。
func TestGroupModelsAggregateContextWindow(t *testing.T) {
	pm := newMgr(t)
	addWindowedProvider(t, pm, "openai", map[string]int{"gpt-4o": 128})
	addWindowedProvider(t, pm, "azure", map[string]int{"gpt-4o": 1000})
	am := newAggMgr(t, pm)
	gm := newGroupMgr(t)
	if err := gm.Add(group.Config{Name: "team-a", Kind: provider.KindCompletion, Models: []string{"gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(pm).WithGroups(gm).WithAggregates(am).Handler())
	defer srv.Close()

	resp, b := doJSON(t, srv, http.MethodGet, "/api/team-a/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, b)
	}
	if !strings.Contains(b, `"id":"gpt-4o","object":"model","owned_by":"team-a","context_window":128`) {
		t.Errorf("group list missing aggregate min context_window:\n%s", b)
	}
}

// 模型列表:聚合裸名条目携带 context_window = 成员最小值(k),直通/合成条目不受影响。
func TestListModelsAggregateContextWindow(t *testing.T) {
	pm := newMgr(t)
	addWindowedProvider(t, pm, "openai", map[string]int{"gpt-4o": 128})
	addWindowedProvider(t, pm, "azure", map[string]int{"gpt-4o": 1000})
	addWindowedProvider(t, pm, "deepseek", map[string]int{"plain": 0})
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).Handler())
	defer srv.Close()

	resp, b := doJSON(t, srv, http.MethodGet, "/api/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, b)
	}
	if !strings.Contains(b, `"id":"gpt-4o","object":"model","owned_by":"unified","context_window":128`) {
		t.Errorf("aggregate entry missing min context_window:\n%s", b)
	}
	// 未配置窗口的聚合(plain,成员全未配置)不发布 context_window。
	if strings.Contains(b, `"id":"plain","object":"model","owned_by":"unified","context_window"`) {
		t.Errorf("all-unconfigured aggregate should omit context_window:\n%s", b)
	}
}

// Claude 预设命令:聚合裸名 model 按成员最小值追加 [Nk] 后缀。
func TestClaudeCommandAggregateContextSuffix(t *testing.T) {
	pm := newMgr(t)
	addWindowedProvider(t, pm, "openai", map[string]int{"gpt-4o": 128})
	addWindowedProvider(t, pm, "azure", map[string]int{"gpt-4o": 1000})
	am := newAggMgr(t, pm)
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(pm).WithAggregates(am).WithClaudePresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()

	body := `{"name":"dev","base_url":"http://127.0.0.1:8080/api","model":"gpt-4o"}`
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets", body, "admin"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev/command", "", "admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status = %d; body=%s", resp.StatusCode, b)
	}
	var cmd struct {
		PowerShell string `json:"powershell"`
		Bash       string `json:"bash"`
	}
	if err := json.Unmarshal([]byte(b), &cmd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.PowerShell, `$env:ANTHROPIC_MODEL = "gpt-4o[128k]"`) {
		t.Errorf("PowerShell missing min suffix:\n%s", cmd.PowerShell)
	}
	if !strings.Contains(cmd.Bash, `export ANTHROPIC_MODEL="gpt-4o[128k]"`) {
		t.Errorf("Bash missing min suffix:\n%s", cmd.Bash)
	}
}

// Codex 模型目录:聚合裸名条目 context_window 取成员最小值换算的 tokens。
func TestCodexCatalogAggregateContextWindow(t *testing.T) {
	cm := newCodexMgr(t)
	pm := newMgr(t)
	addWindowedProvider(t, pm, "openai", map[string]int{"gpt-4o": 128})
	addWindowedProvider(t, pm, "azure", map[string]int{"gpt-4o": 1000})
	am := newAggMgr(t, pm)
	dir := t.TempDir()
	catPath := filepath.Join(dir, ".codex", "bsrouter-models.json")
	cachePath := filepath.Join(dir, ".codex", "models_cache.json")
	configPath := filepath.Join(dir, ".codex", "config.toml")
	authPath := filepath.Join(dir, ".codex", "auth.json")
	s := New(pm).WithAggregates(am).WithCodexPresets(cm).
		WithCodexConfigPath(configPath).WithCodexAuthPath(authPath).
		WithCodexModelCatalogPath(catPath).WithCodexModelsCachePath(cachePath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api/v1","models":["gpt-4o"]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets/dev/apply-local", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatal(err)
	}
	var cat codex.ModelCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		t.Fatalf("catalog invalid: %v\n%s", err, data)
	}
	for _, e := range cat.Models {
		if e.DisplayName == "gpt-4o" {
			if e.ContextWindow != 128000 || e.MaxContextWindow != 128000 {
				t.Errorf("aggregate entry context_window = %d/%d, want 128000", e.ContextWindow, e.MaxContextWindow)
			}
			return
		}
	}
	t.Errorf("catalog missing aggregate gpt-4o:\n%s", data)
}
