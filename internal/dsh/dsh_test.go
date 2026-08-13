package dsh

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDerivedAPIKeyEnv(t *testing.T) {
	cases := map[string]string{
		"dev":    "DEV_API_KEY",
		"term-a": "TERM_A_API_KEY",
		"":       "BSROUTER_API_KEY",
	}
	for in, want := range cases {
		if got := DerivedAPIKeyEnv(in); got != want {
			t.Errorf("DerivedAPIKeyEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{Name: "dev"}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := (Config{Name: "dev", APIKey: "sk-x", Models: []string{"a@b", "c@d"}}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty name", Config{}},
		{"slash", Config{Name: "a/b"}},
		{"newline name", Config{Name: "a\nb"}},
		{"newline key", Config{Name: "a", APIKey: "k\nboom"}},
		{"dup models", Config{Name: "a", Models: []string{"p@1", "p@1"}}},
		{"empty model", Config{Name: "a", Models: []string{"p@1", "  "}}},
	}
	for _, tc := range cases {
		if err := tc.cfg.Validate(); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}
func TestManagerCRUD(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "dsh.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: "dev", APIKey: "sk-abc"}
	if err := m.Add(cfg); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(cfg); !errors.Is(err, ErrExists) {
		t.Errorf("dup err=%v", err)
	}
	if m.Count() != 1 {
		t.Errorf("count=%d", m.Count())
	}
	got, err := m.Get("dev")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-abc" || got.CreatedAt.IsZero() {
		t.Errorf("Get=%+v", got)
	}
	upd := Config{Name: "dev", APIKey: "sk-new"}
	if err := m.Update(upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if g, _ := m.Get("dev"); g.APIKey != "sk-new" || g.CreatedAt != got.CreatedAt {
		t.Errorf("Update=%+v", g)
	}
	if err := m.Update(Config{Name: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("upd missing err=%v", err)
	}
	if err := m.Delete("dev"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("dev"); !errors.Is(err, ErrNotFound) {
		t.Errorf("del again err=%v", err)
	}
}

func TestBuildProviderBlock(t *testing.T) {
	block := BuildProviderBlock(ProviderSpec{
		Name: "bsrouter", DisplayName: "BSRouter", APIKey: "sk-secret",
		APIKeyEnv: "BSR_API_KEY", API: "anthropic-messages",
		BaseURL: "http://127.0.0.1:18154/api",
		Models:  []string{"deepseek@gpt-4o", "deepseek@v4"},
		Windows: map[string]int{"deepseek@gpt-4o": 128000},
	})
	text := renderYAML(block)
	for _, want := range []string{
		"  displayName: BSRouter", "  apiKey: sk-secret",
		"  apiKeyEnv: BSR_API_KEY", "  api: anthropic-messages",
		"  baseURL: http://127.0.0.1:18154/api", "  models:",
		"    - id: deepseek@gpt-4o", "      contextWindow: 128000",
		"      maxTokens: 65536", "    - id: deepseek@v4", "      contextWindow: 200000",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("block missing %q:\n%s", want, text)
		}
	}
}

// 真实 settings.yaml 形态:保留 opencode-go 供应商与顶层字段,只写 bsrouter。
const sampleYAML = `
ui-onboarding:
  welcomeNoticeVersion: 2026-08-13.1
llm-pi-ai:
  providers:
    opencode-go:
      models:
        - id: deepseek-v4-flash
          contextWindow: 1000000
      apiKeyEnv: OPENCODE_GO_API_KEY
agent-default-model:
  provider: opencode-go
  model: deepseek-v4-flash
`

func TestApplyToLocalSettingsOnRealShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := ProviderSpec{
		Name: "bsrouter", DisplayName: "bsrouter",
		APIKey: "sk-new", APIKeyEnv: "BSR_API_KEY",
		API: "anthropic-messages", BaseURL: "http://127.0.0.1:18154/api",
		Models:  []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		Windows: map[string]int{"deepseek-v4-flash": 1000000},
	}
	if err := ApplyToLocalSettings(path, spec); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	for _, want := range []string{
		"ui-onboarding:", "  welcomeNoticeVersion: 2026-08-13.1",
		"opencode-go:", "OPENCODE_GO_API_KEY",
		"agent-default-model:", "  provider: opencode-go",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("preserved missing %q:\n%s", want, s)
		}
	}
	for _, want := range []string{
		"    bsrouter:", "      displayName: bsrouter", "      apiKey: sk-new",
		"      apiKeyEnv: BSR_API_KEY", "      api: anthropic-messages",
		"      baseURL: http://127.0.0.1:18154/api", "      models:",
		"        - id: deepseek-v4-flash", "          contextWindow: 1000000",
		"        - id: deepseek-v4-pro",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("bsrouter missing %q:\n%s", want, s)
		}
	}
}

func TestApplyToLocalSettingsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := ProviderSpec{Name: "bsrouter", APIKeyEnv: "BSR_API_KEY", API: "anthropic-messages", BaseURL: "http://127.0.0.1:18154/api", Models: []string{"deepseek@v4"}}
	if err := ApplyToLocalSettings(path, spec); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := ApplyToLocalSettings(path, spec); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("second apply changed diff:\n%s\n---vs---\n%s", first, second)
	}
	if strings.Count(string(second), "    bsrouter:") != 1 {
		t.Errorf("bsrouter duplicated:\n%s", second)
	}
}

func TestApplyToLocalSettingsNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	spec := ProviderSpec{Name: "dev", APIKeyEnv: "DEV_API_KEY", BaseURL: "http://127.0.0.1:18154/api", Models: []string{"a@b"}}
	if err := ApplyToLocalSettings(path, spec); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	for _, want := range []string{
		"llm-pi-ai:", "  providers:", "    dev:", "      apiKeyEnv: DEV_API_KEY", "        - id: a@b",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("new file missing %q:\n%s", want, s)
		}
	}
}

func TestBuildCommand(t *testing.T) {
	cfg := Config{Name: "dev", APIKey: "sk-secret"}
	cmd := BuildCommand(cfg)
	if !strings.Contains(cmd.PowerShell, "$env:DEV_API_KEY = \"sk-secret\"") {
		t.Errorf("ps cmd = %q", cmd.PowerShell)
	}
	if !strings.Contains(cmd.Bash, "export DEV_API_KEY=\"sk-secret\"") {
		t.Errorf("bash cmd = %q", cmd.Bash)
	}
	if !strings.HasSuffix(cmd.PowerShell, "\ndsh") || !strings.HasSuffix(cmd.Bash, "\ndsh") {
		t.Errorf("missing dsh: %q / %q", cmd.PowerShell, cmd.Bash)
	}
}

func TestConfigJSON(t *testing.T) {
	cfg := Config{Name: "dev", APIKey: "sk-x", Models: []string{"a@b"}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != "dev" || back.APIKey != "sk-x" || len(back.Models) != 1 {
		t.Errorf("round trip = %+v", back)
	}
}
