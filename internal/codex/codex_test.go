package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	if err := (Config{Name: "dev", BaseURL: "http://127.0.0.1:8080/api/v1"}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := (Config{Name: "dev", BaseURL: "https://host/v1", APIKey: "k"}).Validate(); err != nil {
		t.Fatalf("valid config with api_key rejected: %v", err)
	}
	// base_url 可选(留空由网关派生统一 API 入口)。
	if err := (Config{Name: "dev"}).Validate(); err != nil {
		t.Fatalf("valid config without base_url rejected: %v", err)
	}
	// 合法 models:最多 7 个,每个非空。
	if err := (Config{Name: "dev", Models: []string{"deepseek@a", "deepseek@b", "opencode-go@gpt-5.6-luna"}}).Validate(); err != nil {
		t.Fatalf("valid models rejected: %v", err)
	}

	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty name", Config{BaseURL: "http://x/v1"}},
		{"slash name", Config{Name: "a/b", BaseURL: "http://x/v1"}},
		{"newline name", Config{Name: "a\nb", BaseURL: "http://x/v1"}},
		{"file base_url", Config{Name: "a", BaseURL: "file:///tmp/x"}},
		{"ftp base_url", Config{Name: "a", BaseURL: "ftp://x"}},
		{"bad env_key", Config{Name: "a", BaseURL: "http://x", EnvKey: "A-B"}},
		{"newline base_url", Config{Name: "a", BaseURL: "http://x\nboom"}},
		{"newline model", Config{Name: "a", BaseURL: "http://x", Model: "m\nboom"}},
		{"models too many", Config{Name: "a", Models: []string{"p@1", "p@2", "p@3", "p@4", "p@5", "p@6", "p@7", "p@8", "p@9"}}},
		{"models empty item", Config{Name: "a", Models: []string{"p@1", "  "}}},
		{"models whitespace", Config{Name: "a", Models: []string{"p@1", "p@ 2"}}},
		{"models duplicate", Config{Name: "a", Models: []string{"p@1", "p@1"}}},
		{"extra space key", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"a b": "v"}}},
		{"extra at key", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"A@B": "v"}}},
		{"extra leading digit", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"1abc": "v"}}},
		{"extra quoted key", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{`"k"`: "v"}}},
		{"extra reserved top", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"model": "v"}}},
		{"extra reserved provider", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"model_providers.bsrouter.base_url": "v"}}},
		{"extra reserved prefix", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"model_providers.bsrouter.zz": "v"}}},
		{"extra bare table key", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"model_providers": "v"}}},
		{"extra bare bsrouter key", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"model_providers.bsrouter": "v"}}},
		{"extra newline value", Config{Name: "a", BaseURL: "http://x", ExtraConfig: map[string]string{"a.b": "v\nx"}}},
	}
	for _, tc := range cases {
		if err := tc.cfg.Validate(); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestManagerCRUD(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: "dev", BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "sk-abc"}

	if err := m.Add(cfg); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(cfg); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate Add err = %v, want ErrExists", err)
	}
	if err := m.Add(Config{}); err == nil {
		t.Error("invalid Add should fail")
	}
	if m.Count() != 1 {
		t.Errorf("count = %d, want 1", m.Count())
	}

	got, err := m.Get("dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	created := got.CreatedAt
	if created.IsZero() {
		t.Error("Add should set CreatedAt")
	}

	upd := got
	upd.Description = "changed"
	if err := m.Update(upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got2, _ := m.Get("dev")
	if !got2.CreatedAt.Equal(created) {
		t.Errorf("Update changed CreatedAt: %v -> %v", created, got2.CreatedAt)
	}
	if got2.Description != "changed" {
		t.Errorf("description = %q, want changed", got2.Description)
	}

	if err := m.Delete("dev"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.Delete("dev"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete again err = %v, want ErrNotFound", err)
	}
	if m.Count() != 0 {
		t.Errorf("count after delete = %d, want 0", m.Count())
	}
}

func TestManagerPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "codex.json")
	m, _ := NewManager(file)
	if err := m.Add(Config{Name: "dev", BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "sk-secret"}); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(file)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, err := m2.Get("dev")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-secret" {
		t.Errorf("reloaded api_key = %q, want full token", got.APIKey)
	}
}

