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

// ModelConfig 是供应商下的一个模型:可单独指定一个或多个支持的接口格式,
// 留空表示使用供应商默认 Kind。Kinds(多格式)优先于 Kind(旧单格式字段)。
// ContextWindow 为该模型的上下文窗口(k 为单位,如 128 表示 128k;0 留空 = 默认 200k),
// 应用 Claude Code / Codex 预设时据此生成模型名后缀与目录条目窗口。
type ModelConfig struct {
	Name          string `json:"name"`
	Kind          Kind   `json:"kind,omitempty"`
	Kinds         []Kind `json:"kinds,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
}

// UnmarshalJSON 兼容新对象形式 {"name":...,"kind":...,"kinds":[...],"context_window":...}
// 与旧格式的纯字符串模型名,使旧版 providers.json("models":["gpt-4o"])在升级后仍可加载。
func (m *ModelConfig) UnmarshalJSON(data []byte) error {
	var obj struct {
		Name          string `json:"name"`
		Kind          Kind   `json:"kind"`
		Kinds         []Kind `json:"kinds"`
		ContextWindow int    `json:"context_window"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		m.Name = obj.Name
		m.Kind = obj.Kind
		m.Kinds = obj.Kinds
		m.ContextWindow = obj.ContextWindow
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
	BasePath  string        `json:"base_path,omitempty"`  // base_url 与端点之间的路径段,留空回退 "/v1"
	APIKey    string        `json:"api_key"`
	Models    []ModelConfig `json:"models,omitempty"`
	UsageURL  string        `json:"usage_url,omitempty"`  // 用量查询接口 URL(可选)
	ModelsURL string        `json:"models_url,omitempty"` // 模型列表接口 URL(可选,默认 {base}{base_path}/models)
	// 故障阻塞的上游错误码覆盖(nil = 用默认:余额不足 402、限流 429;0 = 禁用该分类阻塞;
	// 其余 4xx/5xx = 该状态码)。用于适配返回码非标准的供应商(如 5 小时限额的 codingplan 用 429)。
	RateLimitStatus           *int  `json:"rate_limit_status,omitempty"`           // 限流错误码(默认 429)
	RateLimitEnabled          *bool `json:"rate_limit_enabled,omitempty"`          // 限流阻塞开关(默认启用;false 禁用)
	RateLimitDurationMinutes  int   `json:"rate_limit_duration_minutes,omitempty"` // 限流阻塞时长(分钟;0 默认 120)
	InsufficientBalanceStatus *int  `json:"insufficient_balance_status,omitempty"` // 余额不足错误码(默认 402)
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
	// base_path 是 base_url 与端点之间的路径段(留空回退 /v1),须以 "/" 开头。
	if c.BasePath != "" && !strings.HasPrefix(c.BasePath, "/") {
		return fmt.Errorf("provider %q: base_path must start with '/'", c.Name)
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
		for _, k := range m.Kinds {
			if !validKind(k) {
				return fmt.Errorf("provider %q: model %q: unknown kind %q", c.Name, m.Name, k)
			}
		}
		if m.ContextWindow < 0 {
			return fmt.Errorf("provider %q: model %q: context_window must be >= 0", c.Name, m.Name)
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
	// 阻塞错误码覆盖:0 = 禁用,其余必须是 4xx/5xx(HTTP 错误码)。
	for name, v := range map[string]*int{"rate_limit_status": c.RateLimitStatus, "insufficient_balance_status": c.InsufficientBalanceStatus} {
		if v != nil && *v != 0 && (*v < 400 || *v > 599) {
			return fmt.Errorf("provider %q: %s must be 0 (disabled) or an HTTP error code (400-599)", c.Name, name)
		}
	}
	// 限流阻塞时长(分钟):0 = 默认 120,不允许负数。
	if c.RateLimitDurationMinutes < 0 {
		return fmt.Errorf("provider %q: rate_limit_duration_minutes must be >= 0 (0 uses default 120)", c.Name)
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

// ModelKinds 返回指定模型支持的接口格式集合(优先级顺序,去重):
// 模型级 Kinds 优先,其次模型级 Kind(旧单格式字段),最后回退供应商默认 Kind。恒返回 ≥1 项。
func (b *BaseProvider) ModelKinds(model string) []Kind {
	var kinds []Kind
	for _, m := range b.cfg.Models {
		if m.Name != model {
			continue
		}
		if len(m.Kinds) > 0 {
			kinds = m.Kinds
		} else if m.Kind != "" {
			kinds = []Kind{m.Kind}
		}
		break
	}
	if len(kinds) == 0 {
		kinds = []Kind{b.cfg.Kind}
	}
	// 去重保序。
	out := make([]Kind, 0, len(kinds))
	seen := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// ModelKind 返回指定模型实际使用的接口格式:多格式时取第一个(转换路径选上游格式)。
func (b *BaseProvider) ModelKind(model string) Kind {
	return b.ModelKinds(model)[0]
}

// Supports 判断模型是否支持请求的接口格式(直通判定用)。
func (b *BaseProvider) Supports(model string, format Kind) bool {
	for _, k := range b.ModelKinds(model) {
		if k == format {
			return true
		}
	}
	return false
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

// CompleteRaw 以指定接口格式发送原始 wire 请求体并返回原始响应体与上游状态码(不解析)。
// 供直通路径使用:模型支持该格式时,请求体不经中间层转换,原样转发。
func (b *BaseProvider) CompleteRaw(ctx context.Context, format Kind, raw json.RawMessage) (int, []byte, error) {
	switch format {
	case KindAnthropic:
		return b.anthropic.CompleteRaw(ctx, raw)
	case KindCompletion:
		return b.completion.CompleteRaw(ctx, raw)
	case KindResponses:
		return b.responses.CompleteRaw(ctx, raw)
	default:
		return 0, nil, fmt.Errorf("provider %q: unsupported kind %q", b.cfg.Name, format)
	}
}

// StreamRaw 以指定接口格式发送原始流式请求体并返回上游 SSE 响应体(调用方负责 Close)。
func (b *BaseProvider) StreamRaw(ctx context.Context, format Kind, raw json.RawMessage) (*http.Response, error) {
	switch format {
	case KindAnthropic:
		return b.anthropic.StreamRaw(ctx, raw)
	case KindCompletion:
		return b.completion.StreamRaw(ctx, raw)
	case KindResponses:
		return b.responses.StreamRaw(ctx, raw)
	default:
		return nil, fmt.Errorf("provider %q: unsupported kind %q", b.cfg.Name, format)
	}
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

// modelsURL 返回模型列表接口地址:优先使用配置的 ModelsURL,否则按 base_path 拼接
// 默认 {base}{base_path}/models(默认 base_path 为 /v1)。
func (b *BaseProvider) modelsURL() string {
	if b.cfg.ModelsURL != "" {
		return b.cfg.ModelsURL
	}
	return gateway.JoinPath(b.cfg.BaseURL, b.cfg.BasePath, "/models")
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

// Provider 是供应商接口:嵌入已有的 gateway.Provider(即 Complete),并暴露元信息、
// 接口格式查询与上游探测能力(连通性 / 模型列表 / 用量查询)。CompleteRaw/StreamRaw
// 供直通路径使用(模型支持请求格式时原样转发,不经中间层转换)。
type Provider interface {
	gateway.Provider
	Name() string
	Kind() Kind
	ModelKinds(model string) []Kind
	ModelKind(model string) Kind
	Supports(model string, format Kind) bool
	Models() []ModelConfig
	Config() Config
	CompleteRaw(ctx context.Context, format Kind, raw json.RawMessage) (int, []byte, error)
	StreamRaw(ctx context.Context, format Kind, raw json.RawMessage) (*http.Response, error)
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
	client := &gateway.Client{BaseURL: cfg.BaseURL, BasePath: cfg.BasePath, APIKey: cfg.APIKey, HTTP: base.httpc}
	base.anthropic = gateway.NewAnthropicProvider(client)
	base.completion = gateway.NewCompletionProvider(client)
	base.responses = gateway.NewResponsesProvider(client)
	return base, nil
}
