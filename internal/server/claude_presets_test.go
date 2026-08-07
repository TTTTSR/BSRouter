package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/claude"
)

func newClaudeMgr(t *testing.T) *claude.Manager {
	t.Helper()
	cm, err := claude.NewManager(filepath.Join(t.TempDir(), "claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

func TestClaudePresetsCRUD(t *testing.T) {
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithClaudePresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()

	// POST 201,响应掩码。
	body := `{"name":"dev","base_url":"http://127.0.0.1:8080/api","api_key":"sk-secret","model":"m"}`
	resp, b := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets", body, "admin")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, b)
	}
	var got claude.Config
	if err := json.Unmarshal([]byte(b), &got); err != nil {
		t.Fatal(err)
	}
	if got.APIKey != maskKey("sk-secret") {
		t.Errorf("api_key = %q, want masked %q", got.APIKey, maskKey("sk-secret"))
	}
	if got.BaseURL != "http://127.0.0.1:8080/api" {
		t.Errorf("base_url = %q", got.BaseURL)
	}
	if got.CreatedAt.IsZero() {
		t.Error("POST response should carry non-zero created_at")
	}

	// 重复 409。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets", body, "admin"); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", resp.StatusCode)
	}
	// 非法 base_url 400。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets", `{"name":"x","base_url":"file:///tmp"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid base_url status = %d, want 400", resp.StatusCode)
	}
	// 掩码 api_key 400。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets", `{"name":"y","base_url":"http://x","api_key":"sk-****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("masked api_key status = %d, want 400", resp.StatusCode)
	}
	// api_key + auth_token 同填 400。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets", `{"name":"z","base_url":"http://x","api_key":"k","auth_token":"t"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("both auth status = %d, want 400", resp.StatusCode)
	}

	// list 掩码。
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("list status = %d", resp.StatusCode)
	} else {
		var list []claude.Config
		if err := json.Unmarshal([]byte(b), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Name != "dev" || list[0].APIKey != maskKey("sk-secret") {
			t.Errorf("list = %+v", list)
		}
	}

	// get 掩码;get 不存在 404。
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("get status = %d", resp.StatusCode)
	} else {
		var one claude.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-secret") {
			t.Errorf("get api_key = %q, want masked", one.APIKey)
		}
	}
	if resp, _ := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/nope", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing status = %d, want 404", resp.StatusCode)
	}

	// PUT 空 token 保留原值。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/claude-presets/dev", `{"base_url":"http://127.0.0.1:8080/api","model":"m2"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT empty token status = %d", resp.StatusCode)
	}
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev", "", "admin"); resp.StatusCode == http.StatusOK {
		var one claude.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-secret") {
			t.Errorf("PUT empty token did not preserve: %q", one.APIKey)
		}
		if one.Model != "m2" {
			t.Errorf("model = %q, want m2", one.Model)
		}
	}
	// PUT 掩码 token 保留原值。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/claude-presets/dev",
		`{"base_url":"http://127.0.0.1:8080/api","api_key":"`+maskKey("sk-secret")+`"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT masked token status = %d", resp.StatusCode)
	}
	// PUT 显式新 token 覆盖。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/claude-presets/dev",
		`{"base_url":"http://127.0.0.1:8080/api","api_key":"sk-new"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT new token status = %d", resp.StatusCode)
	}
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev", "", "admin"); resp.StatusCode == http.StatusOK {
		var one claude.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-new") {
			t.Errorf("PUT new token: api_key = %q, want masked %q", one.APIKey, maskKey("sk-new"))
		}
	}
	// PUT 不存在 404。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/claude-presets/nope", `{"base_url":"http://x"}`, "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT missing status = %d, want 404", resp.StatusCode)
	}

	// DELETE 204,再删 404。
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/claude-presets/dev", "", "admin"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", resp.StatusCode)
	}
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/claude-presets/dev", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE again status = %d, want 404", resp.StatusCode)
	}
}

func TestClaudePresetCommand(t *testing.T) {
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithClaudePresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()

	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api","api_key":"sk-secret","model":"m"}`, "admin"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev/command", "", "admin")
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
	// 命令包含真实密钥(未掩码)与正确的清理行。
	if !strings.Contains(cmd.PowerShell, "sk-secret") {
		t.Errorf("PowerShell missing real token:\n%s", cmd.PowerShell)
	}
	if !strings.Contains(cmd.PowerShell, "Remove-Item Env:ANTHROPIC_AUTH_TOKEN -ErrorAction SilentlyContinue") {
		t.Errorf("PowerShell missing cleanup line:\n%s", cmd.PowerShell)
	}
	if !strings.Contains(cmd.Bash, "sk-secret") {
		t.Errorf("Bash missing real token:\n%s", cmd.Bash)
	}
	if !strings.Contains(cmd.Bash, "unset ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("Bash missing cleanup line:\n%s", cmd.Bash)
	}
	if !strings.HasSuffix(cmd.PowerShell, "\nclaude") || !strings.HasSuffix(cmd.Bash, "\nclaude") {
		t.Error("commands must end with claude")
	}

	// 不存在 404。
	if resp, _ := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/nope/command", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("command missing status = %d, want 404", resp.StatusCode)
	}
}

