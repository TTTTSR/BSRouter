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
	"BSRouter/internal/provider"
)

func newCodexMgr(t *testing.T) *codex.Manager {
	t.Helper()
	cm, err := codex.NewManager(filepath.Join(t.TempDir(), "codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

func TestCodexPresetsCRUD(t *testing.T) {
	cm := newCodexMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithCodexPresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()

	// POST 201,响应掩码。
	body := `{"name":"dev","base_url":"http://127.0.0.1:8080/api/v1","api_key":"sk-secret","model":"openai@gpt-4o"}`
	resp, b := doAuthed(t, srv, http.MethodPost, "/manage/v1/codex-presets", body, "admin")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, b)
	}
	var got codex.Config
	if err := json.Unmarshal([]byte(b), &got); err != nil {
		t.Fatal(err)
	}
	if got.APIKey != maskKey("sk-secret") {
		t.Errorf("api_key = %q, want masked %q", got.APIKey, maskKey("sk-secret"))
	}
	if got.BaseURL != "http://127.0.0.1:8080/api/v1" {
		t.Errorf("base_url = %q", got.BaseURL)
	}
	if got.CreatedAt.IsZero() {
		t.Error("POST response should carry non-zero created_at")
	}

	// 重复 409。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/codex-presets", body, "admin"); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", resp.StatusCode)
	}
	// 非法 base_url 400。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/codex-presets", `{"name":"x","base_url":"file:///tmp"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid base_url status = %d, want 400", resp.StatusCode)
	}
	// 掩码 api_key 400。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/codex-presets", `{"name":"y","base_url":"http://x","api_key":"sk-****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("masked api_key status = %d, want 400", resp.StatusCode)
	}
	// list 掩码。
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("list status = %d", resp.StatusCode)
	} else {
		var list []codex.Config
		if err := json.Unmarshal([]byte(b), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Name != "dev" || list[0].APIKey != maskKey("sk-secret") {
			t.Errorf("list = %+v", list)
		}
	}

	// get 掩码;get 不存在 404。
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets/dev", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("get status = %d", resp.StatusCode)
	} else {
		var one codex.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-secret") {
			t.Errorf("get api_key = %q, want masked", one.APIKey)
		}
	}
	if resp, _ := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets/nope", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing status = %d, want 404", resp.StatusCode)
	}

	// PUT 空密钥保留原值。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/codex-presets/dev", `{"base_url":"http://127.0.0.1:8080/api/v1","model":"m2"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT empty key status = %d", resp.StatusCode)
	}
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets/dev", "", "admin"); resp.StatusCode == http.StatusOK {
		var one codex.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-secret") {
			t.Errorf("PUT empty key did not preserve: %q", one.APIKey)
		}
		if one.Model != "m2" {
			t.Errorf("model = %q, want m2", one.Model)
		}
	}
	// PUT 掩码密钥保留原值。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/codex-presets/dev",
		`{"base_url":"http://127.0.0.1:8080/api/v1","api_key":"`+maskKey("sk-secret")+`"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT masked key status = %d", resp.StatusCode)
	}
	// PUT 原密钥为空时提交掩码占位 → 400(避免存字面 "****")。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/codex-presets/dev",
		`{"base_url":"http://127.0.0.1:8080/api/v1","api_key":"****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT masked-into-empty status = %d, want 400", resp.StatusCode)
	}
	// PUT 掩码不匹配原密钥 → 400(避免存错误掩码)。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/codex-presets/dev",
		`{"base_url":"http://127.0.0.1:8080/api/v1","api_key":"sk-****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT mismatched mask status = %d, want 400", resp.StatusCode)
	}
	// PUT 显式新密钥覆盖。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/codex-presets/dev",
		`{"base_url":"http://127.0.0.1:8080/api/v1","api_key":"sk-new"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT new key status = %d", resp.StatusCode)
	}
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets/dev", "", "admin"); resp.StatusCode == http.StatusOK {
		var one codex.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-new") {
			t.Errorf("PUT new key: api_key = %q, want masked %q", one.APIKey, maskKey("sk-new"))
		}
	}
	// PUT 不存在 404。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/codex-presets/nope", `{"base_url":"http://x"}`, "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT missing status = %d, want 404", resp.StatusCode)
	}

	// DELETE 204,再删 404。
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/codex-presets/dev", "", "admin"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", resp.StatusCode)
	}
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/codex-presets/dev", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE again status = %d, want 404", resp.StatusCode)
	}
}

