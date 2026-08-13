// Package providertemplates 提供内置的供应商接入模板目录:整理主流大模型服务商的
// base_url / 接口格式 / 模型列表 URL,用户只需填入 api_key 即可接入。模板**不包含
// 硬编码的模型列表**——接入时由网关从服务商 models 端点实时拉取(见
// POST /manage/v1/fetch-models),避免模板模型过期/与实际账户权限不符。
// 模板随二进制经 go:embed 嵌入,不依赖网络或外部文件。
package providertemplates

import (
	_ "embed"
	"encoding/json"
	"sort"

	"BSRouter/internal/provider"
)

// Template 是单个供应商模板:除 api_key 留空外,其余字段可直接实例化为 provider.Config。
// 不含硬编码模型列表——接入时由 fetch-models 端点从服务商 models URL 实时拉取。
type Template struct {
	Name        string        `json:"name"`         // 路由 slug(供应商名,即 {name}@{model} 前缀)
	DisplayName string        `json:"display_name"` // 人类可读名称
	Category    string        `json:"category"`     // international / chinese / aggregator / cloud
	Description string        `json:"description,omitempty"`
	Kind        provider.Kind `json:"kind"` // 默认接口格式(anthropic / completion / responses)
	BaseURL     string        `json:"base_url"`
	BasePath    string        `json:"base_path,omitempty"` // base_url 与端点的路径段,留空回退 /v1
	ModelsURL   string        `json:"models_url,omitempty"`
	UsageURL    string        `json:"usage_url,omitempty"`
	Note        string        `json:"note,omitempty"` // 接入提示/注意事项(密钥获取方式、特殊要求等)
}

//go:embed templates.json
var templatesJSON []byte

// templates 解析后的模板目录,init 时一次性加载并排序。
var templates []Template

func init() {
	if err := json.Unmarshal(templatesJSON, &templates); err != nil {
		panic("providertemplates: parse templates.json: " + err.Error())
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].Name < templates[j].Name })
}

// All 返回全部模板(按名称排序的副本,避免调用方修改内部切片)。
func All() []Template {
	out := make([]Template, len(templates))
	copy(out, templates)
	return out
}
