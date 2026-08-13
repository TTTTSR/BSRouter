package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/provider"
	"BSRouter/internal/zcode"
)

func newZcodeMgr(t *testing.T) *zcode.Manager {
	t.Helper()
	zm, err := zcode.NewManager(filepath.Join(t.TempDir(), "zcode.json"))
	if err != nil {
		t.Fatal(err)
	}
	return zm
}

func TestZcodePresetsCRUD(t *testing.T) {
	zm := newZcodeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithZcodePresets(zm).WithAPIKey("admin").Handler())
	defer srv.Close()

	// POST 201,响应掩码。
	body := `{"name":"dev","api_key":"sk-secret"}`
	resp, b := doAuthed(t, srv, http.MethodPost, "/manage/v1/zcode-presets", body, "admin")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, b)
	}
	var got zcode.Config
	if err := json.Unmarshal([]byte(b), &got); err != nil {
		t.Fatal(err)
	}
	if got.APIKey != maskKey("sk-secret") {
		t.Errorf("api_key = %q, want masked %q", got.APIKey, maskKey("sk-secret"))
	}
	if got.CreatedAt.IsZero() {
		t.Error("POST response should carry non-zero created_at")
	}

	// 重复 409。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/zcode-presets", body, "admin"); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", resp.StatusCode)
	}
	// 掩码 api_key 400。
	if resp, _ := doAuthed(t, srv, http.MethodPost, "/manage/v1/zcode-presets", `{"name":"y","api_key":"sk-****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("masked api_key status = %d, want 400", resp.StatusCode)
	}
	// list 掩码。
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/zcode-presets", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("list status = %d", resp.StatusCode)
	} else {
		var list []zcode.Config
		if err := json.Unmarshal([]byte(b), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Name != "dev" || list[0].APIKey != maskKey("sk-secret") {
			t.Errorf("list = %+v", list)
		}
	}

	// get 掩码;get 不存在 404。
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/zcode-presets/dev", "", "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("get status = %d", resp.StatusCode)
	} else {
		var one zcode.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-secret") {
			t.Errorf("get api_key = %q, want masked", one.APIKey)
		}
	}
	if resp, _ := doAuthed(t, srv, http.MethodGet, "/manage/v1/zcode-presets/nope", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing status = %d, want 404", resp.StatusCode)
	}

	// PUT 空密钥保留原值。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/zcode-presets/dev",
		`{}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT empty key status = %d", resp.StatusCode)
	}
	if resp, b := doAuthed(t, srv, http.MethodGet, "/manage/v1/zcode-presets/dev", "", "admin"); resp.StatusCode == http.StatusOK {
		var one zcode.Config
		_ = json.Unmarshal([]byte(b), &one)
		if one.APIKey != maskKey("sk-secret") {
			t.Errorf("PUT empty key did not preserve: %q", one.APIKey)
		}
	}
	// PUT 掩码密钥保留原值。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/zcode-presets/dev",
		`{"api_key":"`+maskKey("sk-secret")+`"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT masked key status = %d", resp.StatusCode)
	}
	// PUT 掩码不匹配原密钥 → 400。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/zcode-presets/dev",
		`{"api_key":"sk-****"}`, "admin"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT mismatched mask status = %d, want 400", resp.StatusCode)
	}
	// PUT 显式新密钥覆盖。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/zcode-presets/dev",
		`{"api_key":"sk-new"}`, "admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT new key status = %d", resp.StatusCode)
	}
	// PUT 不存在 404。
	if resp, _ := doAuthed(t, srv, http.MethodPut, "/manage/v1/zcode-presets/nope", `{}`, "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT missing status = %d, want 404", resp.StatusCode)
	}

	// DELETE 204,再删 404。
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/zcode-presets/dev", "", "admin"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", resp.StatusCode)
	}
	if resp, _ := doAuthed(t, srv, http.MethodDelete, "/manage/v1/zcode-presets/dev", "", "admin"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE again status = %d, want 404", resp.StatusCode)
	}
}

// 覆盖本地 zcode 配置:本机请求成功写 config.json(单一 bsrouter 供应商,保留其余);
// 覆盖本地 zcode 配置:本机请求成功写 config.json(统一 API 入口按模型原生格式分割
// 的供应商,保留其余内置供应商);非本机 403;不存在 404。
func TestApplyZcodePresetLocal(t *testing.T) {
	zm := newZcodeMgr(t)
	m := newMgr(t)
	// 网关有供应商:该模型 provider kind=responses → 分割到 bsrouter-responses。
	if err := m.Add(provider.Config{
		Name: "openai", Kind: "responses", BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "gpt-4o", ContextWindow: 128}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".zcode", "v2", "config.json")
	// 预置一个内置供应商,验证 apply 保留它。
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"provider":{"builtin:zai":{"name":"Z.ai","kind":"anthropic","options":{"baseURL":"https://api.z.ai/api/anthropic"},"source":"custom","models":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(m).WithZcodePresets(zm).WithZcodeConfigPath(configPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets",
		`{"name":"dev","api_key":"sk-new"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	// 本机请求(httptest 来自 127.0.0.1)→ 200。
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}
	var out struct {
		Applied   bool   `json:"applied"`
		Path      string `json:"path"`
		Models    int    `json:"models"`
		Providers int    `json:"providers"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Applied || out.Path != configPath || out.Models < 1 || out.Providers != 1 {
		t.Errorf("apply = %+v", out)
	}

	// 验证 config.json 内容。
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"bsrouter-responses"`, // 该模型 provider kind=responses → 分割到 responses 供应商
		`"openai-compatible"`,
		`"wire_api": "responses"`,
		`"baseURL": "http://127.0.0.1:18154/api/v1"`, // 统一 API 入口
		`"apiKey": "sk-new"`,
		`"openai@gpt-4o"`, // 模型列表手动配置写入
		`"context": 128000`, // 上下文窗口同步
		`"builtin:zai"`, // 保留内置供应商
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}

	// 不存在 → 404。
	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets/nope/apply-local", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("apply missing status = %d, want 404", resp.StatusCode)
	}

	// 非本机请求 → 403(伪造非回环 RemoteAddr 直接调用 handler)。
	req := httptest.NewRequest(http.MethodPost, "/manage/v1/zcode-presets/dev/apply-local", nil)
	req.RemoteAddr = "192.168.1.5:12345"
	w := httptest.NewRecorder()
	s.handleApplyZcodePresetLocal(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-local status = %d, want 403", w.Code)
	}
}

