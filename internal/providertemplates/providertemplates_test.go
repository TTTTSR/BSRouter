package providertemplates

import (
	"testing"

	"BSRouter/internal/provider"
)

// TestTemplatesValid 确保内置模板目录里的每个模板都能实例化为合法的供应商配置
// (字段合法、接口格式合法、display_name/base_url 齐全),避免一处模板笔误破坏接入。
// 模板不携带硬编码模型列表(接入时由 fetch-models 从服务商 API 实时拉取)。
func TestTemplatesValid(t *testing.T) {
	templates := All()
	if len(templates) == 0 {
		t.Fatal("templates catalog is empty")
	}
	seen := make(map[string]bool, len(templates))
	for _, tmpl := range templates {
		if seen[tmpl.Name] {
			t.Fatalf("duplicate template name %q", tmpl.Name)
		}
		seen[tmpl.Name] = true

		cfg := provider.Config{
			Kind:      tmpl.Kind,
			Name:      tmpl.Name,
			BaseURL:   tmpl.BaseURL,
			BasePath:  tmpl.BasePath,
			ModelsURL: tmpl.ModelsURL,
			UsageURL:  tmpl.UsageURL,
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("template %q: invalid config: %v", tmpl.Name, err)
		}
		if tmpl.DisplayName == "" || tmpl.BaseURL == "" {
			t.Errorf("template %q: display_name and base_url are required", tmpl.Name)
		}
	}
}