func TestEnvKeyName(t *testing.T) {
	if got := (Config{}).EnvKeyName(); got != "OPENAI_API_KEY" {
		t.Errorf("default EnvKeyName = %q", got)
	}
	if got := (Config{EnvKey: "ZZ_KEY"}).EnvKeyName(); got != "ZZ_KEY" {
		t.Errorf("custom EnvKeyName = %q", got)
	}
}

// 黄金用例:全字段 + 需转义的特殊字符,精确整串断言。
func TestBuildCommandGolden(t *testing.T) {
	cfg := Config{
		Name:            "dev",
		BaseURL:         `http://127.0.0.1:8080/api/v1`,
		APIKey:          `sk-"x$y\z`,
		Model:           "deepseek-v4-flash[1M]",
		ReasoningEffort: "low",
		ExtraConfig:     map[string]string{"features.js_repl": "true"},
	}
	cmd := BuildCommand(cfg)

	wantPS := "$env:OPENAI_API_KEY = \"sk-`\"x`$y\\z\"\n" +
		"codex\n" +
		"      -c 'model_providers.bsrouter.wire_api=\"responses\"' `\n" +
		"      -c 'model_providers.bsrouter.name=\"bsrouter\"' `\n" +
		"      -c 'model_providers.bsrouter.base_url=\"http://127.0.0.1:8080/api/v1\"' `\n" +
		"      -c 'model_providers.bsrouter.env_key=\"OPENAI_API_KEY\"' `\n" +
		"      -c 'model_providers.bsrouter.requires_openai_auth=\"true\"' `\n" +
		"      -c 'model_provider=\"bsrouter\"' `\n" +
		"      -m \"deepseek-v4-flash[1M]\" `\n" +
		"      -c 'model_reasoning_effort=\"low\"' `\n" +
		"      -c 'features.js_repl=\"true\"'"
	if cmd.PowerShell != wantPS {
		t.Errorf("PowerShell:\n got:\n%s\nwant:\n%s", cmd.PowerShell, wantPS)
	}

	wantBash := "export OPENAI_API_KEY=\"sk-\\\"x\\$y\\\\z\"\n" +
		"codex\n" +
		"      -c 'model_providers.bsrouter.wire_api=\"responses\"' \\\n" +
		"      -c 'model_providers.bsrouter.name=\"bsrouter\"' \\\n" +
		"      -c 'model_providers.bsrouter.base_url=\"http://127.0.0.1:8080/api/v1\"' \\\n" +
		"      -c 'model_providers.bsrouter.env_key=\"OPENAI_API_KEY\"' \\\n" +
		"      -c 'model_providers.bsrouter.requires_openai_auth=\"true\"' \\\n" +
		"      -c 'model_provider=\"bsrouter\"' \\\n" +
		"      -m \"deepseek-v4-flash[1M]\" \\\n" +
		"      -c 'model_reasoning_effort=\"low\"' \\\n" +
		"      -c 'features.js_repl=\"true\"'"
	if cmd.Bash != wantBash {
		t.Errorf("Bash:\n got:\n%s\nwant:\n%s", cmd.Bash, wantBash)
	}
}

// 未配置模型不输出 -m;env_key 自定义;空 wire_api 默认输出 responses。
func TestBuildCommandMinimal(t *testing.T) {
	cmd := BuildCommand(Config{BaseURL: "http://127.0.0.1:8080/api/v1", EnvKey: "ZZ_KEY"})
	if strings.Contains(cmd.PowerShell, `-m "`) {
		t.Errorf("empty model should not emit -m:\n%s", cmd.PowerShell)
	}
	if !strings.Contains(cmd.PowerShell, `wire_api="responses"`) {
		t.Errorf("empty wire_api should default to responses:\n%s", cmd.PowerShell)
	}
	if !strings.Contains(cmd.PowerShell, `$env:ZZ_KEY = ""`) {
		t.Errorf("custom env_key missing in PS:\n%s", cmd.PowerShell)
	}
	if !strings.HasPrefix(strings.TrimSpace(cmd.Bash), "export ZZ_KEY=") {
		t.Errorf("custom env_key missing in Bash:\n%s", cmd.Bash)
	}
}

