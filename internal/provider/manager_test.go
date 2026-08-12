package provider

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func baseAnthropic(name string) Config {
	return Config{Kind: KindAnthropic, Name: name, BaseURL: "https://api.anthropic.com", APIKey: "sk-an", Models: []ModelConfig{{Name: "claude-sonnet-4-5"}}}
}

func TestManagerCRUD(t *testing.T) {
	file := filepath.Join(t.TempDir(), "providers.json")
	m, err := NewManager(file)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := len(m.List()); got != 0 {
		t.Fatalf("initial providers = %d, want 0", got)
	}

	// Add
	if err := m.Add(baseAnthropic("anthropic")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 重复添加
	if err := m.Add(baseAnthropic("anthropic")); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate Add err = %v, want ErrExists", err)
	}

	// Get
	p, err := m.Get("anthropic")
	if err != nil || p.Name() != "anthropic" {
		t.Fatalf("Get = %+v, %v", p, err)
	}
	if _, err := m.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing err = %v, want ErrNotFound", err)
	}

	// Update
	upd := baseAnthropic("anthropic")
	upd.APIKey = "sk-new"
	if err := m.Update(upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if p, _ := m.Get("anthropic"); p.Config().APIKey != "sk-new" {
		t.Errorf("after update api_key = %q", p.Config().APIKey)
	}
	if err := m.Update(baseAnthropic("missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update missing err = %v, want ErrNotFound", err)
	}

	// List
	if got := len(m.List()); got != 1 {
		t.Errorf("List len = %d, want 1", got)
	}

	// Delete
	if err := m.Delete("anthropic"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.Delete("anthropic"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete again err = %v, want ErrNotFound", err)
	}
}

func TestManagerPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "providers.json")
	m, err := NewManager(file)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Add(baseAnthropic("a")); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(Config{Kind: KindCompletion, Name: "b", BaseURL: "https://api.openai.com", APIKey: "sk-b"}); err != nil {
		t.Fatal(err)
	}

	// 重新加载同一文件,应还原全部配置。
	m2, err := NewManager(file)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m2.List(); len(got) != 2 {
		t.Fatalf("reloaded providers = %d, want 2", len(got))
	}
	if got, _ := m2.Get("a"); got.Config().Kind != KindAnthropic || got.Config().BaseURL != "https://api.anthropic.com" {
		t.Errorf("reloaded a = %+v", got.Config())
	}
	if got, _ := m2.Get("b"); got.Config().Kind != KindCompletion {
		t.Errorf("reloaded b kind = %+v", got.Config().Kind)
	}

	// 删除后重新加载,应持久化删除。
	if err := m2.Delete("a"); err != nil {
		t.Fatal(err)
	}
	m3, err := NewManager(file)
	if err != nil {
		t.Fatalf("reload after delete: %v", err)
	}
	if got := m3.List(); len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("after delete reload = %+v", got)
	}
}

func TestManagerMissingFile(t *testing.T) {
	// 文件不存在时视为空配置,不报错。
	m, err := NewManager(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if len(m.List()) != 0 {
		t.Fatalf("expected empty, got %+v", m.List())
	}
}

func TestManagerCorruptFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(file); err == nil {
		t.Fatal("expected error for corrupt config")
	}
}

// 持久化失败时应返回 ErrPersist,且内存态回滚。
func TestManagerPersistRollback(t *testing.T) {
	file := filepath.Join(t.TempDir(), "missing", "providers.json") // 目录不存在,写入必然失败
	m, err := NewManager(file)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Add(baseAnthropic("a")); !errors.Is(err, ErrPersist) {
		t.Fatalf("Add err = %v, want ErrPersist", err)
	}
	if _, err := m.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after failed Add = %v, want ErrNotFound (rolled back)", err)
	}
}

