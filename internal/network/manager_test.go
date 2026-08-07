package network

import (
	"path/filepath"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"valid", Config{EgressHost: "1.2.3.4", EgressPort: "443"}, true},
		{"valid domain", Config{EgressHost: "gw.example.com", EgressPort: "443"}, true},
		{"empty port defaults 80 at Set", Config{EgressHost: "1.2.3.4"}, true},
		{"empty host", Config{EgressPort: "443"}, false},
		{"scheme rejected", Config{EgressHost: "http://1.2.3.4", EgressPort: "443"}, false},
		{"path rejected", Config{EgressHost: "1.2.3.4/api", EgressPort: "443"}, false},
		{"port with colon rejected", Config{EgressHost: "1.2.3.4", EgressPort: "1.2.3.4:443"}, false},
		{"bad port", Config{EgressHost: "1.2.3.4", EgressPort: "abc"}, false},
		{"port zero", Config{EgressHost: "1.2.3.4", EgressPort: "0"}, false},
		{"port too big", Config{EgressHost: "1.2.3.4", EgressPort: "70000"}, false},
	}
	for _, c := range cases {
		err := c.cfg.Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: Validate(%+v) err = %v, want ok=%v", c.name, c.cfg, err, c.ok)
		}
	}
}

func TestManagerSetPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "network.json")
	m, err := NewManager(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get(); got.EgressHost != "" {
		t.Fatalf("initial Get = %+v, want empty", got)
	}

	// Set 空端口 → 默认 80 并持久化。
	if err := m.Set(Config{EgressHost: "1.2.3.4"}); err != nil {
		t.Fatal(err)
	}
	got := m.Get()
	if got.EgressHost != "1.2.3.4" || got.EgressPort != "80" {
		t.Fatalf("Get after Set = %+v, want host=1.2.3.4 port=80", got)
	}

	// 重载后仍在。
	m2, err := NewManager(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.Get(); got.EgressHost != "1.2.3.4" || got.EgressPort != "80" {
		t.Errorf("after reload Get = %+v", got)
	}

	// 非法配置:Set 报错且内存态不被污染。
	if err := m.Set(Config{EgressHost: "http://bad", EgressPort: "443"}); err == nil {
		t.Error("invalid Set should fail")
	}
	if got := m.Get(); got.EgressHost != "1.2.3.4" {
		t.Errorf("Get after failed Set = %+v, want unchanged", got)
	}
}
