package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/dsh"
	"BSRouter/internal/provider"
)

func newDshMgr(t *testing.T) *dsh.Manager {
	t.Helper()
	dm, err := dsh.NewManager(filepath.Join(t.TempDir(), "dsh.json"))
	if err != nil {
		t.Fatal(err)
	}
	return dm
}

func TestDshPresetsCRUD(t *testing.T) {
	dm := newDshMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithDshPresets(dm).WithAPIKey("admin").Handler())
	defer srv.Close()

	body := `{"name":"dev","api_key":"sk-secret"}`
	resp, b := doAuthed(t, srv, http.MethodPost, "/manage/v1/dsh-presets", body, "admin")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, b)
	}
	var got dsh.Config
	if err := json.Unmarshal([]byte(b), &got); err != nil {
		t.Fatal(err)
	}
	if got.APIKey != maskKey("sk-secret") {
		t.Errorf("api_key = %q, want masked %q", got.APIKey, maskKey("sk-secret"))
	}
	if got.CreatedAt.IsZero() {
		t.Error("POST should carry created_at")
	}

	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/dsh-presets", body, "admin"); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status=%d, want 409", resp.StatusCode)
	}
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/dsh-presets", `{"name":"y","api_key":"sk-****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("masked api_key status=%d, want 400", resp.StatusCode)
	}
	// list 掩码
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/dsh-presets", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("list status=%d", resp.StatusCode)
	} else {
		var list []dsh.Config
		_ = json.Unmarshal([]byte(b), &list)
		if len(list) != 1 || list[0].Name != "dev" || list[0].APIKey != maskKey("sk-secret") {
			t.Errorf("list = %+v", list)
		}
	}
	// get 掩码;get 不存在 404
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/dsh-presets/dev", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("get status=%d", resp.StatusCode)
	} else {
		var one dsh.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-secret") {
			t.Errorf("get masked")
		}
	}
	if resp, _ := doAuthed(t, srv, http.MethodGet, "/manage/v1/dsh-presets/nope", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing status=%d, want 404", resp.StatusCode)
	}
	// PUT 空密钥保留原值
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/dsh-presets/dev", `{}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT empty status=%d", resp.StatusCode)
	}
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/dsh-presets/dev", "", "admin"); resp.StatusCode == http.StatusOK {
		var one dsh.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-secret") {
			t.Errorf("PUT empty did not preserve: %q", one.APIKey)
		}
	}
	// PUT 掩码不匹配原密钥 → 400
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/dsh-presets/dev", `{"api_key":"sk-****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT mismatched mask status=%d, want 400", resp.StatusCode)
	}
	// PUT 显式新密钥
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/dsh-presets/dev", `{"api_key":"sk-new"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT new status=%d", resp.StatusCode)
	}
	// PUT 不存在
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/dsh-presets/nope", `{}`, "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT missing status=%d, want 404", resp.StatusCode)
	}
	// DELETE
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/dsh-presets/dev", "", "admin"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status=%d, want 204", resp.StatusCode)
	}
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/dsh-presets/dev", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE again status=%d, want 404", resp.StatusCode)
	}
}

// 覆盖本地 dsh 配置:本机请求成功写 settings.yaml(保留其余内置/自定义供应商与顶层
// 字段);非本机 403;不存在 404。
func TestApplyDshPresetLocal(t *testing.T) {
	dm := newDshMgr(t)
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Name: "openai", Kind: "responses", BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "gpt-4o", ContextWindow: 128}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".dsh", "settings.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `ui-onboarding:
  welcomeNoticeVersion: 2026-08-13.1
llm-pi-ai:
  providers:
    opencode-go:
      apiKeyEnv: OPENCODE_GO_API_KEY
agent-default-model:
  provider: opencode-go
  model: deepseek-v4-flash