// SetModels 只更新模型列表,保留其余字段。
func TestManagerSetModels(t *testing.T) {
	file := filepath.Join(t.TempDir(), "providers.json")
	m, err := NewManager(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Add(baseAnthropic("a")); err != nil {
		t.Fatal(err)
	}
	if err := m.SetModels("a", []ModelConfig{{Name: "x"}, {Name: "y", Kind: KindResponses}}); err != nil {
		t.Fatalf("SetModels: %v", err)
	}
	p, err := m.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Models()) != 2 || p.Models()[0].Name != "x" || p.Models()[1].Name != "y" || p.Models()[1].Kind != KindResponses {
		t.Errorf("models = %v", p.Models())
	}
	if p.Config().APIKey != baseAnthropic("a").APIKey || p.Config().BaseURL != baseAnthropic("a").BaseURL {
		t.Errorf("SetModels changed unrelated fields: %+v", p.Config())
	}
	if err := m.SetModels("missing", []ModelConfig{{Name: "x"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetModels missing err = %v, want ErrNotFound", err)
	}
}

func TestStripContextMarker(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":                 "gpt-4o",
		"gpt-4o[1M]":             "gpt-4o",
		"deepseek-v4-flash[1m]":  "deepseek-v4-flash",
		"deepseek-v4-flash[2M]":  "deepseek-v4-flash",
		"openai@gpt-4o[1M]":      "openai@gpt-4o",
		"a[1M]":                  "a",
		"x[10m]":                 "x",           // 任意 [Nk]/[Nm] 数字标记被剥离
		"y[3M]":                  "y",           // 大小写不敏感
		"deepseek-v4-flash[128k]": "deepseek-v4-flash", // 按上下文窗口派生的 [Nk] 后缀
		"deepseek-v4-flash[200k]": "deepseek-v4-flash",
		"deepseek-v4-flash[1000000]": "deepseek-v4-flash", // 裸数字也视为标记
		"z[foo]":                 "z[foo]",      // 非数字标记不剥
		"z[1.5k]":                "z[1.5k]",     // 非法数字不剥
		"plain":                  "plain",
	}
	for in, want := range cases {
		if got := StripContextMarker(in); got != want {
			t.Errorf("StripContextMarker(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{
		{Kind: KindAnthropic, Name: "a", BaseURL: "http://a"},
		{Kind: KindCompletion, Name: "a-b", BaseURL: "http://ab"},
		{Kind: KindResponses, Name: "b", BaseURL: "http://b"},
	} {
		if err := m.Add(cfg); err != nil {
			t.Fatal(err)
		}
	}

	// 按首个 @ 精确切分;供应商名可含 "-"。
	p, model, err := m.Resolve("a-b@gpt-4o")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != "a-b" || model != "gpt-4o" {
		t.Errorf("Resolve(a-b@gpt-4o) = %s / %s", p.Name(), model)
	}

	p, model, err = m.Resolve("a@claude-sonnet-4-5")
	if err != nil || p.Name() != "a" || model != "claude-sonnet-4-5" {
		t.Errorf("Resolve(a@claude-sonnet-4-5) = %s / %s, err=%v", p.Name(), model, err)
	}

	p, model, err = m.Resolve("b@gpt-5")
	if err != nil || p.Name() != "b" || model != "gpt-5" {
		t.Errorf("Resolve(b@gpt-5) = %s / %s, err=%v", p.Name(), model, err)
	}

	// 带 [1M] 上下文标记的模型名会先被剥离再解析。
	p, model, err = m.Resolve("a-b@gpt-4o[1M]")
	if err != nil || p.Name() != "a-b" || model != "gpt-4o" {
		t.Errorf("Resolve(a-b@gpt-4o[1M]) = %s / %s, err=%v", p.Name(), model, err)
	}

	if _, _, err := m.Resolve("unknown@gpt-4o"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve(unknown) err = %v, want ErrNotFound", err)
	}
	// 无 @ 的裸名不是合成 id。
	if _, _, err := m.Resolve("gpt-4o"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve(bare) err = %v, want ErrNotFound", err)
	}
}
