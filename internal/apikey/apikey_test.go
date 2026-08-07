package apikey

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerCRUD(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}

	k, err := m.Generate("team-a")
	if err != nil {
		t.Fatal(err)
	}
	// 格式: sk- + 64 位 a-zA-Z0-9。
	if !strings.HasPrefix(k.Key, "sk-") || len(k.Key) != 3+64 {
		t.Errorf("key format = %q (len=%d)", k.Key, len(k.Key))
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for _, c := range k.Key[3:] {
		if !strings.ContainsRune(alphabet, c) {
			t.Errorf("bad char in key: %q", c)
		}
	}

	// 同名冲突。
	if _, err := m.Generate("team-a"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate Generate err = %v, want ErrExists", err)
	}
	// 空名/斜杠名。
	if _, err := m.Generate("  "); err == nil {
		t.Error("empty name should fail")
	}
	if _, err := m.Generate("a/b"); err == nil {
		t.Error("slash name should fail")
	}

	if !m.Valid(k.Key) {
		t.Error("Valid should be true for generated key")
	}
	if m.Valid("sk-wrong") {
		t.Error("Valid should be false for wrong key")
	}
	if m.Valid("") {
		t.Error("Valid should be false for empty")
	}
	if m.Count() != 1 {
		t.Errorf("count = %d, want 1", m.Count())
	}

	if err := m.Delete("team-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.Delete("team-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete again err = %v, want ErrNotFound", err)
	}
	if m.Valid(k.Key) {
		t.Error("deleted key should be invalid")
	}
}

func TestManagerPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "keys.json")
	m, _ := NewManager(file)
	k, err := m.Generate("a")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(file)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !m2.Valid(k.Key) {
		t.Error("reloaded key should still be valid")
	}
}