func TestCodexPresetCommand(t *testing.T) {
	cm := newCodexMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithCodexPresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()

	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api/v1","api_key":"sk-secret","model":"openai@gpt-4o"}`, "admin"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets/dev/command", "", "admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status = %d; body=%s", resp.StatusCode, b)
	}
	var cmd struct {
		Name       string `json:"name"`
		PowerShell string `json:"powershell"`
		Bash       string `json:"bash"`
	}
	if err := json.Unmarshal([]byte(b), &cmd); err != nil {
		t.Fatal(err)
	}
	// 命令包含真实密钥(未掩码)与 codex 启动。
	if !strings.Contains(cmd.PowerShell, "sk-secret") {
		t.Errorf("PowerShell missing real key:\n%s", cmd.PowerShell)
	}
	if !strings.Contains(cmd.PowerShell, "codex") {
		t.Errorf("PowerShell missing codex invocation:\n%s", cmd.PowerShell)
	}
	if !strings.Contains(cmd.Bash, "sk-secret") {
		t.Errorf("Bash missing real key:\n%s", cmd.Bash)
	}
	if !strings.Contains(cmd.Bash, "codex") {
		t.Errorf("Bash missing codex invocation:\n%s", cmd.Bash)
	}
	// base_url 指向统一 API 的 /v1 段。
	if !strings.Contains(cmd.Bash, `base_url="http://127.0.0.1:8080/api/v1"`) {
		t.Errorf("Bash missing base_url:\n%s", cmd.Bash)
	}

	// 不存在 404。
	if resp, _ := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets/nope/command", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("command missing status = %d, want 404", resp.StatusCode)
	}
}

func TestCodexPresetPersistFailure(t *testing.T) {
	// manager 指向不存在的目录 -> save 失败 -> 500,内存回滚。
	cm, err := codex.NewManager(filepath.Join(t.TempDir(), "missing", "codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithCodexPresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()
	resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/codex-presets", `{"name":"dev","base_url":"http://x"}`, "admin")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("persist failure status = %d, want 500", resp.StatusCode)
	}
	if cm.Count() != 0 {
		t.Error("failed Add should roll back in-memory state")
	}
}

// 预设未配置密钥时,命令端点自动生成默认受管 key 并注入;该 key 在 /api 上有效。
func TestCodexPresetDefaultKey(t *testing.T) {
	km := newKeyMgr(t)
	if _, err := km.Generate("team-a"); err != nil {
		t.Fatal(err)
	}
	cm := newCodexMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKeys(km).WithCodexPresets(cm).Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api/v1"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/codex-presets/dev/command", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status = %d; body=%s", resp.StatusCode, b)
	}
	k, err := km.Get(claudeDefaultKeyName)
	if err != nil {
		t.Fatalf("default key not auto-generated: %v", err)
	}
	var cmd struct {
		PowerShell string `json:"powershell"`
	}
	if err := json.Unmarshal([]byte(b), &cmd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.PowerShell, `$env:OPENAI_API_KEY = "`+k.Key+`"`) {
		t.Errorf("command should set OPENAI_API_KEY to auto key:\n%s", cmd.PowerShell)
	}

	// 自动生成的受管 key 在 /api 上有效。
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+k.Key)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("auto key /api status = %d, want 200", r2.StatusCode)
	}
}

// 覆盖本地 Codex 配置:本机请求成功写 config.toml + auth.json + 模型目录;非本机 403;不存在 404。
func TestApplyCodexPresetLocal(t *testing.T) {
	cm := newCodexMgr(t)
	m := newMgr(t)
	// 给网关加一个有模型的供应商,验证模型目录生成。
	if err := m.Add(provider.Config{
		Name: "openai", Kind: "responses", BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".codex", "config.toml")
	authPath := filepath.Join(dir, ".codex", "auth.json")
	catPath := filepath.Join(dir, ".codex", "bsrouter-models.json")
	cachePath := filepath.Join(dir, ".codex", "models_cache.json")
	s := New(m).WithCodexPresets(cm).WithCodexConfigPath(configPath).WithCodexAuthPath(authPath).WithCodexModelCatalogPath(catPath).WithCodexModelsCachePath(cachePath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api/v1","api_key":"sk-new","model":"openai@gpt-4o"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	// 本机请求(httptest 来自 127.0.0.1)→ 200。
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}
	var out struct {
		Applied  bool   `json:"applied"`
		Path     string `json:"path"`
		AuthPath string `json:"auth_path"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Applied || out.Path != configPath || out.AuthPath != authPath {
		t.Errorf("apply = %+v", out)
	}

	// 验证 config.toml 写入单一 bsrouter 块。
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`model_provider = "bsrouter"`,
		`model = "openai@gpt-4o"`,
		`[model_providers.bsrouter]`,
		`base_url = "http://127.0.0.1:8080/api/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}

	// 验证 auth.json 写入 OPENAI_API_KEY(跳过登录的关键)。
	authData, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	if !strings.Contains(string(authData), `"OPENAI_API_KEY": "sk-new"`) {
		t.Errorf("auth.json missing OPENAI_API_KEY:\n%s", authData)
	}

	// 验证模型目录生成(含网关可路由模型 openai@gpt-4o)。
	catData, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read model catalog: %v", err)
	}
	if !strings.Contains(string(catData), `"openai@gpt-4o"`) {
		t.Errorf("model catalog missing gateway model:\n%s", catData)
	}

	// 验证 models_cache.json(桌面 app 读)也生成。
	cacheData, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read models cache: %v", err)
	}
	if !strings.Contains(string(cacheData), `"openai@gpt-4o"`) {
		t.Errorf("models_cache missing gateway model:\n%s", cacheData)
	}
	if !strings.Contains(string(cacheData), `"fetched_at"`) {
		t.Errorf("models_cache missing fetched_at header:\n%s", cacheData)
	}

	// 不存在 → 404。
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets/nope/apply-local", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("apply missing status = %d, want 404", resp.StatusCode)
	}

	// 非本机请求 → 403(伪造非回环 RemoteAddr 直接调用 handler)。
	req := httptest.NewRequest(http.MethodPost, "/manage/v1/codex-presets/dev/apply-local", nil)
	req.RemoteAddr = "192.168.1.5:12345"
	w := httptest.NewRecorder()
	s.handleApplyCodexPresetLocal(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-local status = %d, want 403", w.Code)
	}
}

