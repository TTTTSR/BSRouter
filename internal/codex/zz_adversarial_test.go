package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func advRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

// 表头带行尾注释时,块内键不应被当作顶层键修改。
func TestAdversarialHeaderWithTrailingComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "[model_providers.custom] # my custom\nmodel = \"nested-model\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "k", Model: "new-model"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	got := advRead(t, path)
	if !strings.Contains(got, `model = "nested-model"`) {
		t.Errorf("nested model inside commented-header table was overwritten:\n%s", got)
	}
	if !strings.Contains(got, `model = "new-model"`) {
		t.Errorf("top-level model missing:\n%s", got)
	}
}

// present=false 时,注释表头块内的 model 键应保留(不误删)。
func TestAdversarialHeaderCommentDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "[model_providers.custom] # my custom\nmodel = \"nested-model\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "k"} // Model 空 → 删除顶层 model
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	got := advRead(t, path)
	if !strings.Contains(got, `model = "nested-model"`) {
		t.Errorf("nested model inside commented-header table was DELETED:\n%s", got)
	}
}

// bsrouter 块表头带行尾注释时,应仍被识别并替换,而非追加重复块。
func TestAdversarialBsrouterHeaderComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "[model_providers.bsrouter] # old block\nname = \"bsrouter\"\nbase_url = \"http://old:1/api/v1\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "sk-new"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	got := advRead(t, path)
	if strings.Count(got, "[model_providers.bsrouter]") != 1 {
		t.Errorf("bsrouter block duplicated (old + new):\n%s", got)
	}
	if !strings.Contains(got, `base_url = "http://127.0.0.1:8080/api/v1"`) {
		t.Errorf("new base_url missing:\n%s", got)
	}
	if strings.Contains(got, "http://old:1") {
		t.Errorf("old base_url not replaced:\n%s", got)
	}
}

// [[数组表]] 形态的 bsrouter 块也应被替换。
func TestAdversarialArrayOfTablesBsrouter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "[[model_providers.bsrouter]]\nname = \"bsrouter\"\nbase_url = \"http://old:1/api/v1\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "sk-new"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	got := advRead(t, path)
	if strings.Count(got, "[model_providers.bsrouter]") != 1 {
		t.Errorf("array-of-tables bsrouter not replaced:\n%s", got)
	}
	if !strings.Contains(got, `base_url = "http://127.0.0.1:8080/api/v1"`) {
		t.Errorf("new base_url missing:\n%s", got)
	}
}

// 顶层 model 键带行尾注释,应更新该行(注释会丢,但只留一个键)。
func TestAdversarialTopKeyWithComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "model = \"old\" # keep this model\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "k", Model: "new"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	got := advRead(t, path)
	if strings.Count(got, `model = "new"`) != 1 {
		t.Errorf("model not updated:\n%s", got)
	}
}

// 顶层键后紧跟其它表/数组表头,表内键不得被当顶层键处理。
func TestAdversarialArrayTableNestedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "model_provider = \"custom\"\n[[other.table]]\nmodel = \"nested\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "k", Model: "new"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	got := advRead(t, path)
	if !strings.Contains(got, `model = "new"`) {
		t.Errorf("top-level model missing:\n%s", got)
	}
	// 数组表内的 model 不是顶层键,应原样保留。
	if !strings.Contains(got, `model = "nested"`) {
		t.Errorf("nested model in [[other.table]] lost:\n%s", got)
	}
}

// CRLF 文件:顶层键仍正确更新,不产生残留。
func TestAdversarialCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "model_provider = \"custom\"\r\nmodel = \"old\"\r\n[model_providers.custom]\r\nname = \"custom\"\r\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://127.0.0.1:8080/api/v1", APIKey: "k", Model: "new"}
	if err := ApplyToLocalConfig(path, cfg, ""); err != nil {
		t.Fatal(err)
	}
	got := advRead(t, path)
	if strings.Contains(got, `model = "old"`) {
		t.Errorf("old model not updated (CRLF file):\n%s", got)
	}
	if !strings.Contains(got, `model = "new"`) {
		t.Errorf("new model missing (CRLF file):\n%s", got)
	}
	if !strings.Contains(got, `model_provider = "bsrouter"`) {
		t.Errorf("model_provider not set (CRLF file):\n%s", got)
	}
}
