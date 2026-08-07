// Package provider 管理网关的上游供应商:供应商配置(基类 + 三种接口格式具体实现)、
// 增删改查与本地 JSON 持久化,以及 "{供应商名}-{模型名}" 的模型路由解析。
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"BSRouter/internal/gateway"
)

// Kind 表示供应商使用的接口格式。
type Kind string

const (
	KindAnthropic  Kind = "anthropic"
	KindCompletion Kind = "completion"
	KindResponses  Kind = "responses"
)

// maxUpstreamBody 限制读取上游响应体的上限。
const maxUpstreamBody = 100 << 20 // 100 MB

// ErrNotConfigured 表示执行操作所需的配置未设置(如 usage_url)。
var ErrNotConfigured = errors.New("provider: not configured")

// ModelConfig 是供应商下的一个模型:可单独指定接口格式,留空表示使用供应商默认 Kind。
type ModelConfig struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind,omitempty"`
}

// UnmarshalJSON 兼容新对象形式 {"name":...,"kind":...} 与旧格式的纯字符串模型名,
// 使旧版 providers.json("models":["gpt-4o"])在升级后仍可加载。
func (m *ModelConfig) UnmarshalJSON(data []byte) error {
	var obj struct {
		Name string `json:"name"`
		Kind Kind   `json:"kind"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		m.Name = obj.Name
		m.Kind = obj.Kind
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		m.Name = s
		return nil
	}
	return fmt.Errorf("invalid model config: %s", data)
}

// validKind 判断是否为受支持的接口格式。
func validKind(k Kind) bool {
	switch k {
	case KindAnthropic, KindCompletion, KindResponses:
		return true
	}
	return false
}

// Config 是供应商配置基类,也是 JSON 持久化的存储格式。
type Config struct {
	Kind      Kind          `json:"kind"`
	Name      string        `json:"name"`
	BaseURL   string        `json:"base_url"`
	APIKey    string        `json:"api_key"`
	Models    []ModelConfig `json:"models,omitempty"`
	UsageURL  string        `json:"usage_url,omitempty"`  // 用量查询接口 URL(可选)
	ModelsURL string        `json:"models_url,omitempty"` // 模型列表接口 URL(可选,默认 {base}/v1/models)
}

// Validate 校验配置字段。
func (c Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("provider: name is required")
	}
	if strings.Contains(c.Name, "/") {
		return fmt.Errorf("provider: name %q must not contain '/'", c.Name)
	}
	// "@" 是供应商名与模型名的分隔保留字,禁止出现,避免路由/聚合歧义。
	if strings.Contains(c.Name, "@") {
		return fmt.Errorf("provider: name %q must not contain '@'", c.Name)
	}
	if c.BaseURL == "" {
		return fmt.Errorf("provider %q: base_url is required", c.Name)
	}
	if !validKind(c.Kind) {
		return fmt.Errorf("provider %q: unknown kind %q", c.Name, c.Kind)
	}
	seen := make(map[string]bool, len(c.Models))
	for _, m := range c.Models {
		if m.Name == "" {
			return fmt.Errorf("provider %q: model name must not be empty", c.Name)
		}
		if strings.Contains(m.Name, "@") {
			return fmt.Errorf("provider %q: model name %q must not contain '@'", c.Name, m.Name)
		}
		if seen[m.Name] {
			return fmt.Errorf("provider %q: duplicate model name %q", c.Name, m.Name)
		}
		seen[m.Name] = true
		if m.Kind != "" && !validKind(m.Kind) {
			return fmt.Errorf("provider %q: model %q: unknown kind %q", c.Name, m.Name, m.Kind)
		}
	}
	// 可选的探针 URL 必须是合法的 http(s) 地址,避免 file:// 等非网络协议。
	for name, u := range map[string]string{"usage_url": c.UsageURL, "models_url": c.ModelsURL} {
		if u == "" {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("provider %q: %s must be a valid http(s) URL", c.Name, name)
		}
	}
	return nil
}

// BaseProvider 是供应商实现:持有公共配置、三种格式的上游适配器与探测能力,
// 并按请求模型的实际接口格式派发转发。
type BaseProvider struct {
	cfg        Config
	httpc      *http.Client
	anthropic  *gateway.AnthropicProvider
	completion *gateway.CompletionProvider
	responses  *gateway.ResponsesProvider
}

// Name 返回供应商名。
func (b *BaseProvider) Name() string { return b.cfg.Name }

// Kind 返回供应商默认的接口格式。
func (b *BaseProvider) Kind() Kind { return b.cfg.Kind }

// ModelKind 返回指定模型实际使用的接口格式:模型自带则用之,否则回退供应商默认。
func (b *BaseProvider) ModelKind(model string) Kind {
	for _, m := range b.cfg.Models {
		if m.Name == model && m.Kind != "" {
			return m.Kind
		}
	}
	return b.cfg.Kind
}

// Models 返回该供应商的模型列表(含各自的接口格式)。
func (b *BaseProvider) Models() []ModelConfig { return b.cfg.Models }

// Config 返回供应商配置。
func (b *BaseProvider) Config() Config { return b.cfg }

// Complete 实现 gateway.Provider 接口:按请求模型的实际接口格式派发到对应上游适配器。
// 约定:调用方需先把 req.Model 去掉 "{供应商名}-" 前缀,传入裸模型名;
// 否则 ModelKind 会回退到供应商默认格式,且前缀会被原样发给上游。
func (b *BaseProvider) Complete(ctx context.Context, req *gateway.Request) (*gateway.Response, error) {
	p, err := b.adapterFor(req.Model)
	if err != nil {
		return nil, err
	}
	return p.Complete(ctx, req)
}

// Stream 实现 gateway.Provider 接口:按模型实际格式派发到上游适配器的流式接口。
func (b *BaseProvider) Stream(ctx context.Context, req *gateway.Request) (io.ReadCloser, error) {
	p, err := b.adapterFor(req.Model)
	if err != nil {
		return nil, err
	}
	return p.Stream(ctx, req)
}

// adapterFor 返回请求模型对应的格式适配器。
func (b *BaseProvider) adapterFor(model string) (gateway.Provider, error) {
	switch b.ModelKind(model) {
	case KindAnthropic:
		return b.anthropic, nil
	case KindCompletion:
		return b.completion, nil
	case KindResponses:
		return b.responses, nil
	default:
		return nil, fmt.Errorf("provider %q: unsupported kind for model %q", b.cfg.Name, model)
	}
}

// modelsURL 返回模型列表接口地址:优先使用配置的 ModelsURL,否则按接口格式默认。
func (b *BaseProvider) modelsURL() string {
	if b.cfg.ModelsURL != "" {
		return b.cfg.ModelsURL
	}
	return strings.TrimRight(b.cfg.BaseURL, "/") + "/v1/models"
}

// applyAuth 按接口格式为请求设置鉴权头。
func (b *BaseProvider) applyAuth(req *http.Request) {
	if b.cfg.Kind == KindAnthropic {
		req.Header.Set("x-api-key", b.cfg.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.APIKey)
}

// PingResult 是连通性测试的结果。
type PingResult struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// Ping 测试与上游的连通性(可达性 + 鉴权),对模型列表接口发一次 GET。
func (b *BaseProvider) Ping(ctx context.Context) (*PingResult, error) {
	result := &PingResult{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.modelsURL(), nil)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	b.applyAuth(req)
	start := time.Now()
	resp, err := b.httpc.Do(req)
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	defer resp.Body.Close()
	// 排空响应体以便连接复用(1 MB 覆盖正常模型列表规模)。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	result.StatusCode = resp.StatusCode
	result.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !result.OK {
		result.Error = fmt.Sprintf("upstream returned %d", resp.StatusCode)
	}
	return result, nil
}

// FetchModels 从模型列表接口拉取可用模型(兼容 {"data":[{"id":...}]} 结构)。
func (b *BaseProvider) FetchModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	b.applyAuth(req)
	resp, err := b.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncate(data))
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("parse model list: %w", err)
	}
	models := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

// QueryUsage 从配置的 UsageURL 查询用量;未配置时返回 ErrNotConfigured。
func (b *BaseProvider) QueryUsage(ctx context.Context) (json.RawMessage, error) {
	if b.cfg.UsageURL == "" {
		return nil, ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.UsageURL, nil)
	if err != nil {
		return nil, err
	}
	b.applyAuth(req)
	resp, err := b.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncate(data))
	}
	return json.RawMessage(data), nil
}

// truncate 截断上游响应体,用于错误信息。
func truncate(b []byte) string {
	const max = 500
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// Provider 是供应商接口:嵌入已有的 gateway.Provider(即 Complete),并暴露元信息
// 与上游探测能力(连通性 / 模型列表 / 用量查询)。
type Provider interface {
	gateway.Provider
	Name() string
	Kind() Kind
	ModelKind(model string) Kind
	Models() []ModelConfig
	Config() Config
	Ping(ctx context.Context) (*PingResult, error)
	FetchModels(ctx context.Context) ([]string, error)
	QueryUsage(ctx context.Context) (json.RawMessage, error)
}

// New 根据配置构造供应商实现(内部持有三种格式的上游适配器,按模型实际格式派发)。
func New(cfg Config) (Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	base := &BaseProvider{cfg: cfg, httpc: &http.Client{Timeout: 120 * time.Second}}
	client := &gateway.Client{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, HTTP: base.httpc}
	base.anthropic = gateway.NewAnthropicProvider(client)
	base.completion = gateway.NewCompletionProvider(client)
	base.responses = gateway.NewResponsesProvider(client)
	return base, nil
}