// -c 值内嵌单引号时的 shell 转义。
func TestBuildCommandQuoteInValue(t *testing.T) {
	cfg := Config{BaseURL: "http://x", APIKey: "k", ExtraConfig: map[string]string{"a.b": `it's`}}
	cmd := BuildCommand(cfg)
	if !strings.Contains(cmd.PowerShell, `-c 'a.b="it''s"'`) {
		t.Errorf("PS single-quote escape wrong:\n%s", cmd.PowerShell)
	}
	if !strings.Contains(cmd.Bash, `-c 'a.b="it'\''s"'`) {
		t.Errorf("Bash single-quote escape wrong:\n%s", cmd.Bash)
	}
}

func TestBuildCommandIdempotent(t *testing.T) {
	cfg := Config{Name: "x", BaseURL: "http://x/v1", APIKey: "k",
		ExtraConfig: map[string]string{"a": "1", "b": "2"}}
	a := BuildCommand(cfg)
	b := BuildCommand(cfg)
	if a != b {
		t.Error("BuildCommand not deterministic")
	}
}

// escapeToml 与 shell 转义器单测。
func TestEscapeToml(t *testing.T) {
	if got := escapeToml(`a"b\c`); got != `a\"b\\c` {
		t.Errorf("escapeToml = %q", got)
	}
	if got := escapeToml(`plain`); got != "plain" {
		t.Errorf("escapeToml plain = %q", got)
	}
}

// auth.json:写入 OPENAI_API_KEY,保留其余字段;key 为空不动。
func TestApplyToLocalAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "OPENAI_SESSION": "sess-token",
  "OPENAI_API_KEY": "old-key"
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyToLocalAuth(path, "sk-new-key"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["OPENAI_API_KEY"] != "sk-new-key" {
		t.Errorf("OPENAI_API_KEY = %q, want sk-new-key", out["OPENAI_API_KEY"])
	}
	if out["OPENAI_SESSION"] != "sess-token" {
		t.Errorf("OPENAI_SESSION lost = %q, want preserved", out["OPENAI_SESSION"])
	}
}

