package group

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"BSRouter/internal/provider"
)

func baseGroup(name string) Config {
	return Config{Name: name, Kind: provider.KindCompletion, Models: []string{"openai@gpt-4o"}}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"ok default url", Config{Name: "a", Kind: provider.KindCompletion, Models: []string{"x"}}, true},
		{"empty name", Config{Name: "", Kind: provider.KindCompletion}, false},
		{"slash in name", Config{Name: "a/b", Kind: provider.KindCompletion}, false},
		{"unknown kind", Config{Name: "a", Kind: "nope"}, false},
		{"empty model", Config{Name: "a", Kind: provider.KindCompletion, Models: []string{""}}, false},
		{"url not under api", Config{Name: "a", Kind: provider.KindCompletion, URL: "/team-a"}, false},
		{"url no leading slash", Config{Name: "a", Kind: provider.KindCompletion, URL: "api/team-a"}, false},
		{"url is api root", Config{Name: "a", Kind: provider.KindCompletion, URL: "/api"}, false},
		{"url api v1 reserved", Config{Name: "a", Kind: provider.KindCompletion, URL: "/api/v1/team"}, false},
		{"url with dotdot", Config{Name: "a", Kind: provider.KindCompletion, URL: "/api/a/../b"}, false},
		{"ok custom url", Config{Name: "a", Kind: provider.KindCompletion, URL: "/api/a"}, true},
		{"ok nested url", Config{Name: "a", Kind: provider.KindCompletion, URL: "/api/some/team-a"}, true},
		// 默认 URL 由名称推导,同样要过 /api 与保留段规则。
		{"name v1 derives reserved url", Config{Name: "v1", Kind: provider.KindCompletion, Models: []string{"x"}}, false},
		{"name with space", Config{Name: "a b", Kind: provider.KindCompletion, Models: []string{"x"}}, false},
		{"name with dotdot", Config{Name: "a..b", Kind: provider.KindCompletion, Models: []string{"x"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.cfg.Validate(); (err == nil) != c.ok {
				t.Errorf("Validate() err = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

func TestEffectiveURL(t *testing.T) {
	if got := (Config{Name: "team-a"}).EffectiveURL(); got != "/api/team-a" {
		t.Errorf("default url = %q, want /api/team-a", got)
	}
	if got := (Config{Name: "team-a", URL: "/api/team-a"}).EffectiveURL(); got != "/api/team-a" {
		t.Errorf("custom url = %q", got)
	}
	if got := (Config{Name: "team-a", URL: "/api/team-a/"}).EffectiveURL(); got != "/api/team-a" {
		t.Errorf("trailing slash url = %q, want trimmed", got)
	}
}

func TestManagerCRUD(t *testing.T) {
	file := filepath.Join(t.TempDir(), "groups.json")
	m, err := NewManager(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 0 {
		t.Fatalf("initial groups = %d, want 0", len(m.List()))
	}
	if err := m.Add(baseGroup("a")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(baseGroup("a")); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate Add err = %v, want ErrExists", err)
	}
	g, err := m.Get("a")
	if err != nil || g.Name != "a" {
		t.Fatalf("Get = %+v, %v", g, err)
	}
	if _, err := m.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing err = %v, want ErrNotFound", err)
	}
	upd := baseGroup("a")
	upd.Models = []string{"openai@gpt-5"}
	if err := m.Update(upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if g, _ := m.Get("a"); len(g.Models) != 1 || g.Models[0] != "openai@gpt-5" {
		t.Errorf("after update = %+v", g)
	}
	if err := m.Update(baseGroup("missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update missing err = %v, want ErrNotFound", err)
	}
	if err := m.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.Delete("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete again err = %v, want ErrNotFound", err)
	}
}

func TestManagerPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "groups.json")
	m, err := NewManager(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Add(Config{Name: "a", Kind: provider.KindCompletion, URL: "/api/team-a", Models: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(baseGroup("b")); err != nil {
		t.Fatal(err)
	}

	m2, err := NewManager(file)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m2.List(); len(got) != 2 {
		t.Fatalf("reloaded groups = %d, want 2", len(got))
	}
	if g, _ := m2.Get("a"); g.URL != "/api/team-a" {
		t.Errorf("a url = %q", g.URL)
	}
	// 默认 URL 已归一化并持久化。
	if g, _ := m2.Get("b"); g.URL != "/api/b" {
		t.Errorf("b default url = %q, want /api/b", g.URL)
	}
}

func TestManagerCorruptFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "groups.json")
	if err := os.WriteFile(file, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(file); err == nil {
		t.Fatal("expected error for corrupt config")
	}
}

// 重复的分组名在配置文件中应直接报错,而非静默后覆盖。
func TestManagerDuplicateNameLoad(t *testing.T) {
	file := filepath.Join(t.TempDir(), "groups.json")
	body := `[{"name":"a","kind":"completion","models":["x"]},
	          {"name":"a","kind":"anthropic","models":["y"]}]`
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(file); err == nil {
		t.Fatal("expected error for duplicate group names")
	}
}

// 相同或互为 /api 下 v1 边界前缀的分组 URL 应被拒绝。
func TestManagerURLCollision(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Add(Config{Name: "a", Kind: provider.KindCompletion, URL: "/api/team-a", Models: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	// 完全相同 -> 拒绝。
	if err := m.Add(Config{Name: "b", Kind: provider.KindAnthropic, URL: "/api/team-a", Models: []string{"y"}}); err == nil {
		t.Error("expected URL collision error for identical url")
	}
	// 互为 /api 下 v1 边界前缀 -> 拒绝。
	if err := m.Add(Config{Name: "c", Kind: provider.KindCompletion, URL: "/api/team-a/v1", Models: []string{"y"}}); err == nil {
		t.Error("expected URL collision error for prefix url")
	}
	// 边界不同(非路径前缀)-> 允许。
	if err := m.Add(Config{Name: "d", Kind: provider.KindCompletion, URL: "/api/team-ab", Models: []string{"y"}}); err != nil {
		t.Errorf("expected ok for /api/team-ab, got %v", err)
	}
	// Update 换到冲突 URL -> 拒绝。
	if err := m.Update(Config{Name: "d", Kind: provider.KindCompletion, URL: "/api/team-a", Models: []string{"y"}}); err == nil {
		t.Error("expected URL collision error on update")
	}
}

func TestResolveURL(t *testing.T) {
	m, _ := NewManager(filepath.Join(t.TempDir(), "groups.json"))
	if err := m.Add(Config{Name: "a", Kind: provider.KindCompletion, URL: "/api/team-a", Models: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(Config{Name: "b", Kind: provider.KindAnthropic, URL: "/api/team-a/sub", Models: []string{"y"}}); err != nil {
		t.Fatal(err)
	}

	// 最长前缀。
	g, rest, ok := m.ResolveURL("/api/team-a/v1/chat/completions")
	if !ok || g.Name != "a" || rest != "/v1/chat/completions" {
		t.Errorf("ResolveURL(/api/team-a/v1/chat/completions) = %s / %s / %v", g.Name, rest, ok)
	}
	g, rest, ok = m.ResolveURL("/api/team-a/sub/v1/messages")
	if !ok || g.Name != "b" || rest != "/v1/messages" {
		t.Errorf("ResolveURL(/api/team-a/sub/v1/messages) = %s / %s / %v", g.Name, rest, ok)
	}
	// 路径边界:/api/team-ab 不应匹配 /api/team-a。
	if _, _, ok := m.ResolveURL("/api/team-ab/v1/chat/completions"); ok {
		t.Error("ResolveURL(/api/team-ab/...) should not match /api/team-a")
	}
	// 恰好等于 URL。
	g, rest, ok = m.ResolveURL("/api/team-a")
	if !ok || g.Name != "a" || rest != "/" {
		t.Errorf("ResolveURL(/api/team-a) = %s / %s / %v", g.Name, rest, ok)
	}
	// 未匹配。
	if _, _, ok := m.ResolveURL("/nope/v1/x"); ok {
		t.Error("ResolveURL(/nope/...) should not match")
	}
}
