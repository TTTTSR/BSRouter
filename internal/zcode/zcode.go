// Package zcode 管理 Z.ai zcode 运行配置预设:一条预设对应一套 zcode 供应商配置。
// zcode 是 Z.ai 的 opencode 派生编码智能体(桌面 App + CLI),其配置为本地
// ~/.zcode/v2/config.json(Windows %USERPROFILE%\.zcode\v2\config.json),顶层是一个
// `provider` map,每个供应商一条(display name、wire kind、options.baseURL/apiKey、
// 模型 map)。本包实现把 BSRouter 作为自定义供应商覆盖进该配置:保留其余内置/
// 自定义供应商与顶层其它字段,只写 bsrouter* 系列。apply-local 按模型原生接口格式
// 分割为多条供应商(openai-compatible / anthropic / responses),全部走网关统一 API
// 入口,让 zcode 里每种格式的模型都走原生 wire 连接(不经网关转换)。
//
// zcode 的模型列表是**手动配置**在 config.json 里的(不自动获取),apply-local 必须
// 把预设配置的模型(留空回退网关全部可路由模型)逐条写入各供应商 models map,每条
// 含 limit.context(按模型上下文窗口换算的 tokens)。预设配置虚拟供应商本身
// (api_key)与模型列表,入口固定为网关统一 API。
//
// 与 Claude Code / Codex 预设不同:zcode 的鉴权不在环境变量而在 config.json 的
// options.apiKey,也没有 `-c` 覆盖或 env 注入的启动命令,因此不生成一键启动命令,
// 只做"覆盖本地配置"(apply-local)——最后一次应用生效,写入后启动 zcode 即生效。
package zcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound 表示预设不存在。
	ErrNotFound = errors.New("zcode: not found")
	// ErrExists 表示同名预设已存在。
	ErrExists = errors.New("zcode: already exists")
	// ErrPersist 表示配置写入本地 JSON 失败。
	ErrPersist = errors.New("zcode: persist failed")
)

// 供应商键名/接口格式常量。apply-local 按模型原生格式分割为
// ProviderNameOpenAI/Anthropic/Responses 三条,让 zcode 里每种格式的模型都走原生
// wire 连接(不经网关转换);ProviderName 仅作旧版单供应商(手填 base_url)的兼容。
const (
	// ProviderName 是旧版手填 base_url 预设回退时写的供应商键名/显示名。
	ProviderName = "bsrouter"
	// ProviderNameOpenAI 是 openai-compatible 供应商的键名。
	ProviderNameOpenAI = "bsrouter-openai"
	// ProviderNameAnthropic 是 anthropic 供应商的键名。
	ProviderNameAnthropic = "bsrouter-anthropic"
	// ProviderNameResponses 是 responses 供应商的键名。
	ProviderNameResponses = "bsrouter-responses"
	// KindAnthropic 是 zcode 的 anthropic 接口格式:base_url 不带 /v1
	// (zcode 会拼接 /v1/messages)。
	KindAnthropic = "anthropic"
	// WireAPIResponses 是 openai-compatible 供应商 options 的 wire_api 取值
	// (responses),使 zcode 走 /responses 端点。
	WireAPIResponses = "responses"
)

// DefaultKind 是写入 zcode 供应商条目的默认接口格式(openai-compatible):zcode
// 的 openai-compatible 会向 baseURL 追加 /chat/completions,对应 BSRouter 统一 API
// 的 /api/v1/... 路径。anthropic 模型写 KindAnthropic,此时 base_url 不带 /v1
// (zcode 会拼接 /v1/messages)。
const DefaultKind = "openai-compatible"

// defaultContextTokens 是模型未配置上下文窗口时写入 limit.context 的默认值
// (200k tokens,与 BSRouter 对未配置窗口模型的 200k 默认一致)。
const defaultContextTokens = 200000

