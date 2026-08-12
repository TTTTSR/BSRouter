package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextSuffix(t *testing.T) {
	cases := map[int]string{
		128:   "[128k]",
		200:   "[200k]",
		100:   "[100k]",
		1000:  "[1m]",
		2000:  "[2m]",
		4000:  "[4m]",
		1500:  "[1500k]",
		0:     "",
		-5:    "",
	}
	for k, want := range cases {
		if got := ContextSuffix(k); got != want {
			t.Errorf("ContextSuffix(%d) = %q, want %q", k, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{Name: "dev", BaseURL: "http://127.0.0.1:8080/api", Model: "m"}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := (Config{Name: "dev", BaseURL: "https://host/api", APIKey: "k"}).Validate(); err != nil {
		t.Fatalf("valid config with api_key rejected: %v", err)
	}

	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty name", Config{BaseURL: "http://x"}},
		{"slash name", Config{Name: "a/b", BaseURL: "http://x"}},
		{"empty base_url", Config{Name: "a"}},
		{"file base_url", Config{Name: "a", BaseURL: "file:///tmp/x"}},
		{"ftp base_url", Config{Name: "a", BaseURL: "ftp://x"}},
		{"both auth", Config{Name: "a", BaseURL: "http://x", APIKey: "k", AuthToken: "t"}},
		{"newline in base_url", Config{Name: "a", BaseURL: "http://x\nboom"}},
		{"newline in model", Config{Name: "a", BaseURL: "http://x", Model: "m\nboom"}},
		{"extra_env empty key", Config{Name: "a", BaseURL: "http://x", ExtraEnv: map[string]string{"": "v"}}},
		{"extra_env bad key", Config{Name: "a", BaseURL: "http://x", ExtraEnv: map[string]string{"A-B": "v"}}},
		{"extra_env reserved", Config{Name: "a", BaseURL: "http://x", ExtraEnv: map[string]string{"ANTHROPIC_MODEL": "v"}}},
		{"extra_env newline value", Config{Name: "a", BaseURL: "http://x", ExtraEnv: map[string]string{"A": "v\nx"}}},
	}
	for _, tc := range cases {
		if err := tc.cfg.Validate(); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestManagerCRUD(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: "dev", BaseURL: "http://127.0.0.1:8080/api", AuthToken: "tok-abc"}

	if err := m.Add(cfg); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(cfg); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate Add err = %v, want ErrExists", err)
	}
	if err := m.Add(Config{Name: "x"}); err == nil {
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

	// Update 保留 CreatedAt。
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

	// List 排序。
	if lst := m.List(); len(lst) != 1 || lst[0].Name != "dev" {
		t.Errorf("List = %+v", lst)
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
	file := filepath.Join(t.TempDir(), "claude.json")
	m, _ := NewManager(file)
	if err := m.Add(Config{Name: "dev", BaseURL: "http://127.0.0.1:8080/api", APIKey: "sk-secret"}); err != nil {
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

func TestEscapeSpecial(t *testing.T) {
	// 输入含双引号、$、反斜杠与反引号。
	v := `sk-"x$y\z` + "`" + `w`
	if got := escapePS(v); got != "sk-`\"x`$y\\z``w" {
		t.Errorf("escapePS = %q, want %q", got, "sk-`\"x`$y\\z``w")
	}
	if got := escapeSh(v); got != `sk-\"x\$y\\z\`+"`"+`w` {
		t.Errorf("escapeSh = %q, want %q", got, `sk-\"x\$y\\z\`+"`"+`w`)
	}
}

// 黄金用例:全字段,含需转义的特殊字符,精确整串断言。
func TestBuildCommandGolden(t *testing.T) {
	cfg := Config{
		Name:       "dev",
		BaseURL:    "http://127.0.0.1:8080/api",
		APIKey:     `sk-"x$y\z`,
		Model:      "deepseek-v4-flash[1M]",
		HaikuModel: "deepseek-v4-flash",
		ExtraEnv:   map[string]string{"B_ROUTER_TAG": `a"b$c\e`},
	}
	cmd := BuildCommand(cfg)

	wantPS := "$env:ANTHROPIC_BASE_URL = \"http://127.0.0.1:8080/api\"\n" +
		"$env:ANTHROPIC_API_KEY = \"sk-`\"x`$y\\z\"\n" +
		"$env:ANTHROPIC_MODEL = \"deepseek-v4-flash[1M]\"\n" +
		"$env:ANTHROPIC_DEFAULT_HAIKU_MODEL = \"deepseek-v4-flash\"\n" +
		"$env:B_ROUTER_TAG = \"a`\"b`$c\\e\"\n" +
		"Remove-Item Env:ANTHROPIC_AUTH_TOKEN -ErrorAction SilentlyContinue\n" +
		"claude"
	if cmd.PowerShell != wantPS {
		t.Errorf("PowerShell:\n got:\n%s\nwant:\n%s", cmd.PowerShell, wantPS)
	}

	wantBash := "export ANTHROPIC_BASE_URL=\"http://127.0.0.1:8080/api\"\n" +
		"export ANTHROPIC_API_KEY=\"sk-\\\"x\\$y\\\\z\"\n" +
		"export ANTHROPIC_MODEL=\"deepseek-v4-flash[1M]\"\n" +
		"export ANTHROPIC_DEFAULT_HAIKU_MODEL=\"deepseek-v4-flash\"\n" +
		"export B_ROUTER_TAG=\"a\\\"b\\$c\\\\e\"\n" +
		"unset ANTHROPIC_AUTH_TOKEN\n" +
		"claude"
	if cmd.Bash != wantBash {
		t.Errorf("Bash:\n got:\n%s\nwant:\n%s", cmd.Bash, wantBash)
	}
}

// auth_token 模式 + 档位 _MODEL/_NAME 顺序 + DISABLE_AUTOUPDATER。
func TestBuildCommandAuthTokenTiers(t *testing.T) {
	cfg := Config{
		Name:              "prod",
		BaseURL:           "https://host/api/term-b",
		AuthToken:         "tok-abc",
		Model:             "anthropic-claude-sonnet-4-5",
		FableModel:        "anthropic-claude-fable-4-5",
		OpusModel:         "anthropic-claude-opus-4-5[1M]",
		OpusModelName:     "anthropic-claude-opus-4-5",
		SubagentModel:     "deepseek-v4-flash",
		DisableAutoupdater: true,
	}
	cmd := BuildCommand(cfg)

	for _, line := range []string{
		`$env:ANTHROPIC_BASE_URL = "https://host/api/term-b"`,
		`$env:ANTHROPIC_AUTH_TOKEN = "tok-abc"`,
		`$env:ANTHROPIC_MODEL = "anthropic-claude-sonnet-4-5"`,
		`$env:ANTHROPIC_DEFAULT_FABLE_MODEL = "anthropic-claude-fable-4-5"`,
		`$env:ANTHROPIC_DEFAULT_OPUS_MODEL = "anthropic-claude-opus-4-5[1M]"`,
		`$env:ANTHROPIC_DEFAULT_OPUS_MODEL_NAME = "anthropic-claude-opus-4-5"`,
		`$env:CLAUDE_CODE_SUBAGENT_MODEL = "deepseek-v4-flash"`,
		`$env:DISABLE_AUTOUPDATER = "1"`,
		"Remove-Item Env:ANTHROPIC_API_KEY -ErrorAction SilentlyContinue",
	} {
		if !strings.Contains(cmd.PowerShell, line) {
			t.Errorf("PowerShell missing %q\nfull:\n%s", line, cmd.PowerShell)
		}
	}
	// _NAME 在 _MODEL 之后。
	mi := strings.Index(cmd.PowerShell, "ANTHROPIC_DEFAULT_OPUS_MODEL =")
	ni := strings.Index(cmd.PowerShell, "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME =")
	if mi < 0 || ni < 0 || ni < mi {
		t.Errorf("OPUS _NAME must follow _MODEL (model@%d name@%d)", mi, ni)
	}
	// 未使用的 ANTHROPIC_API_KEY 不发出,且被清理。
	if strings.Contains(cmd.PowerShell, "ANTHROPIC_API_KEY =") {
		t.Error("ANTHROPIC_API_KEY should not be emitted in auth_token mode")
	}

	for _, line := range []string{
		`export ANTHROPIC_AUTH_TOKEN="tok-abc"`,
		`export ANTHROPIC_DEFAULT_FABLE_MODEL="anthropic-claude-fable-4-5"`,
		`export ANTHROPIC_DEFAULT_OPUS_MODEL_NAME="anthropic-claude-opus-4-5"`,
		`export DISABLE_AUTOUPDATER="1"`,
		"unset ANTHROPIC_API_KEY",
	} {
		if !strings.Contains(cmd.Bash, line) {
			t.Errorf("Bash missing %q\nfull:\n%s", line, cmd.Bash)
		}
	}
}

// 仅 base_url(无鉴权):两个鉴权变量都被清理,防止继承父 shell。
func TestBuildCommandOnlyBaseURL(t *testing.T) {
	cmd := BuildCommand(Config{Name: "x", BaseURL: "http://127.0.0.1:8080/api"})
	wantPS := "$env:ANTHROPIC_BASE_URL = \"http://127.0.0.1:8080/api\"\n" +
		"Remove-Item Env:ANTHROPIC_API_KEY -ErrorAction SilentlyContinue\n" +
		"Remove-Item Env:ANTHROPIC_AUTH_TOKEN -ErrorAction SilentlyContinue\n" +
		"claude"
	if cmd.PowerShell != wantPS {
		t.Errorf("PowerShell:\n got:\n%s\nwant:\n%s", cmd.PowerShell, wantPS)
	}
	wantBash := "export ANTHROPIC_BASE_URL=\"http://127.0.0.1:8080/api\"\n" +
		"unset ANTHROPIC_API_KEY\n" +
		"unset ANTHROPIC_AUTH_TOKEN\n" +
		"claude"
	if cmd.Bash != wantBash {
		t.Errorf("Bash:\n got:\n%s\nwant:\n%s", cmd.Bash, wantBash)
	}
}

// extra_env 多键按字典序排序,命令确定。
func TestBuildCommandExtraEnvSorted(t *testing.T) {
	cfg := Config{Name: "x", BaseURL: "http://x", APIKey: "k",
		ExtraEnv: map[string]string{"Z": "1", "A": "2", "M": "3"}}
	cmd := BuildCommand(cfg)
	za := strings.Index(cmd.Bash, "export Z=")
	aa := strings.Index(cmd.Bash, "export A=")
	ma := strings.Index(cmd.Bash, "export M=")
	if !(aa >= 0 && ma >= 0 && za >= 0 && aa < ma && ma < za) {
		t.Errorf("extra_env not sorted A<M<Z:\n%s", cmd.Bash)
	}
}

func TestBuildCommandIdempotent(t *testing.T) {
	cfg := Config{Name: "x", BaseURL: "http://x", APIKey: "k",
		ExtraEnv: map[string]string{"A": "1", "B": "2"}}
	a := BuildCommand(cfg)
	b := BuildCommand(cfg)
	if a != b {
		t.Error("BuildCommand not deterministic")
	}
}

func TestEnvVars(t *testing.T) {
	cfg := Config{
		BaseURL:    "http://127.0.0.1:8080/api",
		APIKey:     "sk-secret",
		Model:      "openai@gpt-4o",
		HaikuModel: "deepseek-v4-flash[1M]",
		ExtraEnv:   map[string]string{"B_ROUTER_TAG": "dev"},
	}
	env := cfg.EnvVars()
	want := map[string]string{
		"ANTHROPIC_BASE_URL":            "http://127.0.0.1:8080/api",
		"ANTHROPIC_API_KEY":             "sk-secret",
		"ANTHROPIC_MODEL":               "openai@gpt-4o",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL": "deepseek-v4-flash[1M]",
		"B_ROUTER_TAG":                  "dev",
	}
	if len(env) != len(want) {
		t.Errorf("EnvVars = %v, want %v", env, want)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("EnvVars[%q] = %q, want %q", k, env[k], v)
		}
	}
	// 未使用的鉴权变量不应出现。
	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Error("AUTH_TOKEN should not be present when using API_KEY")
	}
}

func TestApplyToLocalSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "theme": "auto",
  "env": {
    "ANTHROPIC_API_KEY": "old-key",
    "ANTHROPIC_BASE_URL": "https://old",
    "DISABLE_AUTOUPDATER": "1"
  },
  "hooks": {"Stop": []}
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		BaseURL:   "http://127.0.0.1:8080/api/term-a",
		AuthToken: "sk-new",
		Model:     "openai@gpt-4o",
	}
	if err := ApplyToLocalSettings(path, cfg); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	// 其余字段保留。
	if out["theme"] != "auto" {
		t.Errorf("theme = %v, want auto", out["theme"])
	}
	if _, ok := out["hooks"]; !ok {
		t.Error("hooks should be preserved")
	}
	envBlock, _ := out["env"].(map[string]any)
	if envBlock == nil {
		t.Fatal("env block missing")
	}
	if envBlock["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8080/api/term-a" {
		t.Errorf("base_url = %v", envBlock["ANTHROPIC_BASE_URL"])
	}
	if envBlock["ANTHROPIC_AUTH_TOKEN"] != "sk-new" {
		t.Errorf("auth_token = %v", envBlock["ANTHROPIC_AUTH_TOKEN"])
	}
	if envBlock["ANTHROPIC_MODEL"] != "openai@gpt-4o" {
		t.Errorf("model = %v", envBlock["ANTHROPIC_MODEL"])
	}
	// 未涉及的 env 键保留。
	if envBlock["DISABLE_AUTOUPDATER"] != "1" {
		t.Errorf("DISABLE_AUTOUPDATER = %v, want preserved", envBlock["DISABLE_AUTOUPDATER"])
	}
	// 使用 auth_token 时清理未用的 api_key。
	if _, ok := envBlock["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY should be removed when using auth_token")
	}
}

func TestApplyToLocalSettingsCreatesMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api", APIKey: "k"}
	if err := ApplyToLocalSettings(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file should be created: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	envBlock, _ := out["env"].(map[string]any)
	if envBlock == nil || envBlock["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8080/api" {
		t.Errorf("created env = %v", out["env"])
	}
}
