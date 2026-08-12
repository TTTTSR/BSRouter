// Package group 实现模型分组系统:每个分组是面向下游的"虚拟供应商",
// 拥有独立的 API 调用 URL 与接口格式,并分配一组可供调用的模型
// (模型为全局 "{供应商名}@{模型名}" ID,转发时再解析到真实上游)。
package group

import (
	"fmt"
	"strings"

	"BSRouter/internal/provider"
)

// Config 是分组配置,也是本地 JSON 持久化的存储格式。
type Config struct {
	Name   string        `json:"name"`
	Kind   provider.Kind `json:"kind"`          // 分组对外呈现的接口格式
	URL    string        `json:"url,omitempty"` // 分组 API 基础路径,默认 "/{name}"
	Models []string      `json:"models"`        // 分配的模型列表(全局模型 ID)
}

// EffectiveURL 返回分组实际的基础路径(未配置时默认 "/api/{name}")。
func (c Config) EffectiveURL() string {
	if c.URL != "" {
		return strings.TrimRight(c.URL, "/")
	}
	return "/api/" + c.Name
}

// Validate 校验分组配置(含由名称推导的默认 URL)。
func (c Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("group: name is required")
	}
	if strings.Contains(c.Name, "/") {
		return fmt.Errorf("group: name %q must not contain '/'", c.Name)
	}
	switch c.Kind {
	case provider.KindAnthropic, provider.KindCompletion, provider.KindResponses:
	default:
		return fmt.Errorf("group %q: unknown kind %q", c.Name, c.Kind)
	}
	for _, m := range c.Models {
		if m == "" {
			return fmt.Errorf("group %q: model name must not be empty", c.Name)
		}
	}
	// 校验最终生效的 URL(含未配置时由名称推导的默认 URL)。
	// 分组虚拟供应商统一位于 /api 下,且不占用 /api/v1 保留段(统一 API)。
	u := c.EffectiveURL()
	if !strings.HasPrefix(u, "/api/") {
		return fmt.Errorf("group %q: url must be under /api", c.Name)
	}
	if rest := strings.TrimPrefix(u, "/api/"); rest == "v1" || strings.HasPrefix(rest, "v1/") {
		return fmt.Errorf("group %q: url must not use the reserved /api/v1 namespace", c.Name)
	}
	if strings.Contains(u, "..") || strings.ContainsAny(u, " \t") || strings.Contains(u, "//") {
		return fmt.Errorf("group %q: url must not contain whitespace, '..' or '//'", c.Name)
	}
	return nil
}
