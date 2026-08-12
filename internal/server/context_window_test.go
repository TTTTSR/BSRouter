package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/codex"
	"BSRouter/internal/provider"
)

// modelContextWindowK 按 "{供应商}@{模型}" 解析模型配置的上下文窗口(k);聚合裸名、
// 供应商/模型不存在或未配置时返回 0。
func TestModelContextWindowK(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Kind: provider.KindCompletion, Name: "up", BaseURL: "http://x",
		Models: []provider.ModelConfig{
			{Name: "flash", ContextWindow: 128},
			{Name: "plain"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	s := New(m)
	cases := map[string]int{
		"up@flash":            128,
		"up@flash[1M]":        128, // 先剥标记再解析
		"up@plain":            0,   // 未配置
		"nope@flash":          0,   // 供应商不存在
		"up@missing":          0,   // 模型不存在
		"gpt-4o":              0,   // 聚合裸名
		"":                    0,
	}
	for in, want := range cases {
		if got := s.modelContextWindowK(in); got != want {
			t.Errorf("modelContextWindowK(%q) = %d, want %d", in, got, want)
		}
	}
}

// Claude 预设命令:按各模型配置的上下文窗口自动追加 [Nk]/[1m] 后缀;
// 未配置窗口不加后缀;预设显式携带的标记在未配置时保留。
func TestClaudeCommandContextSuffix(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Kind: provider.KindCompletion, Name: "up", BaseURL: "http://x",
		Models: []provider.ModelConfig{
			{Name: "flash", ContextWindow: 128},
			{Name: "gpt-4o"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(m).WithClaudePresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()

	// model 有窗口 → 派生 [128k];subagent 无窗口 → 不加;small_fast 显式 [1M] → 保留。
	body := `{"name":"dev","base_url":"http://127.0.0.1:8080/api",` +
		`"model":"up@flash","subagent_model":"up@gpt-4o","small_fast_model":"up@gpt-4o[1M]"}`
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
	for _, want := range []string{
		`ANTHROPIC_MODEL = "up@flash[128k]"`,
		`CLAUDE_CODE_SUBAGENT_MODEL = "up@gpt-4o"`,
		`ANTHROPIC_SMALL_FAST_MODEL = "up@gpt-4o[1M]"`,
	} {
		if !strings.Contains(cmd.PowerShell, want) {
			t.Errorf("PowerShell missing %q:\n%s", want, cmd.PowerShell)
		}
	}
	// 整千窗口 → [Nm] 形式。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets", `{"name":"m1m","base_url":"http://127.0.0.1:8080/api"}`, "admin"); resp.StatusCode != http.StatusCreated {
		t.Fatal(resp.StatusCode)
	}
	if err := m.Update(provider.Config{
		Kind: provider.KindCompletion, Name: "up", BaseURL: "http://x",
		Models: []provider.ModelConfig{{Name: "flash", ContextWindow: 1000}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, b = doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev/command", "", "admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status = %d", resp.StatusCode)
	}
	if err := json.Unmarshal([]byte(b), &cmd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.PowerShell, `ANTHROPIC_MODEL = "up@flash[1m]"`) {
		t.Errorf("PowerShell missing 1m suffix:\n%s", cmd.PowerShell)
	}
}

// Claude 预设覆盖本地 settings.json 的 env 块:ANTHROPIC_MODEL 携带窗口后缀。
func TestClaudeApplyLocalContextSuffix(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Kind: provider.KindCompletion, Name: "up", BaseURL: "http://x",
		Models: []provider.ModelConfig{{Name: "flash", ContextWindow: 200}},
	}); err != nil {
		t.Fatal(err)
	}
	cm := newClaudeMgr(t)
	settingsPath := filepath.Join(t.TempDir(), ".claude", "settings.json")
	s := New(m).WithClaudePresets(cm).WithClaudeSettingsPath(settingsPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api","model":"up@flash"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets/dev/apply-local", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ANTHROPIC_MODEL": "up@flash[200k]"`) {
		t.Errorf("settings.json missing suffixed model:\n%s", data)
	}
}

// Codex 预设应用本地:模型目录条目 context_window 取模型配置值(tokens)。
func TestCodexApplyLocalCatalogContextWindow(t *testing.T) {
	cm := newCodexMgr(t)
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Name: "openai", Kind: provider.KindResponses, BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "gpt-4o", ContextWindow: 128}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	catPath := filepath.Join(dir, ".codex", "bsrouter-models.json")
	cachePath := filepath.Join(dir, ".codex", "models_cache.json")
	configPath := filepath.Join(dir, ".codex", "config.toml")
	authPath := filepath.Join(dir, ".codex", "auth.json")
	s := New(m).WithCodexPresets(cm).
		WithCodexConfigPath(configPath).WithCodexAuthPath(authPath).
		WithCodexModelCatalogPath(catPath).WithCodexModelsCachePath(cachePath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api/v1","models":["openai@gpt-4o"]}`); resp.StatusCode != http.StatusCreated {
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
	found := false
	for _, e := range cat.Models {
		if e.DisplayName == "openai@gpt-4o" {
			found = true
			if e.ContextWindow != 128000 || e.MaxContextWindow != 128000 {
				t.Errorf("entry context_window = %d/%d, want 128000", e.ContextWindow, e.MaxContextWindow)
			}
		}
	}
	if !found {
		t.Errorf("catalog missing openai@gpt-4o:\n%s", data)
	}
}

// 单模型上下文窗口更新端点:200 / 404 / 400。
func TestUpdateModelContextWindow(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Kind: provider.KindCompletion, Name: "up", BaseURL: "http://x",
		Models: []provider.ModelConfig{{Name: "flash", ContextWindow: 100}},
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithAPIKey("admin").Handler())
	defer srv.Close()

	// 200 更新。
	resp, b := doAuthed(t, srv, http.MethodPut, "/manage/v1/providers/up/models/flash", `{"context_window":256}`, "admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d; body=%s", resp.StatusCode, b)
	}
	if !strings.Contains(b, `"context_window":256`) {
		t.Errorf("response missing updated window:\n%s", b)
	}
	p, _ := m.Get("up")
	if p.Models()[0].ContextWindow != 256 {
		t.Errorf("persisted window = %d, want 256", p.Models()[0].ContextWindow)
	}

	// 清空回默认:context_window 0。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/providers/up/models/flash", `{"context_window":0}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("clear status = %d", resp.StatusCode)
	}
	p, _ = m.Get("up")
	if p.Models()[0].ContextWindow != 0 {
		t.Errorf("cleared window = %d, want 0", p.Models()[0].ContextWindow)
	}

	// 404 模型不存在。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/providers/up/models/nope", `{"context_window":1}`, "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing model status = %d, want 404", resp.StatusCode)
	}
	// 404 供应商不存在。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/providers/nope/models/flash", `{"context_window":1}`, "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing provider status = %d, want 404", resp.StatusCode)
	}
	// 400 负数。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/providers/up/models/flash", `{"context_window":-1}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative status = %d, want 400", resp.StatusCode)
	}
}