// anthropic 格式模型:zcode 供应商 kind=anthropic,base_url 为统一 API 根不带 /v1
// (zcode 会拼接 /v1/messages)。
func TestApplyZcodePresetLocalAnthropic(t *testing.T) {
	zm := newZcodeMgr(t)
	m := newMgr(t)
	// 网关需有可路由模型供 apply-local 写入(模型列表手动配置,不自动获取)。
	if err := m.Add(provider.Config{
		Name: "anthropic", Kind: provider.KindAnthropic, BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "claude-sonnet-4-5", ContextWindow: 200}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".zcode", "v2", "config.json")
	s := New(m).WithZcodePresets(zm).WithZcodeConfigPath(configPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets",
		`{"name":"dev","api_key":"sk-new"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets/dev/apply-local", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"bsrouter-anthropic"`, // anthropic 模型按格式分割到 anthropic 供应商
		`"kind": "anthropic"`,
		`"baseURL": "http://127.0.0.1:18154/api"`, // 统一 API 根,不带 /v1
		`"anthropic@claude-sonnet-4-5"`, // 模型列表写入(手动配置)
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"baseURL": "http://127.0.0.1:18154/api/v1"`) {
		t.Errorf("anthropic base_url must not contain /v1:\n%s", got)
	}
}

// 网关无任何模型时,apply-local 应返回 400 且不覆盖已有 config.json(模型列表手动
// 配置,写入空模型列表会破坏 zcode 已有配置,无法回滚)。
func TestApplyZcodePresetLocalNoModels(t *testing.T) {
	zm := newZcodeMgr(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".zcode", "v2", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"provider":{"keep":{"name":"keep","kind":"anthropic"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(newMgr(t)).WithZcodePresets(zm).WithZcodeConfigPath(configPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets",
		`{"name":"dev"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("apply status = %d, want 400; body=%s", resp.StatusCode, b)
	}
	data, _ := os.ReadFile(configPath)
	if !strings.Contains(string(data), `"keep"`) {
		t.Errorf("empty apply overwrote existing config:\n%s", data)
	}
}

// 未启用预设 Manager 时,zcode 端点应 404(路由未注册)。
func TestZcodePresetsNotRegistered(t *testing.T) {
	srv := httptest.NewServer(New(newMgr(t)).WithAPIKey("admin").Handler())
	defer srv.Close()
	resp, _ := doAuthed(t, srv, http.MethodGet, "/manage/v1/zcode-presets", "", "admin")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unregistered zcode-presets status = %d, want 404", resp.StatusCode)
	}
}