// apply-local 未配置密钥时,自动注入默认 key 并写入 auth.json。
func TestApplyCodexPresetLocalDefaultKeyAuth(t *testing.T) {
	km := newKeyMgr(t)
	cm := newCodexMgr(t)
	m := newMgr(t)
	// 需要模型目录非空,否则空保护会拒绝(400)。
	if err := m.Add(provider.Config{
		Name: "deepseek", Kind: "anthropic", BaseURL: "https://api.deepseek.com/anthropic",
		Models: []provider.ModelConfig{{Name: "deepseek-v4-flash"}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".codex", "config.toml")
	authPath := filepath.Join(dir, ".codex", "auth.json")
	s := New(m).WithAPIKeys(km).WithCodexPresets(cm).WithCodexConfigPath(configPath).WithCodexAuthPath(authPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api/v1"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets/dev/apply-local", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}
	k, err := km.Get(claudeDefaultKeyName)
	if err != nil {
		t.Fatalf("default key not auto-generated: %v", err)
	}
	authData, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	if !strings.Contains(string(authData), k.Key) {
		t.Errorf("auth.json missing auto default key:\n%s", authData)
	}
}

// 原密钥为空时,PUT 提交掩码占位应 400,而非存为字面密钥。
func TestCodexPresetUpdateMaskedIntoEmpty(t *testing.T) {
	cm := newCodexMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithCodexPresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()

	// 无密钥的预设。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"nokey","base_url":"http://127.0.0.1:8080/api/v1"}`, "admin"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	// 提交 "****" → 400。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/codex-presets/nokey",
		`{"base_url":"http://127.0.0.1:8080/api/v1","api_key":"****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT masked-into-empty status = %d, want 400", resp.StatusCode)
	}
	// 存储仍无密钥。
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets/nokey", "", "admin"); resp.StatusCode == http.StatusOK {
		if strings.Contains(b, `"api_key"`) {
			t.Errorf("api_key should remain empty:\n%s", b)
		}
	}
	// 正常 PUT 空密钥(前端编辑默认行为)保留空。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/codex-presets/nokey",
		`{"base_url":"http://127.0.0.1:8080/api/v1"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT empty status = %d, want 200", resp.StatusCode)
	}
}

// 网关无任何模型时,apply-local 应返回 400 且不写空模型文件(避免覆盖破坏)。
func TestApplyCodexPresetLocalNoModels(t *testing.T) {
	cm := newCodexMgr(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".codex", "config.toml")
	catPath := filepath.Join(dir, ".codex", "bsrouter-models.json")
	// 先放一个已有模型目录,验证不会被空列表覆盖。
	if err := os.MkdirAll(filepath.Dir(catPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catPath, []byte(`{"models":[{"slug":"keep-me"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(newMgr(t)).WithCodexPresets(cm).WithCodexConfigPath(configPath).WithCodexModelCatalogPath(catPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api/v1"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	// 无模型 → 400,且不覆盖已有目录。
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/codex-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("apply status = %d, want 400; body=%s", resp.StatusCode, b)
	}
	data, _ := os.ReadFile(catPath)
	if !strings.Contains(string(data), "keep-me") {
		t.Errorf("empty apply overwrote existing catalog:\n%s", data)
	}
}

// 未启用预设 Manager 时,codex 端点应 404(路由未注册)。
func TestCodexPresetsNotRegistered(t *testing.T) {
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKey("admin").Handler())
	defer srv.Close()
	resp, _ := doAuthed(t, srv, http.MethodGet, "/manage/v1/codex-presets", "", "admin")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unregistered codex-presets status = %d, want 404", resp.StatusCode)
	}
}