// key 为空时不写 auth.json(不破坏已有鉴权)。
func TestApplyToLocalAuthEmptyKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := `{"OPENAI_API_KEY": "keep-me"}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyToLocalAuth(path, "  "); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "keep-me") {
		t.Errorf("empty key should not touch auth.json:\n%s", data)
	}
}

// auth.json 不存在时创建。
func TestApplyToLocalAuthCreatesMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "auth.json")
	if err := ApplyToLocalAuth(path, "sk-new"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file should be created: %v", err)
	}
	if !strings.Contains(string(data), "sk-new") {
		t.Errorf("created auth.json missing key:\n%s", data)
	}
}

// 模型目录生成:slug 用模型 id,字段齐全(codex schema 严格),按 id 排序。
func TestBuildModelCatalog(t *testing.T) {
	data := BuildModelCatalog([]string{"deepseek@v4", "openai@gpt-4o", "gpt-4o"}, nil)
	var cat ModelCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		t.Fatalf("catalog must be valid JSON: %v", err)
	}
	// 只发布原生行:3 个模型 → 3 个原生 slug 行。
	if len(cat.Models) != 3 {
		t.Fatalf("catalog has %d models, want 3 (only native rows)", len(cat.Models))
	}
	// 排序:gpt-5.2 < gpt-5.3-codex < gpt-5.4(原生池字典序前 3)。
	want := []string{"gpt-5.2", "gpt-5.3-codex", "gpt-5.4"}
	for i, w := range want {
		if cat.Models[i].Slug != w {
			t.Errorf("catalog[%d].slug = %q, want %q (all=%v)", i, cat.Models[i].Slug, w, slugsOf(cat.Models))
		}
	}
	native := map[string]string{}
	for _, m := range cat.Models {
		// 不得发布模型原名的路由 id 行(含 @)。
		if strings.Contains(m.Slug, "@") {
			t.Errorf("catalog must not publish model-id rows: %s", m.Slug)
		}
		if !IsNativeOpenAISlug(m.Slug) {
			t.Errorf("slug %q not in native pool", m.Slug)
		}
		native[m.Slug] = m.DisplayName
		// 未传 windows 映射(留空)时目录条目回退默认 200K。
		if m.ContextWindow != 200000 || m.MaxContextWindow != 200000 {
			t.Errorf("%s context_window = %d/%d, want 200000 (default)", m.Slug, m.ContextWindow, m.MaxContextWindow)
		}
		if m.Visibility != "list" || m.ShellType == "" || m.SupportedReasoningLevels == nil {
			t.Errorf("%s missing required fields: %+v", m.Slug, m)
		}
	}
	// display_name 用模型 id(诚实标签)。
	if native["gpt-5.2"] != "deepseek@v4" || native["gpt-5.3-codex"] != "gpt-4o" || native["gpt-5.4"] != "openai@gpt-4o" {
		t.Errorf("native display mapping wrong: %v", native)
	}
	// 必填字段齐全(codex 模型目录参考格式)。
	for _, m := range cat.Models {
		if m.ContextWindow == 0 || m.MaxContextWindow == 0 || m.TruncationPolicy.Mode == "" ||
			m.ShellType == "" || m.Visibility == "" {
			t.Errorf("model %s missing required fields: %+v", m.Slug, m)
		}
	}
	// 确定性。
	if string(BuildModelCatalog([]string{"b", "a"}, nil)) != string(BuildModelCatalog([]string{"b", "a"}, nil)) {
		t.Error("BuildModelCatalog not deterministic")
	}
}

// 传 windows 映射时,目录条目 context_window 取模型配置值(tokens);未配置的模型
// 回退默认 200K。
func TestBuildModelCatalogContextWindow(t *testing.T) {
	models := []string{"deepseek@v4", "openai@gpt-4o", "gpt-4o"}
	windows := map[string]int{"deepseek@v4": 128000, "openai@gpt-4o": 1000000}
	data := BuildModelCatalog(models, windows)
	var cat ModelCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, m := range cat.Models {
		got[m.DisplayName] = m.ContextWindow
	}
	want := map[string]int{"deepseek@v4": 128000, "openai@gpt-4o": 1000000, "gpt-4o": 200000}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("context_window[%s] = %d, want %d (all=%v)", id, got[id], w, got)
		}
	}
}

func slugsOf(ms []ModelCatalogEntry) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Slug
	}
	return out
}

// models_cache.json 生成(桌面 app 读):外层有 fetched_at/client_version,模型齐全。
func TestBuildModelsCache(t *testing.T) {
	data := BuildModelsCache([]string{"openai@gpt-4o", "deepseek@v4"}, "0.147.0", nil)
	var cache ModelsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("models_cache must be valid JSON: %v", err)
	}
	if cache.FetchedAt == "" || cache.ClientVersion == "" {
		t.Errorf("models_cache missing header: %+v", cache)
	}
	// 只发布原生行:2 个模型 → 2 个原生 slug 行。
	if len(cache.Models) != 2 {
		t.Fatalf("models_cache has %d models, want 2", len(cache.Models))
	}
	if cache.Models[0].Slug != "gpt-5.2" || cache.Models[1].Slug != "gpt-5.3-codex" {
		t.Errorf("models_cache order = %v", slugsOf(cache.Models))
	}
}

// 模型目录写入:文件创建、内容合法、可被 codex 解析。
func TestApplyToLocalModelCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "bsrouter-models.json")
	if err := ApplyToLocalModelCatalog(path, []string{"openai@gpt-4o"}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"openai@gpt-4o"`) {
		t.Errorf("catalog missing model:\n%s", data)
	}
}

func TestApplyToLocalConfigNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "config.toml")
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "sk-new", Model: "openai@gpt-4o"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file should be created: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`model_provider = "bsrouter"`,
		`model = "openai@gpt-4o"`,
		`[model_providers.bsrouter]`,
		`name = "bsrouter"`,
		`base_url = "http://127.0.0.1:8080/api/v1"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// 密钥不进 config.toml(由 auth.json 提供)。
	if strings.Contains(got, "http_headers") {
		t.Errorf("config.toml should not embed http_headers:\n%s", got)
	}
}

// 合并进已有 config.toml:保留注释/其它表,替换 bsrouter 块,更新顶层键。
func TestApplyToLocalConfigMergeExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := `# 我的 codex 配置
model_provider = "custom"
model = "old-model"
model_reasoning_effort = "high"

[model_providers.custom]
name = "custom"
base_url = "https://opencode.ai/zen/go/v1"
wire_api = "responses"

# 旧的 bsrouter 块
[model_providers.bsrouter]
name = "bsrouter"
base_url = "http://old:1/api/v1"
wire_api = "responses"
http_headers = { Authorization = "Bearer old-key" }

[marketplaces.zz]
source = "x"
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "sk-new", Model: "gpt-5.6-luna", ReasoningEffort: "low"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	got := string(data)
	// 顶层键更新。
	if !strings.Contains(got, `model_provider = "bsrouter"`) {
		t.Errorf("model_provider not set:\n%s", got)
	}
	if !strings.Contains(got, `model = "gpt-5.6-luna"`) {
		t.Errorf("model not updated:\n%s", got)
	}
	if !strings.Contains(got, `model_reasoning_effort = "low"`) {
		t.Errorf("reasoning_effort not updated:\n%s", got)
	}
	// 头部注释保留。
	if !strings.Contains(got, "# 我的 codex 配置") {
		t.Errorf("header comment lost:\n%s", got)
	}
	// 其它 provider 表保留。
	if !strings.Contains(got, "[model_providers.custom]") || !strings.Contains(got, `base_url = "https://opencode.ai/zen/go/v1"`) {
		t.Errorf("custom provider lost:\n%s", got)
	}
	// 其它表保留。
	if !strings.Contains(got, "[marketplaces.zz]") {
		t.Errorf("marketplaces table lost:\n%s", got)
	}
	// 旧的 bsrouter 块被替换(旧 base_url 与旧 http_headers 均不在)。
	if strings.Contains(got, "http://old:1") || strings.Contains(got, "old-key") {
		t.Errorf("old bsrouter block not replaced:\n%s", got)
	}
	// 新块不再内嵌密钥(由 auth.json 提供)。
	if strings.Contains(got, "http_headers") {
		t.Errorf("new block should not embed http_headers:\n%s", got)
	}
	// 顶层键只出现一次。
	if strings.Count(got, `model_provider = "bsrouter"`) != 1 {
		t.Errorf("model_provider appears %d times:\n%s", strings.Count(got, `model_provider = "bsrouter"`), got)
	}
}

// 预设未设置 model/reasoning_effort 时删除对应顶层键(最后一次应用生效)。
func TestApplyToLocalConfigRemovesStaleTopKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := `model_provider = "bsrouter"
model = "stale-model"
model_reasoning_effort = "high"
[model_providers.bsrouter]
name = "bsrouter"
base_url = "http://x/api/v1"
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "k"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if strings.Contains(got, "stale-model") {
		t.Errorf("stale model not removed:\n%s", got)
	}
	if strings.Contains(got, "model_reasoning_effort") {
		t.Errorf("stale reasoning_effort not removed:\n%s", got)
	}
	if !strings.Contains(got, `model_provider = "bsrouter"`) {
		t.Errorf("model_provider missing:\n%s", got)
	}
}

// 顶层键出现在其它 table 内(用户笔误)时不得误改。
func TestApplyToLocalConfigIgnoresTableNestedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := `model_provider = "custom"
[model_providers.custom]
name = "custom"
model = "nested-model-not-toplevel"
[marketplaces.zz]
model = "also-not-toplevel"
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "k", Model: "new-model"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.Contains(got, `model = "new-model"`) {
		t.Errorf("top-level model missing:\n%s", got)
	}
	if !strings.Contains(got, `model = "nested-model-not-toplevel"`) {
		t.Errorf("nested model inside table modified:\n%s", got)
	}
	if !strings.Contains(got, `model = "also-not-toplevel"`) {
		t.Errorf("nested model inside marketplaces modified:\n%s", got)
	}
}