func TestClaudePresetPersistFailure(t *testing.T) {
	// manager 指向不存在的目录 -> save 失败 -> 500,内存回滚。
	cm, err := claude.NewManager(filepath.Join(t.TempDir(), "missing", "claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(newMgr(t)).WithClaudePresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()
	resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets", `{"name":"dev","base_url":"http://x"}`, "admin")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("persist failure status = %d, want 500", resp.StatusCode)
	}
	if cm.Count() != 0 {
		t.Error("failed Add should roll back in-memory state")
	}
}

func TestClaudePresetRequireJSON(t *testing.T) {
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithClaudePresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/manage/v1/claude-presets", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

// 预设未配置密钥时,命令端点自动生成默认受管 key 并注入;该 key 在 /api 上有效。
func TestClaudePresetDefaultKey(t *testing.T) {
	km := newKeyMgr(t)
	if _, err := km.Generate("team-a"); err != nil {
		t.Fatal(err)
	} // 已有其他受管 key -> /api 需要受管 key
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKeys(km).WithClaudePresets(cm).Handler())
	defer srv.Close()

	// 预设不配置密钥。
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	// 命令端点自动生成默认受管 key 并注入。
	resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev/command", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status = %d; body=%s", resp.StatusCode, b)
	}
	k, err := km.Get(claudeDefaultKeyName)
	if err != nil {
		t.Fatalf("default key not auto-generated: %v", err)
	}
	var cmd struct {
		Name       string `json:"name"`
		PowerShell string `json:"powershell"`
		Bash       string `json:"bash"`
	}
	if err := json.Unmarshal([]byte(b), &cmd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.PowerShell, `$env:ANTHROPIC_AUTH_TOKEN = "`+k.Key+`"`) {
		t.Errorf("command should set ANTHROPIC_AUTH_TOKEN to auto key:\n%s", b)
	}
	if !strings.Contains(cmd.Bash, `unset ANTHROPIC_API_KEY`) {
		t.Errorf("bash should clean ANTHROPIC_API_KEY:\n%s", b)
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

// 未启用受管 key 时,命令注入网关 key 作为默认鉴权。
func TestClaudePresetDefaultKeyGatewayKey(t *testing.T) {
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithClaudePresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()

	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/claude-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api"}`, "admin"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev/command", "", "admin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status = %d; body=%s", resp.StatusCode, b)
	}
	var cmd struct {
		PowerShell string `json:"powershell"`
	}
	if err := json.Unmarshal([]byte(b), &cmd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.PowerShell, `$env:ANTHROPIC_AUTH_TOKEN = "admin"`) {
		t.Errorf("command should use gateway key as default:\n%s", b)
	}
}

// 本地模式检测:httptest 请求来自 127.0.0.1,应返回 local:true。
func TestLocalStatus(t *testing.T) {
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithClaudePresets(cm).Handler())
	defer srv.Close()
	resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/local", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(b, `"local":true`) {
		t.Errorf("body = %s, want local:true", b)
	}
}

// 覆盖本地 Claude Code 配置:本机请求成功写 settings.json;非本机 403;不存在 404。
func TestApplyClaudePresetLocal(t *testing.T) {
	cm := newClaudeMgr(t)
	settingsPath := filepath.Join(t.TempDir(), ".claude", "settings.json")
	s := New(newMgr(t)).WithClaudePresets(cm).WithClaudeSettingsPath(settingsPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets",
		`{"name":"dev","base_url":"http://127.0.0.1:8080/api/term-a","auth_token":"sk-new","model":"openai@gpt-4o"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	// 本机请求(httptest 来自 127.0.0.1)→ 200。
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}
	var out struct {
		Applied bool   `json:"applied"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Applied || out.Path != settingsPath {
		t.Errorf("apply = %+v", out)
	}

	// 验证 settings.json 被覆盖。
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !strings.Contains(string(data), `"ANTHROPIC_BASE_URL": "http://127.0.0.1:8080/api/term-a"`) {
		t.Errorf("settings missing base_url:\n%s", data)
	}
	if !strings.Contains(string(data), `"ANTHROPIC_AUTH_TOKEN": "sk-new"`) {
		t.Errorf("settings missing auth_token:\n%s", data)
	}

	// 不存在 → 404。
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets/nope/apply-local", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("apply missing status = %d, want 404", resp.StatusCode)
	}

	// 非本机请求 → 403(伪造非回环 RemoteAddr 直接调用 handler)。
	req := httptest.NewRequest(http.MethodPost, "/manage/v1/claude-presets/dev/apply-local", nil)
	req.RemoteAddr = "192.168.1.5:12345"
	w := httptest.NewRecorder()
	s.handleApplyClaudePresetLocal(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-local status = %d, want 403", w.Code)
	}
}

// 受管 apikey 不能访问 /manage 下的 claude 端点(落入 /manage 鉴权)。
func TestClaudePresetManagedKeyRejected(t *testing.T) {
	km := newKeyMgr(t)
	k, _ := km.Generate("client-a")
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKeys(km).WithClaudePresets(cm).WithAPIKey("admin").Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/manage/v1/claude-presets/x/command", nil)
	req.Header.Set("Authorization", "Bearer "+k.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("managed key status = %d, want 401", resp.StatusCode)
	}
}
