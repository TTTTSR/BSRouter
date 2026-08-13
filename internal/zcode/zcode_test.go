package zcode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	if err := (Config{Name: "dev"}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := (Config{Name: "dev", APIKey: "sk-abc"}).Validate(); err != nil {
		t.Fatalf("valid config with api_key rejected: %v", err)
	}
	// 合法 models(无数量上限,每个非空)。
	if err := (Config{Name: "dev", Models: []string{"deepseek@a", "deepseek@b", "opencode-go@gpt-5.6-luna"}}).Validate(); err != nil {
		t.Fatalf("valid models rejected: %v", err)
	}

	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty name", Config{Models: []string{"p@1"}}},
		{"slash name", Config{Name: "a/b"}},
		{"newline name", Config{Name: "a\nb"}},
		{"newline api_key", Config{Name: "a", APIKey: "k\nboom"}},
		{"models empty item", Config{Name: "a", Models: []string{"p@1", "  "}}},
		{"models whitespace", Config{Name: "a", Models: []string{"p@1", "p@ 2"}}},
		{"models duplicate", Config{Name: "a", Models: []string{"p@1", "p@1"}}},
	}
	for _, tc := range cases {
		if err := tc.cfg.Validate(); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestManagerCRUD(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "zcode.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: "dev", APIKey: "sk-abc"}

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
	if got.APIKey != "sk-abc" || got.CreatedAt.IsZero() {
		t.Errorf("Get = %+v", got)
	}
	if _, err := m.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing err = %v, want ErrNotFound", err)
	}

	// Update 保留原创建时间;空密钥保留原值由 server 层负责,这里只验证覆盖。
	upd := Config{Name: "dev", APIKey: "sk-new"}
	if err := m.Update(upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if g, _ := m.Get("dev"); g.APIKey != "sk-new" || g.CreatedAt != got.CreatedAt {
		t.Errorf("Update = %+v", g)
	}
	if err := m.Update(Config{Name: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update missing err = %v, want ErrNotFound", err)
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

func TestBuildProvider(t *testing.T) {
	entry := BuildProvider(ProviderSpec{
		Name: ProviderNameOpenAI, Kind: DefaultKind, BaseURL: "http://127.0.0.1:18154/api/v1",
		Models: []string{"openai@gpt-4o", "deepseek@v4"}, Windows: map[string]int{"openai@gpt-4o": 128000},
	}, "sk-secret")
	data, _ := json.Marshal(entry)
	var got struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Source string `json:"source"`
		Opts   struct {
			APIKey         string `json:"apiKey"`
			BaseURL        string `json:"baseURL"`
			APIKeyRequired bool   `json:"apiKeyRequired"`
			WireAPI        string `json:"wire_api"`
		} `json:"options"`
		Models map[string]struct {
			Limit      struct{ Context int } `json:"limit"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "bsrouter-openai" || got.Kind != "openai-compatible" || got.Source != "custom" {
		t.Errorf("entry = %s", data)
	}
	if got.Opts.BaseURL != "http://127.0.0.1:18154/api/v1" || got.Opts.APIKey != "sk-secret" || !got.Opts.APIKeyRequired || got.Opts.WireAPI != "" {
		t.Errorf("options = %+v", got.Opts)
	}
	// 模型手动配置写入 models map:limit.context 取配置窗口,未配置回退默认 200k。
	if got.Models["openai@gpt-4o"].Limit.Context != 128000 {
		t.Errorf("context = %d, want 128000", got.Models["openai@gpt-4o"].Limit.Context)
	}
	if got.Models["deepseek@v4"].Limit.Context != 200000 {
		t.Errorf("default context = %d, want 200000", got.Models["deepseek@v4"].Limit.Context)
	}
	if len(got.Models["deepseek@v4"].Modalities.Input) != 1 || got.Models["deepseek@v4"].Modalities.Input[0] != "text" {
		t.Errorf("modalities = %+v", got.Models["deepseek@v4"].Modalities)
	}
	// 空密钥时 apiKeyRequired 置 false(网关未鉴权)。
	entry2 := BuildProvider(ProviderSpec{Name: ProviderName, Kind: DefaultKind, BaseURL: "http://x/api/v1", Models: []string{"a@b"}, Windows: nil}, "")
	d2, _ := json.Marshal(entry2)
	var e2 struct {
		Opts struct{ APIKeyRequired bool } `json:"options"`
	}
	_ = json.Unmarshal(d2, &e2)
	if e2.Opts.APIKeyRequired {
		t.Error("empty apiKey should set apiKeyRequired=false")
	}
	// 显式 kind(anthropic)透传;此时 base_url 不带 /v1。
	entry3 := BuildProvider(ProviderSpec{Name: ProviderNameAnthropic, Kind: KindAnthropic, BaseURL: "http://127.0.0.1:18154/api"}, "sk-secret")
	d3, _ := json.Marshal(entry3)
	var e3 struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(d3, &e3)
	if e3.Kind != "anthropic" {
		t.Errorf("kind = %q, want anthropic", e3.Kind)
	}
	// responses 供应商:openai-compatible + wire_api=responses。
	entry4 := BuildProvider(ProviderSpec{Name: ProviderNameResponses, Kind: DefaultKind, WireAPI: WireAPIResponses, BaseURL: "http://127.0.0.1:18154/api/v1", Models: []string{"codex@gpt-5"}, Windows: nil}, "sk")
	d4, _ := json.Marshal(entry4)
	var e4 struct {
		Kind string `json:"kind"`
		Opts struct{ WireAPI string `json:"wire_api"` } `json:"options"`
	}
	_ = json.Unmarshal(d4, &e4)
	if e4.Kind != "openai-compatible" || e4.Opts.WireAPI != "responses" {
		t.Errorf("responses entry kind=%q wire_api=%q, want openai-compatible/responses", e4.Kind, e4.Opts.WireAPI)
	}
}

func TestApplyToLocalConfigNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".zcode", "v2", "config.json")
	err := ApplyToLocalConfig(path, "sk-secret", []ProviderSpec{{
		Name: ProviderName, Kind: DefaultKind, BaseURL: "http://127.0.0.1:18154/api/v1",
		Models: []string{"openai@gpt-4o"}, Windows: map[string]int{"openai@gpt-4o": 128000},
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, data)
	}
	providers, ok := cfg["provider"].(map[string]any)
	if !ok {
		t.Fatalf("missing provider map:\n%s", data)
	}
	entry, ok := providers["bsrouter"].(map[string]any)
	if !ok {
		t.Fatalf("missing bsrouter provider:\n%s", data)
	}
	if entry["name"] != "bsrouter" || entry["kind"] != "openai-compatible" || entry["source"] != "custom" {
		t.Errorf("bsrouter entry = %+v", entry)
	}
}

func TestApplyToLocalConfigPreservesOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// 预置:一个内置供应商(UUID 键的 bsrouter 旧条目)+ 顶层其它字段。
	existing := `{
  "theme": "dark",
  "provider": {
    "builtin:zai": {"name": "Z.ai", "kind": "anthropic", "options": {"baseURL": "https://api.z.ai/api/anthropic"}, "source": "custom", "models": {}},
    "321b22cb-b8cf-4e56-8c5b-e44304384d66": {"name": "bsrouter", "kind": "openai-compatible", "options": {"apiKey": "old", "baseURL": "http://old/api/v1"}, "source": "custom", "models": {}}
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ApplyToLocalConfig(path, "sk-new", []ProviderSpec{{
		Name: ProviderName, Kind: DefaultKind, BaseURL: "http://127.0.0.1:18154/api/v1",
		Models: []string{"a@b"}, Windows: nil,
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	// 顶层其它字段保留。
	if cfg["theme"] != "dark" {
		t.Errorf("theme lost:\n%s", data)
	}
	providers := cfg["provider"].(map[string]any)
	// 内置供应商保留。
	if providers["builtin:zai"] == nil {
		t.Errorf("builtin:zai lost:\n%s", data)
	}
	// 旧 UUID 键的 bsrouter 被移除,只剩固定键 bsrouter(幂等,无重复)。
	if providers["321b22cb-b8cf-4e56-8c5b-e44304384d66"] != nil {
		t.Errorf("stale UUID-keyed bsrouter not removed:\n%s", data)
	}
	entry, ok := providers["bsrouter"].(map[string]any)
	if !ok {
		t.Fatalf("bsrouter missing after apply:\n%s", data)
	}
	opts := entry["options"].(map[string]any)
	if opts["apiKey"] != "sk-new" || opts["baseURL"] != "http://127.0.0.1:18154/api/v1" {
		t.Errorf("bsrouter options = %+v", opts)
	}
	// 只有 2 个供应商(内置 + bsrouter),无残留重复。
	if len(providers) != 2 {
		t.Errorf("provider count = %d, want 2:\n%s", len(providers), data)
	}
}

func TestApplyToLocalConfigStripsBOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// 带 UTF-8 BOM 的现有文件(Windows 工具常加)。
	body := "\xef\xbb\xbf{\"provider\": {\"builtin:zai\": {\"name\": \"Z.ai\", \"kind\": \"anthropic\"}}}"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyToLocalConfig(path, "", []ProviderSpec{{Name: ProviderName, Kind: DefaultKind, BaseURL: "http://x/api/v1", Models: []string{"a@b"}, Windows: nil}}); err != nil {
		t.Fatalf("apply with BOM: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.HasPrefix(string(data), "\xef\xbb\xbf") {
		t.Errorf("output still has BOM")
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, data)
	}
	if cfg["provider"].(map[string]any)["builtin:zai"] == nil {
		t.Errorf("builtin:zai lost after BOM-strip apply:\n%s", data)
	}
}

// 多供应商 apply:统一 API 分割的三个供应商(openai/anthropic/responses)逐条写入,
// 旧的 bsrouter* 键(单/多)清理,无关供应商保留;responses 供应商带 wire_api=responses。
func TestApplyToLocalConfigMultipleProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	existing := `{"provider":{
	  "bsrouter": {"name":"bsrouter","kind":"openai-compatible","models":{}},
	  "bsrouter-anthropic": {"name":"bsrouter-anthropic","kind":"anthropic","models":{}},
	  "keep": {"name":"keep","kind":"anthropic","models":{}}
	}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ApplyToLocalConfig(path, "sk", []ProviderSpec{
		{Name: ProviderNameOpenAI, Kind: DefaultKind, BaseURL: "http://127.0.0.1:18154/api/v1", Models: []string{"openai@gpt-4o"}, Windows: nil},
		{Name: ProviderNameAnthropic, Kind: KindAnthropic, BaseURL: "http://127.0.0.1:18154/api", Models: []string{"anthropic@claude"}, Windows: nil},
		{Name: ProviderNameResponses, Kind: DefaultKind, WireAPI: WireAPIResponses, BaseURL: "http://127.0.0.1:18154/api/v1", Models: []string{"codex@gpt-5"}, Windows: nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	providers := cfg["provider"].(map[string]any)
	for _, want := range []string{ProviderNameOpenAI, ProviderNameAnthropic, ProviderNameResponses} {
		if providers[want] == nil {
			t.Errorf("missing provider %q:\n%s", want, data)
		}
	}
	// 旧的 bsrouter 键清理(多供应商键在本次也重建),无关供应商保留。
	if providers["bsrouter"] != nil || providers["keep"] == nil {
		t.Errorf("stale bsrouter / lost keep:\n%s", data)
	}
	// responses 供应商 options.wire_api = responses。
	opts := providers[ProviderNameResponses].(map[string]any)["options"].(map[string]any)
	if opts["wire_api"] != WireAPIResponses {
		t.Errorf("wire_api = %v, want %q", opts["wire_api"], WireAPIResponses)
	}
}