// 表头带行内注释(TOML 合法)时仍应识别为表,块与顶层键正确处理。
func TestApplyToLocalConfigInlineCommentHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := `model_provider = "custom"
model = "old"

[model_providers.custom] # 我的供应商
name = "custom"
base_url = "https://opencode.ai/zen/go/v1"

[model_providers.bsrouter] # 旧的 bsrouter 块
name = "bsrouter"
base_url = "http://old:1/api/v1"
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "sk-new"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	// 带注释的 bsrouter 块被替换为新块。
	if !strings.Contains(got, `base_url = "http://127.0.0.1:8080/api/v1"`) {
		t.Errorf("new bsrouter block missing:\n%s", got)
	}
	if strings.Contains(got, "http://old:1") {
		t.Errorf("old bsrouter block not replaced:\n%s", got)
	}
	// 顶层 model_provider 被更新。
	if strings.Count(got, `model_provider = "bsrouter"`) != 1 {
		t.Errorf("model_provider should appear once:\n%s", got)
	}
	// custom 表保留,其注释保留。
	if !strings.Contains(got, "# 我的供应商") || !strings.Contains(got, `base_url = "https://opencode.ai/zen/go/v1"`) {
		t.Errorf("custom provider lost:\n%s", got)
	}
}

// bsrouter 块不存在时追加到末尾,顶层键插到头部。
func TestApplyToLocalConfigAppendBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := `model_provider = "custom"

[model_providers.custom]
name = "custom"
base_url = "https://opencode.ai/zen/go/v1"
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "k"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	// bsrouter 块在末尾。
	bi := strings.Index(got, "[model_providers.bsrouter]")
	ci := strings.Index(got, "[model_providers.custom]")
	if bi < 0 || ci < 0 || bi < ci {
		t.Errorf("bsrouter block should be appended after custom:\n%s", got)
	}
	if !strings.Contains(got, `model_provider = "bsrouter"`) {
		t.Errorf("model_provider missing:\n%s", got)
	}
	// 顶部的原 model_provider 已被替换(不再有 custom)。
	if strings.Contains(got, `model_provider = "custom"`) {
		t.Errorf("old model_provider not replaced:\n%s", got)
	}
}

// 支持接管的原生 slug 集合:与 Desktop available_models allowlist 完全一致
// (8 个),含 gpt-5.3-codex / gpt-5.2;gpt-5.3-codex-spark 不在其中。
func TestNativeOpenAISlugs(t *testing.T) {
	slugs := NativeOpenAISlugs()
	want := []string{
		"gpt-5.2", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini",
		"gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
	}
	if len(slugs) != len(want) {
		t.Fatalf("NativeOpenAISlugs = %d, want %d; %v", len(slugs), len(want), slugs)
	}
	for i, w := range want {
		if slugs[i] != w {
			t.Errorf("slugs[%d] = %q, want %q (all=%v)", i, slugs[i], w, slugs)
		}
	}
	if !IsNativeOpenAISlug("gpt-5.3-codex") || !IsNativeOpenAISlug("gpt-5.2") {
		t.Errorf("expected supported slugs missing: %v", slugs)
	}
	if IsNativeOpenAISlug("gpt-5.3-codex-spark") || IsNativeOpenAISlug("nova-sol") || IsNativeOpenAISlug("gpt-9.9") {
		t.Errorf("unsupported slug reported as native: %v", slugs)
	}
}