`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(m).WithDshPresets(dm).WithDshSettingsPath(path)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/dsh-presets", `{"name":"dev","api_key":"sk-new"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status=%d", resp.StatusCode)
	}
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/dsh-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status=%d; body=%s", resp.StatusCode, b)
	}
	var out struct {
		Applied   bool   `json:"applied"`
		Path      string `json:"path"`
		Provider  string `json:"provider"`
		API       string `json:"api"`
		APIKeyEnv string `json:"api_key_env"`
		Models    int    `json:"models"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Applied || out.Path != path || out.Provider != "dev" || out.API != "anthropic-messages" || out.APIKeyEnv != "DEV_API_KEY" || out.Models < 1 {
		t.Errorf("apply = %+v", out)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"    dev:", "      apiKey: sk-new", "      apiKeyEnv: DEV_API_KEY", "      api: anthropic-messages", "      baseURL: http://127.0.0.1:18154/api",
		"ui-onboarding:", "opencode-go:", "OPENCODE_GO_API_KEY", "agent-default-model:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}

	// 不存在 → 404
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/dsh-presets/nope/apply-local", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("apply missing status=%d, want 404", resp.StatusCode)
	}
	// 非本机 → 403
	req := httptest.NewRequest(http.MethodPost, "/manage/v1/dsh-presets/dev/apply-local", nil)
	req.RemoteAddr = "192.168.1.5:12345"
	w := httptest.NewRecorder()
	s.handleApplyDshPresetLocal(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-local status=%d, want 403", w.Code)
	}
}

// 复用 local.go 的 newMgr 变体:这里定义把 provider kind 传入的辅助函数。
// command 端点:返回嵌入真实密钥的命令(设置 apiKeyEnv 环境变量),掩码不返回。
func TestDshPresetCommand(t *testing.T) {
	dm := newDshMgr(t)
	if err := dm.Add(dsh.Config{Name: "dev", APIKey: "sk-secret"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithDshPresets(dm).WithAPIKey("admin").Handler())
	defer srv.Close()
	resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/dsh-presets/dev/command", "", "admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status=%d; body=%s", resp.StatusCode, b)
	}
	var out struct {
		Name string `json:"name"`
		PS   string `json:"powershell"`
		Bash string `json:"bash"`
		Env  string `json:"api_key_env"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "dev" || out.Env != "DEV_API_KEY" {
		t.Errorf("out = %+v", out)
	}
	if !strings.Contains(out.PS, "$env:DEV_API_KEY = \"sk-secret\"") {
		t.Errorf("ps = %q", out.PS)
	}
	if !strings.Contains(out.Bash, "export DEV_API_KEY=\"sk-secret\"") {
		t.Errorf("bash = %q", out.Bash)
	}
	if !strings.HasSuffix(out.PS, "\ndsh") {
		t.Errorf("ps missing dsh launch: %q", out.PS)
	}
	// 未注册 Manager → 404
	srv2 := httptest.NewServer(New(newMgr(t)).WithAPIKey("admin").Handler())
	defer srv2.Close()
	if resp, _ := doAuthed(t, srv2, http.MethodGet, "/manage/v1/dsh-presets/dev/command", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unregistered command status=%d, want 404", resp.StatusCode)
	}
}

// 网关无任何模型时,apply-local 应返回 400 且不覆盖已有 settings.yaml。
func TestApplyDshPresetLocalNoModels(t *testing.T) {
	dm := newDshMgr(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".dsh", "settings.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seed2 := `ui-onboarding:
  welcomeNoticeVersion: 1
agent-default-model:
  provider: opencode-go
  model: a
`
	if err := os.WriteFile(path, []byte(seed2), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(newMgr(t)).WithDshPresets(dm).WithDshSettingsPath(path)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/dsh-presets", `{"name":"dev"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status=%d", resp.StatusCode)
	}
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/dsh-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("apply status=%d, want 400; body=%s", resp.StatusCode, b)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "agent-default-model:") {
		t.Errorf("empty apply overwrote config:\n%s", data)
	}
}