// 模型列表公开 context_window(k)。
func TestListModelsContextWindow(t *testing.T) {
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Kind: provider.KindCompletion, Name: "up", BaseURL: "http://x",
		Models: []provider.ModelConfig{{Name: "flash", ContextWindow: 128}, {Name: "plain"}},
	}); err != nil {
		t.Fatal(err)
	}
	srv := newAPI(t, m)
	resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(b, `"id":"up@flash","object":"model","owned_by":"up","context_window":128`) {
		t.Errorf("body missing context_window:\n%s", b)
	}
}

// sync-models 拉取模型后保留已配置的上下文窗口,新增模型窗口为 0(默认 200k)。
func TestSyncModelsPreservesContextWindow(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"flash"},{"id":"new-model"}]}`)
	}))
	defer up.Close()

	m := newMgr(t)
	if err := m.Add(provider.Config{
		Kind: provider.KindCompletion, Name: "oa", BaseURL: up.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "flash", ContextWindow: 128}},
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
		case "flash":
			if mc.ContextWindow != 128 {
				t.Errorf("flash window = %d, want preserved 128", mc.ContextWindow)
			}
		case "new-model":
			if mc.ContextWindow != 0 {
				t.Errorf("new-model window = %d, want 0", mc.ContextWindow)
			}
		}
	}
}