// zcodeProviderSpecs 一律走统一 API 入口,把模型按原生格式分割为多个供应商
// (openai/anthropic/responses);某格式无模型不建该供应商。
func TestZcodeProviderSpecs(t *testing.T) {
	m := newMgr(t)
	// 网关含 completion / anthropic / responses 三种原生格式的模型。
	if err := m.Add(provider.Config{
		Name: "openai", Kind: provider.KindCompletion, BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "gpt-4o", ContextWindow: 128}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(provider.Config{
		Name: "anthropic", Kind: provider.KindAnthropic, BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "claude-sonnet-4-5", ContextWindow: 200}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(provider.Config{
		Name: "codex", Kind: provider.KindResponses, BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "gpt-5", ContextWindow: 1000}},
	}); err != nil {
		t.Fatal(err)
	}
	s := New(m)

	models := []string{"openai@gpt-4o", "anthropic@claude-sonnet-4-5", "codex@gpt-5"}
	windows := map[string]int{"openai@gpt-4o": 128000, "anthropic@claude-sonnet-4-5": 200000, "codex@gpt-5": 1000000}

	specs := s.zcodeProviderSpecs(models, windows)
	if len(specs) != 3 {
		t.Fatalf("specs = %d, want 3: %+v", len(specs), specs)
	}
	byName := make(map[string]zcode.ProviderSpec, len(specs))
	for _, sp := range specs {
		byName[sp.Name] = sp
	}
	if o := byName[zcode.ProviderNameOpenAI]; o.BaseURL != "http://127.0.0.1:18154/api/v1" || o.Kind != zcode.DefaultKind || len(o.Models) != 1 || o.Models[0] != "openai@gpt-4o" {
		t.Errorf("openai spec = %+v", o)
	}
	if a := byName[zcode.ProviderNameAnthropic]; a.BaseURL != "http://127.0.0.1:18154/api" || a.Kind != zcode.KindAnthropic || len(a.Models) != 1 || a.Models[0] != "anthropic@claude-sonnet-4-5" {
		t.Errorf("anthropic spec = %+v", a)
	}
	if r := byName[zcode.ProviderNameResponses]; r.BaseURL != "http://127.0.0.1:18154/api/v1" || r.Kind != zcode.DefaultKind || r.WireAPI != zcode.WireAPIResponses || len(r.Models) != 1 || r.Models[0] != "codex@gpt-5" {
		t.Errorf("responses spec = %+v", r)
	}

	// 只有 anthropic 模型 → 只建 anthropic 供应商。
	specs = s.zcodeProviderSpecs([]string{"anthropic@claude-sonnet-4-5"}, windows)
	if len(specs) != 1 || specs[0].Name != zcode.ProviderNameAnthropic || specs[0].BaseURL != "http://127.0.0.1:18154/api" {
		t.Errorf("anthropic-only specs = %+v", specs)
	}
}

// 统一 API 目标 apply:按模型原生格式分割写入多个 zcode 供应商。
func TestApplyZcodePresetLocalSplit(t *testing.T) {
	zm := newZcodeMgr(t)
	m := newMgr(t)
	if err := m.Add(provider.Config{
		Name: "openai", Kind: provider.KindCompletion, BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "gpt-4o", ContextWindow: 128}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(provider.Config{
		Name: "anthropic", Kind: provider.KindAnthropic, BaseURL: "http://example.com",
		Models: []provider.ModelConfig{{Name: "claude-sonnet-4-5", ContextWindow: 200}},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".zcode", "v2", "config.json")
	s := New(m).WithZcodePresets(zm).WithZcodeConfigPath(configPath)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if resp, _ := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets",
		`{"name":"dev","models":["openai@gpt-4o","anthropic@claude-sonnet-4-5"]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/zcode-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d; body=%s", resp.StatusCode, b)
	}
	var out struct {
		Models    int `json:"models"`
		Providers int `json:"providers"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	if out.Providers != 2 || out.Models != 2 {
		t.Errorf("apply = %+v, want 2 providers / 2 models", out)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"bsrouter-openai"`,
		`"bsrouter-anthropic"`,
		`"kind": "anthropic"`,
		`"baseURL": "http://127.0.0.1:18154/api/v1"`, // openai 带 /v1
		`"baseURL": "http://127.0.0.1:18154/api"`,    // anthropic 不带 /v1
		`"openai@gpt-4o"`,
		`"anthropic@claude-sonnet-4-5"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
}