// Config 是一条 zcode 运行配置预设,也是本地 JSON 持久化的存储格式。
// 预设只配置虚拟供应商本身(api_key)与模型列表;入口固定为网关统一 API
// (apply-local 按模型原生接口格式分割为多个供应商)。
type Config struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// APIKey 是写入 options.apiKey 的鉴权密钥(Bearer;掩码返回)。可选:留空时
	// apply-local 注入系统默认 key(与 Claude/Codex 预设同一机制)。
	APIKey string `json:"api_key,omitempty"`
	// Models 是预设直接配置的模型列表(网关可路由 id:"{供应商}@{模型}" 合成或
	// 聚合裸名)。zcode 的模型列表手动配置在 config.json,apply-local 按模型原生
	// 接口格式分割写入各供应商;留空回退网关全部可路由模型。
	Models    []string  `json:"models,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate 校验配置字段。
func (c Config) Validate() error {
	if c.Name == "" {
		return errors.New("zcode: name is required")
	}
	if strings.Contains(c.Name, "/") {
		return fmt.Errorf("zcode: name %q must not contain '/'", c.Name)
	}
	if strings.ContainsAny(c.Name, "\r\n") {
		return fmt.Errorf("zcode: name %q must not contain newlines", c.Name)
	}
	// models:每个非空、不含空白/换行、不重复。
	seen := make(map[string]bool, len(c.Models))
	for i, m := range c.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			return fmt.Errorf("zcode %q: models[%d] must not be empty", c.Name, i)
		}
		if strings.ContainsAny(m, "\r\n \t") {
			return fmt.Errorf("zcode %q: models[%d] must not contain whitespace or newlines", c.Name, i)
		}
		if seen[m] {
			return fmt.Errorf("zcode %q: duplicate model %q", c.Name, m)
		}
		seen[m] = true
	}
	// 字段值不得含换行(避免污染 JSON 结构)。
	if strings.ContainsAny(c.APIKey, "\r\n") {
		return fmt.Errorf("zcode %q: api_key must not contain newlines", c.Name)
	}
	return nil
}

// Manager 负责 zcode 预设的增删改查与本地 JSON 持久化。
type Manager struct {
	mu       sync.RWMutex
	presets  map[string]Config // name -> Config
	filePath string
}

// NewManager 从指定 JSON 文件加载预设;文件不存在时视为空。
func NewManager(filePath string) (*Manager, error) {
	m := &Manager{presets: make(map[string]Config), filePath: filePath}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load 启动时读取本地 JSON 中的预设。
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("zcode: read config: %w", err)
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return fmt.Errorf("zcode: parse config %s: %w", m.filePath, err)
	}
	for _, c := range cfgs {
		if c.Name == "" {
			return fmt.Errorf("zcode: invalid entry in %s", m.filePath)
		}
		m.presets[c.Name] = c
	}
	return nil
}

// save 将当前配置写回本地 JSON,采用临时文件 + 改名避免写入中断损坏配置。
func (m *Manager) save() error {
	cfgs := make([]Config, 0, len(m.presets))
	for _, c := range m.presets {
		cfgs = append(cfgs, c)
	}
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Name < cfgs[j].Name })
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.filePath)
	tmp, err := os.CreateTemp(dir, ".zcode-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, m.filePath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Add 新增预设;校验失败返回错误,同名返回 ErrExists,持久化失败回滚并返回 ErrPersist。
func (m *Manager) Add(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.presets[cfg.Name]; ok {
		return fmt.Errorf("%w: %s", ErrExists, cfg.Name)
	}
	m.presets[cfg.Name] = cfg
	if err := m.save(); err != nil {
		delete(m.presets, cfg.Name)
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Update 修改预设;不存在返回 ErrNotFound,持久化失败回滚。保留原创建时间。
func (m *Manager) Update(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.presets[cfg.Name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, cfg.Name)
	}
	cfg.CreatedAt = old.CreatedAt
	m.presets[cfg.Name] = cfg
	if err := m.save(); err != nil {
		m.presets[cfg.Name] = old
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Delete 删除指定名称的预设;不存在返回 ErrNotFound。
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.presets[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	delete(m.presets, name)
	if err := m.save(); err != nil {
		m.presets[name] = old // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Get 返回指定名称的预设(含未掩码的密钥)。
func (m *Manager) Get(name string) (Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.presets[name]
	if !ok {
		return Config{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return c, nil
}

// List 返回全部预设(按名称排序)。
func (m *Manager) List() []Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfgs := make([]Config, 0, len(m.presets))
	for _, c := range m.presets {
		cfgs = append(cfgs, c)
	}
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Name < cfgs[j].Name })
	return cfgs
}

// Count 返回预设数量。
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.presets)
}

// DefaultConfigPath 返回本地 zcode 的 config.json 路径(用户主目录下的
// ~/.zcode/v2/config.json;Windows 为 %USERPROFILE%\.zcode\v2\config.json)。
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("zcode: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".zcode", "v2", "config.json"), nil
}

// ---- 供应商条目构建(zcode config.json 的 wire 格式)----

// providerEntry 是 zcode config.json `provider` map 里一个供应商条目的形态。
// 字段名与 zcode 持久化的 camelCase 完全一致;顺序按 JSON 输出友好排列。
type providerEntry struct {
	Name    string              `json:"name"`
	Kind    string              `json:"kind"`
	Options providerOptions     `json:"options"`
	Source  string              `json:"source"`
	Models  map[string]modelCfg `json:"models"`
}

// providerOptions 是供应商的 options 块。WireAPI 仅供 openai-compatible 的
// responses 供应商使用(wire_api=responses),其余供应商留空。
type providerOptions struct {
	APIKey         string `json:"apiKey"`
	BaseURL        string `json:"baseURL"`
	APIKeyRequired bool   `json:"apiKeyRequired"`
	WireAPI        string `json:"wire_api,omitempty"`
}

// modelCfg 是供应商 models map 里一个模型条目的形态。
type modelCfg struct {
	Limit      modelLimit `json:"limit"`
	Modalities modalities `json:"modalities"`
}

// modelLimit 是模型的上下文/输出限制(tokens)。
type modelLimit struct {
	Context int `json:"context"`
}

// modalities 是模型支持的输入/输出模态。
type modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// buildModelEntry 生成单个模型的条目:limit.context 取该模型配置的上下文窗口
// (tokens),未配置(≤0)回退默认 200k;模态固定 text/text(与 zcode 内置条目一致)。
func buildModelEntry(window int) modelCfg {
	if window <= 0 {
		window = defaultContextTokens
	}
	return modelCfg{
		Limit:      modelLimit{Context: window},
		Modalities: modalities{Input: []string{"text"}, Output: []string{"text"}},
	}
}

// ProviderSpec 描述 apply-local 要写入的一个 zcode 供应商条目。
// apply-local 按模型原生格式分割为多个 ProviderSpec(completion→openai-compatible、
// anthropic→anthropic、responses→openai-compatible+wire_api)。
type ProviderSpec struct {
	Name    string         // provider map 键与显示名(如 "bsrouter-openai")
	Kind    string         // zcode 接口格式(DefaultKind openai-compatible / KindAnthropic);空 = 默认
	WireAPI string         // 可选:openai-compatible 的 wire_api(如 WireAPIResponses)
	BaseURL string         // options.baseURL(接口格式决定是否带 /v1)
	Models  []string       // 该供应商写入的模型 id 列表
	Windows map[string]int // 模型 id → 上下文窗口(tokens),未配置回退默认 200k
}

// BuildProvider 由一个 ProviderSpec 与网关 apiKey 生成供应商条目(map 形态,便于
// 合并进 provider map)。zcode 的模型列表手动配置在 config.json:每个模型写入一条
// (models 键),limit.context 按 spec.Windows 换算的 tokens(未配置回退默认 200k)。
// apiKey 为空时 apiKeyRequired 置 false(网关未鉴权时 zcode 无需带 key)。
func BuildProvider(spec ProviderSpec, apiKey string) map[string]any {
	kind := spec.Kind
	if kind == "" {
		kind = DefaultKind
	}
	modelsMap := make(map[string]modelCfg, len(spec.Models))
	for _, id := range spec.Models {
		modelsMap[id] = buildModelEntry(spec.Windows[id])
	}
	entry := providerEntry{
		Name:   spec.Name,
		Kind:   kind,
		Options: providerOptions{
			APIKey:         apiKey,
			BaseURL:        spec.BaseURL,
			APIKeyRequired: strings.TrimSpace(apiKey) != "",
			WireAPI:        spec.WireAPI,
		},
		Source: "custom",
		Models: modelsMap,
	}
	// 经 JSON 往返得到 map[string]any,便于与已解析的 config 合并(不引入私有类型)。
	data, _ := json.Marshal(entry)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

// ApplyToLocalConfig 把 providers 里的 zcode 供应商(bsrouter* 系列)覆盖进本地
// ~/.zcode/v2/config.json 的 provider map:保留其余内置/自定义供应商与顶层其它字段,
// 移除任何已有 name 以 "bsrouter" 开头的条目(单供应商旧键与多供应商键都清理,保证
// 幂等),再逐条写回 providers。文件不存在时创建;写入采用临时文件 + 改名原子写。
// apiKey 为网关 key(所有供应商共用);各 ProviderSpec 的 BaseURL/Kind/WireAPI/Models
// 应由调用方按目标入口派生。
func ApplyToLocalConfig(path, apiKey string, providers []ProviderSpec) error {
	var cfg map[string]any
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // 剥离 UTF-8 BOM(Windows 工具常加)
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("zcode: parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		cfg = make(map[string]any)
	default:
		return fmt.Errorf("zcode: read %s: %w", path, err)
	}

	existing, _ := cfg["provider"].(map[string]any)
	if existing == nil {
		existing = make(map[string]any)
	}
	// 移除任何已有 name 以 "bsrouter" 开头的条目(单供应商旧键 + 多供应商键都清理,
	// 可能还含 zcode 此前分配 UUID 键的旧条目),保证写回后 bsrouter* 系列唯一、apply 幂等。
	for key, v := range existing {
		if entry, ok := v.(map[string]any); ok {
			if n, _ := entry["name"].(string); strings.HasPrefix(n, ProviderName) {
				delete(existing, key)
			}
		}
	}
	for _, spec := range providers {
		existing[spec.Name] = BuildProvider(spec, apiKey)
	}
	cfg["provider"] = existing

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("zcode: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
