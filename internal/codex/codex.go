// Package codex 管理 OpenAI Codex CLI 运行配置预设:一条预设对应一套 Codex
// 运行配置(config.toml 的 model_providers 块 + 顶层 model_provider/model),
// 可一键生成 PowerShell / bash 启动命令(-c 覆盖,不写文件),或覆盖本地
// ~/.codex/config.toml(固定 bsrouter 块,最后一次应用生效),实现多终端环境分隔。
//
// Codex 与 Claude Code 的配置形态不同:Claude 用环境变量(settings.json env 块),
// Codex 用 TOML(model_providers.<key> 块 + 顶层键)。因此命令生成改为
// `codex -c 'model_providers.<key>.<field>="<value>"'` 形式的覆盖参数,
// "覆盖本地"改为把单一 [model_providers.bsrouter] 块合并进 ~/.codex/config.toml。
package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound 表示预设不存在。
	ErrNotFound = errors.New("codex: not found")
	// ErrExists 表示同名预设已存在。
	ErrExists = errors.New("codex: already exists")
	// ErrPersist 表示配置写入本地 JSON 失败。
	ErrPersist = errors.New("codex: persist failed")
)

// bsrouterProvider 是"覆盖本地 Codex 配置"使用的固定 provider 键:
// apply-local 总是写入这个块的配置,最后一次应用生效(不按预设名各建一块)。
const bsrouterProvider = "bsrouter"

// defaultEnvKey 是命令/配置中未指定 EnvKey 时用于承载 API key 的环境变量。
// Codex 自定义 provider 不会自动读取 OPENAI_API_KEY,必须在 env_key 显式声明;
// 命令生成时把该环境变量设为首行(与 Claude 预设的 $env:ANTHROPIC_API_KEY 同理)。
const defaultEnvKey = "OPENAI_API_KEY"

// envKeyRe 是环境变量名的合法形态(与 POSIX 环境变量命名一致)。
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// cfgKeyRe 是附加 -c 覆盖键的合法形态(点路径:段与段之间用点连接,每段为首字符
// 字母/下划线,后续字母/数字/下划线/连字符)。禁止含引号/空白/换行。
var cfgKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*(\.[A-Za-z_][A-Za-z0-9_-]*)*$`)

// reservedTop 是一等字段直接对应的 -c 点路径键,extra_config 不得覆盖,避免歧义。
var reservedTop = map[string]bool{
	"model_provider":         true,
	"model":                  true,
	"model_reasoning_effort": true,
}

// reservedProvider 是"覆盖本地"写入的 bsrouter 块内部键,extra_config 不得覆盖
// (base_url/wire_api 由一等字段生成;name 固定)。
var reservedProvider = map[string]bool{
	"model_providers.bsrouter.name":     true,
	"model_providers.bsrouter.base_url": true,
	"model_providers.bsrouter.wire_api": true,
}

// Config 是一条 Codex 运行配置预设,也是本地 JSON 持久化的存储格式。
// 字段映射 codex config.toml 的 model_providers 块与顶层键。
type Config struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// BaseURL 是 codex 的 base_url(指向 BSRouter 的 <入口>/v1,codex 会拼接
	// /responses)。可选:留空时 apply-local/命令用网关统一 API 入口派生
	// (本地 http://127.0.0.1:<端口>/api/v1,远程用广告地址)。codex 仅支持
	// wire_api=responses(chat 已不再支持)。
	BaseURL string `json:"base_url,omitempty"`
	// APIKey 是 Bearer 密钥(掩码返回)。Codex 自定义 provider 只走 Bearer,
	// 没有 x-api-key 二元形态,故不设 auth_token 字段。
	APIKey string `json:"api_key,omitempty"`
	// EnvKey 是命令注入密钥使用的环境变量名;留空默认 OPENAI_API_KEY。
	EnvKey string `json:"env_key,omitempty"`
	// Model 是默认模型;留空时 codex 交互选择。
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"model_reasoning_effort,omitempty"` // model_reasoning_effort(low/medium/high/none)
	// Models 是预设直接配置的模型列表(最多 len(NativeOpenAISlugs()) 个,即原生
	// id 池大小,网关可路由 id:"{供应商}@{模型}" 合成或聚合裸名)。这是 codex 配置
	// 的模型来源:不再依赖虚拟供应商/网关全部模型,而是由预设直接指定。每个模型在
	// codex 模型目录中发布为一条自动分配的裸原生 slug 行(排序位置 → 原生 id 池),
	// 从而在 Codex Desktop 显示(桌面渲染层只放行它认识的裸原生 OpenAI id)。
	// 留空回退网关全部可路由模型(自动分配前 len(NativeOpenAISlugs()) 个,向后兼容)。
	Models []string `json:"models,omitempty"`
	// ExtraConfig 是任意附加 -c key=value 覆盖(键合法且非保留)。
	ExtraConfig map[string]string `json:"extra_config,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// NativeAlias 是一条生效的 native-alias 绑定(slug → 模型)。Codex Desktop 的渲染层
// 会按远程 available_models allowlist 过滤模型选择器,只保留它认识的裸原生 OpenAI
// id(openai/codex#19694);普通 "{供应商}@{模型}" 路由 id 不在名单内会被剔除。
// 网关为每个配置模型自动分配一条绑定:slug 取自原生 id 池(原生 id 对应关系无关
// 紧要,只是通过 allowlist 的"护照"),display_name 用模型 id(诚实标签:桌面显示
// 真实模型名)。该 slug 的请求经网关路由到绑定模型。本类型不直接出现在预设配置里,
// 由服务器按模型列表自动生成。
type NativeAlias struct {
	// Slug 是被接管的裸原生 OpenAI 模型 id(如 gpt-5.3-codex-spark),也是目录行的 slug。
	Slug string `json:"slug"`
	// Model 是该 slug 实际路由到的网关可路由模型 id("{供应商}@{模型}" 合成 id 或
	// 聚合裸名),可带上下文标记(如 [1M])。
	Model string `json:"model"`
	// DisplayName 是目录行的 display_name(Codex 选择器显示的标签),自动分配时为模型 id。
	DisplayName string `json:"display_name"`
}

// nativeOpenAISlugs 是本网关支持接管的原生 OpenAI 模型 id。该集合必须与 Codex
// Desktop 的 available_models allowlist 完全一致(实测 2026-08 桌面版)——
// 只有这些裸原生 id 能通过 Desktop 渲染层过滤,自动分配必须从中取 slug:
// ["gpt-5.6-sol","gpt-5.6-terra","gpt-5.6-luna","gpt-5.5","gpt-5.4",
//  "gpt-5.4-mini","gpt-5.3-codex","gpt-5.2"]。
var nativeOpenAISlugs = map[string]bool{
	"gpt-5.6-sol":   true,
	"gpt-5.6-terra": true,
	"gpt-5.6-luna":  true,
	"gpt-5.5":       true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.3-codex": true,
	"gpt-5.2":       true,
}

// IsNativeOpenAISlug 判断 slug 是否为支持接管的原生 OpenAI 模型 id。
func IsNativeOpenAISlug(slug string) bool {
	return nativeOpenAISlugs[slug]
}

// NativeOpenAISlugs 返回支持接管的原生 OpenAI 模型 id(排序),供管理接口/前端展示。
func NativeOpenAISlugs() []string {
	out := make([]string, 0, len(nativeOpenAISlugs))
	for s := range nativeOpenAISlugs {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Validate 校验配置字段。
func (c Config) Validate() error {
	if c.Name == "" {
		return errors.New("codex: name is required")
	}
	if strings.Contains(c.Name, "/") {
		return fmt.Errorf("codex: name %q must not contain '/'", c.Name)
	}
	if strings.ContainsAny(c.Name, "\r\n") {
		return fmt.Errorf("codex: name %q must not contain newlines", c.Name)
	}
	// base_url 可选:留空由 apply-local/命令派生网关统一 API 入口;非空必须是合法
	// http(s) 地址,避免 file:// 等非网络协议。
	if c.BaseURL != "" {
		parsed, err := url.Parse(c.BaseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("codex %q: base_url must be a valid http(s) URL", c.Name)
		}
	}
	if k := strings.TrimSpace(c.EnvKey); k != "" && !envKeyRe.MatchString(k) {
		return fmt.Errorf("codex %q: invalid env_key %q", c.Name, k)
	}
	// 任意字段值不得含换行:命令是逐行生成的,换行会注入额外命令。
	for name, v := range map[string]string{
		"base_url": c.BaseURL, "api_key": c.APIKey, "model": c.Model,
		"model_reasoning_effort": c.ReasoningEffort,
	} {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("codex %q: %s must not contain newlines", c.Name, name)
		}
	}
	for k, v := range c.ExtraConfig {
		if !cfgKeyRe.MatchString(k) {
			return fmt.Errorf("codex %q: invalid config key %q", c.Name, k)
		}
		// 保留:一等字段对应键 + bsrouter 块内部键 + 裸表键 model_providers /
		// model_providers.bsrouter(无尾点),避免覆盖整个 provider 表。
		if reservedTop[k] || reservedProvider[k] || k == "model_providers" ||
			k == "model_providers."+bsrouterProvider ||
			strings.HasPrefix(k, "model_providers."+bsrouterProvider+".") {
			return fmt.Errorf("codex %q: config key %q is reserved", c.Name, k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("codex %q: config %q value must not contain newlines", c.Name, k)
		}
	}
	// models:直接配置的模型列表,最多 len(NativeOpenAISlugs())(对应原生 id 池),
	// 每个非空、不含空白/换行、不重复。留空表示回退网关全部可路由模型(向后兼容)。
	if len(c.Models) > len(NativeOpenAISlugs()) {
		return fmt.Errorf("codex %q: at most %d models are allowed (one per native OpenAI slug)", c.Name, len(NativeOpenAISlugs()))
	}
	seen := make(map[string]bool, len(c.Models))
	for i, m := range c.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			return fmt.Errorf("codex %q: models[%d] must not be empty", c.Name, i)
		}
		if strings.ContainsAny(m, "\r\n \t") {
			return fmt.Errorf("codex %q: models[%d] must not contain whitespace or newlines", c.Name, i)
		}
		if seen[m] {
			return fmt.Errorf("codex %q: duplicate model %q", c.Name, m)
		}
		seen[m] = true
	}
	return nil
}

// EnvKeyName 返回预设实际使用的密钥环境变量名(留空时用默认)。
func (c Config) EnvKeyName() string {
	if k := strings.TrimSpace(c.EnvKey); k != "" {
		return k
	}
	return defaultEnvKey
}

// Manager 负责 Codex 预设的增删改查与本地 JSON 持久化。
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
		return fmt.Errorf("codex: read config: %w", err)
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return fmt.Errorf("codex: parse config %s: %w", m.filePath, err)
	}
	for _, c := range cfgs {
		if c.Name == "" {
			return fmt.Errorf("codex: invalid entry in %s", m.filePath)
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
	tmp, err := os.CreateTemp(dir, ".codex-*.json")
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

// Command 是一条预设对应的一键启动命令(两个 shell 版本)。
type Command struct {
	PowerShell string `json:"powershell"`
	Bash       string `json:"bash"`
}

// BuildCommand 由预设生成两个 shell 的一键启动命令:设置密钥环境变量后,
// 用一串 `-c` 覆盖完整定义 provider 并启动 codex。纯函数,结果确定,
// 不写任何文件,天然实现多终端环境分隔(与 Claude 预设命令同一哲学)。
func BuildCommand(cfg Config) Command {
	envKey := cfg.EnvKeyName()
	// 收集 codex 的原始参数(`key="value"` 形态,值按 TOML basic-string 转义;
	// -m 为裸模型名)。输出时再按各 shell 的引号规则包裹。
	type rawArg struct{ text string }
	var raws []rawArg
	dashC := func(key, val string) {
		raws = append(raws, rawArg{key + `="` + escapeToml(val) + `"`})
	}
	// codex 仅支持 wire_api=responses,显式声明保证 provider 配置完整。
	dashC("model_providers."+bsrouterProvider+".wire_api", "responses")
	dashC("model_providers."+bsrouterProvider+".name", bsrouterProvider)
	dashC("model_providers."+bsrouterProvider+".base_url", cfg.BaseURL)
	dashC("model_providers."+bsrouterProvider+".env_key", envKey)
	// requires_openai_auth 让 Codex App/TUI 视为有账号能力,展示自定义模型列表。
	dashC("model_providers."+bsrouterProvider+".requires_openai_auth", "true")
	dashC("model_provider", bsrouterProvider)
	if m := strings.TrimSpace(cfg.Model); m != "" {
		raws = append(raws, rawArg{"model:" + m}) // 特殊标记:以 -m 形式输出
	}
	if e := strings.TrimSpace(cfg.ReasoningEffort); e != "" {
		dashC("model_reasoning_effort", e)
	}
	keys := make([]string, 0, len(cfg.ExtraConfig))
	for k := range cfg.ExtraConfig {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		dashC(k, cfg.ExtraConfig[k])
	}

	// 按 shell 生成命令文本:首行设密钥环境变量,随后 codex 命令逐参数续行。
	wrap := func(r rawArg, ps bool) string {
		if strings.HasPrefix(r.text, "model:") {
			m := r.text[len("model:"):]
			if ps {
				return `-m "` + escapePS(m) + `"`
			}
			return `-m "` + escapeSh(m) + `"`
		}
		if ps {
			return "-c '" + escapePSArg(r.text) + "'"
		}
		return "-c '" + escapeShArg(r.text) + "'"
	}

	var ps, sh strings.Builder
	ps.WriteString(`$env:` + envKey + ` = "` + escapePS(cfg.APIKey) + `"`)
	ps.WriteString("\ncodex")
	sh.WriteString(`export ` + envKey + `="` + escapeSh(cfg.APIKey) + `"`)
	sh.WriteString("\ncodex")
	for i, r := range raws {
		cont := i < len(raws)-1
		ps.WriteString("\n      " + wrap(r, true))
		sh.WriteString("\n      " + wrap(r, false))
		if cont {
			ps.WriteString(" `")
			sh.WriteString(" \\")
		}
	}
	return Command{PowerShell: ps.String(), Bash: sh.String()}
}

// escapePS 转义 PowerShell 双引号字符串字面量内容。
// PS 双引号内反引号是转义符(→ 双反引号)、$ 触发变量展开(→ `$)、" 结束字符串(→ `")。
// 反斜杠在 PS 双引号内不特殊,原样保留。NewReplacer 单遍扫描,替换结果不再重扫。
func escapePS(s string) string {
	return strings.NewReplacer(
		"`", "``",
		`"`, "`\"",
		"$", "`$",
	).Replace(s)
}

// escapeSh 转义 bash 双引号字符串字面量内容。
// bash 双引号内反斜杠、双引号、$、反引号四个字符需加反斜杠前缀。
func escapeSh(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"$", `\$`,
		"`", "\\`",
	).Replace(s)
}

// escapeToml 转义 TOML basic-string 双引号字面量内容:
// 反斜杠 → 双反斜杠、双引号 → 反斜杠+双引号。其余字符(含 tab)在 TOML basic-string
// 内为字面量;换行由 Validate 禁止。NewReplacer 单遍扫描。
func escapeToml(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
	).Replace(s)
}

// escapePSArg 转义 PowerShell 单引号字符串字面量内容(单引号 → 双单引号)。
func escapePSArg(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// escapeShArg 转义 bash 单引号字符串字面量内容(单引号 → '\'' 序列)。
func escapeShArg(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// DefaultConfigPath 返回本地 Codex 的 config.toml 路径(用户主目录下的 .codex)。
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// DefaultAuthPath 返回本地 Codex 的 auth.json 路径(用户主目录下的 .codex)。
func DefaultAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

// modelCatalogFileName 是 BSRouter 生成的 codex 模型目录文件名(存于 ~/.codex)。
const modelCatalogFileName = "bsrouter-models.json"

// ModelCatalogFileName 返回模型目录文件名(config.toml 顶层 model_catalog_json
// 引用相对 ~/.codex 的文件名)。
func ModelCatalogFileName() string { return modelCatalogFileName }

// DefaultModelCatalogPath 返回 BSRouter 生成的模型目录路径(~/.codex/bsrouter-models.json)。
func DefaultModelCatalogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".codex", modelCatalogFileName), nil
}

// DefaultModelsCachePath 返回 codex 的 models_cache.json 路径(~/.codex/models_cache.json,
// 桌面 app 的模型列表来源)。
func DefaultModelsCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".codex", "models_cache.json"), nil
}

// ModelCatalog 是 codex 模型目录文件(model_catalog_json)的格式:codex TUI/桌面端
// 的模型选择列表来源。schema 参考 codex 的 model_catalog 格式(用户提供)。
type ModelCatalog struct {
	Models []ModelCatalogEntry `json:"models"`
}

// ModelCatalogEntry 是目录中的单个模型。slug 即请求体 model 字段(codex 会原样
// 发送,BSRouter 按其路由),故用网关的 "{供应商}@{模型}" 合成 id / 聚合裸名。
type ModelCatalogEntry struct {
	Slug                          string         `json:"slug"`
	DisplayName                   string         `json:"display_name"`
	Description                   string         `json:"description"`
	Visibility                    string         `json:"visibility"`
	SupportedInAPI                bool           `json:"supported_in_api"`
	ContextWindow                 int            `json:"context_window"`
	MaxContextWindow              int            `json:"max_context_window"`
	EffectiveContextWindowPercent int            `json:"effective_context_window_percent"`
	AutoCompactTokenLimit         int            `json:"auto_compact_token_limit"`
	InputModalities               []string       `json:"input_modalities"`
	SupportsImageDetailOriginal   bool           `json:"supports_image_detail_original"`
	SupportsParallelToolCalls     bool           `json:"supports_parallel_tool_calls"`
	SupportsSearchTool            bool           `json:"supports_search_tool"`
	WebSearchToolType             string         `json:"web_search_tool_type"`
	ApplyPatchToolType            string         `json:"apply_patch_tool_type"`
	ShellType                     string         `json:"shell_type"`
	SupportedReasoningLevels      []ReasoningLvl `json:"supported_reasoning_levels"`
	SupportsReasoningSummaries    bool           `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary       string         `json:"default_reasoning_summary"`
	DefaultReasoningLevel         string         `json:"default_reasoning_level"`
	SupportVerbosity              bool           `json:"support_verbosity"`
	DefaultVerbosity              string         `json:"default_verbosity"`
	TruncationPolicy              Truncation     `json:"truncation_policy"`
	Priority                      int            `json:"priority"`
	ExperimentalSupportedTools    []any          `json:"experimental_supported_tools"`
	AdditionalSpeedTiers          []any          `json:"additional_speed_tiers"`
	ServiceTiers                  []any          `json:"service_tiers"`
	AvailabilityNux               any            `json:"availability_nux"`
	Upgrade                       any            `json:"upgrade"`
	BaseInstructions              string         `json:"base_instructions"`
	IncludeSkillsUsageInstructions bool          `json:"include_skills_usage_instructions"`
	UseResponsesLite              bool           `json:"use_responses_lite"`
}

// ReasoningLvl 是 supported_reasoning_levels 的一项(codex 严格要求该字段)。
type ReasoningLvl struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

// Truncation 是 truncation_policy。
type Truncation struct {
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

// buildModelEntry 生成单个模型的目录条目。slug 直接用 id(BSRouter 路由用
// "{供应商}@{模型}" 或聚合裸名);上下文窗口未知时给合理默认(200K),按用户
// 提供的 codex model_catalog 参考格式生成。
func buildModelEntry(id string) ModelCatalogEntry {
	return ModelCatalogEntry{
		Slug:                          id,
		DisplayName:                   id,
		Description:                   id,
		Visibility:                    "list",
		SupportedInAPI:                true,
		ContextWindow:                 200000,
		MaxContextWindow:              200000,
		EffectiveContextWindowPercent: 100,
		AutoCompactTokenLimit:         40000,
		InputModalities:               []string{"text", "image"},
		SupportsImageDetailOriginal:   true,
		SupportsParallelToolCalls:     true,
		SupportsSearchTool:            true,
		WebSearchToolType:             "text_and_image",
		ApplyPatchToolType:            "freeform",
		ShellType:                     "shell_command",
		SupportedReasoningLevels: []ReasoningLvl{
			{Effort: "high", Description: "Enabled Thinking"},
			{Effort: "none", Description: "Disable Thinking"},
		},
		SupportsReasoningSummaries:    true,
		DefaultReasoningSummary:       "auto",
		DefaultReasoningLevel:         "medium",
		SupportVerbosity:              true,
		DefaultVerbosity:              "low",
		TruncationPolicy:              Truncation{Mode: "tokens", Limit: 10000},
		Priority:                      10,
		ExperimentalSupportedTools:    []any{},
		AdditionalSpeedTiers:          []any{},
		ServiceTiers:                  []any{},
		AvailabilityNux:               nil,
		Upgrade:                       nil,
		BaseInstructions:              "You are Codex, a coding agent.",
		IncludeSkillsUsageInstructions: false,
		UseResponsesLite:              false,
	}
}

// BuildModelCatalog 由模型 id 列表生成 codex 模型目录 JSON(model_catalog_json)。
// windows 为模型 id → 上下文窗口(tokens)的映射(来自供应商模型配置),目录条目据此
// 同步每个模型的实际窗口;未配置(不在映射中)的模型回退默认 200K。
// 每个模型只发布一条自动分配的裸原生 slug 行(排序位置 → 原生 id 池),让 Codex
// Desktop 经原生 allowlist 显示这些模型;不再发布模型原名的路由 id 行(避免重复
// 对象)。全部条目按 slug 排序保证确定。
func BuildModelCatalog(models []string, windows map[string]int) []byte {
	cat := ModelCatalog{Models: buildCatalogEntries(models, windows)}
	data, _ := json.MarshalIndent(cat, "", "  ")
	return data
}

// buildCatalogEntries 为每个模型生成自动原生条目,按 slug 排序去重。原生行:slug
// 取原生 id 池[排序位置](原生 id 对应关系无关紧要,只是通过 Desktop allowlist 的
// "护照"),display_name 用模型 id(诚实标签:桌面显示真实模型名)。池用尽即停。
func buildCatalogEntries(models []string, windows map[string]int) []ModelCatalogEntry {
	ordered := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, id := range models {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	pool := NativeOpenAISlugs()
	entries := make([]ModelCatalogEntry, 0, len(ordered))
	for i, id := range ordered {
		if i >= len(pool) {
			break // 原生 id 池用尽,其余模型无法在 Desktop 显示
		}
		entries = append(entries, buildNativeAliasEntry(id, pool[i], windows[id]))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	return entries
}

// buildNativeAliasEntry 生成裸原生 slug 目录条目:slug 是被接管的原生 id(护照),
// display_name 为模型 id(诚实标签);上下文窗口取模型配置值 window(tokens),
// 未配置(≤0)时保留 buildModelEntry 的默认 200K。
func buildNativeAliasEntry(model string, slug string, window int) ModelCatalogEntry {
	e := buildModelEntry(slug)
	e.DisplayName = model
	e.Description = model
	if window > 0 {
		e.ContextWindow = window
		e.MaxContextWindow = window
	}
	return e
}

// ModelsCache 是 codex 的 models_cache.json 格式:桌面 app(app-server)的模型
// 列表来源。外层有 fetched_at/client_version,每模型在 ModelCatalogEntry 基础上
// 多 supports_reasoning_summaries(实测桌面 app 读此文件而非 model_catalog_json)。
type ModelsCache struct {
	FetchedAt     string              `json:"fetched_at"`
	ClientVersion string              `json:"client_version"`
	Models        []ModelCatalogEntry `json:"models"`
}

// codexClientVersionRe 匹配 codex --version 输出的主版本号(如
// "codex-cli 0.147.0-alpha.6.5" → "0.147.0")。models_cache.json 的
// client_version 必须与 codex 当前版本主号匹配,否则桌面 app 报
// "cache version mismatch" 拒绝缓存、改走在线刷新。
var codexClientVersionRe = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// detectCodexVersionOnce 探测本地 codex 主版本号(执行 codex --version 解析主号)。
// 检测失败返回空串。用 sync.Once 保证只探测一次。codex 可能在 PATH 或常见安装位置。
var (
	detectCodexVersionOnce sync.Once
	detectedCodexVersion   string
)

func detectCodexVersion() string {
	detectCodexVersionOnce.Do(func() {
		detectedCodexVersion = runCodexVersion()
	})
	return detectedCodexVersion
}

// runCodexVersion 执行 "codex --version" 并解析主版本号(如 "0.147.0-alpha.6.5"
// → "0.147.0")。codex 不在 PATH 时尝试常见 Windows/macOS/Linux 安装位置。
func runCodexVersion() string {
	exe := "codex"
	if runtime.GOOS == "windows" {
		exe = "codex.exe"
	}
	// 候选可执行文件:PATH + 常见安装位置。
	candidates := []string{exe}
	if runtime.GOOS == "windows" {
		// 常见安装:C:\Users\<user>\AppData\Local\OpenAI\Codex\bin\<hash>\codex.exe
		if home, err := os.UserHomeDir(); err == nil {
			if bins, err := filepath.Glob(filepath.Join(home, "AppData", "Local", "OpenAI", "Codex", "bin", "*", exe)); err == nil {
				candidates = append(candidates, bins...)
			}
		}
	}
	for _, c := range candidates {
		cmd := exec.Command(c, "--version")
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		m := codexClientVersionRe.FindStringSubmatch(strings.TrimSpace(string(out)))
		if len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// DetectCodexVersion 返回本地 codex 主版本号;未安装/检测失败返回空串。
// 供 server 端写 models_cache.json 时填充 client_version。
func DetectCodexVersion() string {
	return detectCodexVersion()
}

// BuildModelsCache 由模型 id 列表生成 codex 的 models_cache.json 内容。
// clientVersion 应传 codex 主版本号(如 "0.147.0");留空回退 "0.0.0"
// (旧行为,可能被桌面 app 判为 version mismatch)。fetched_at 用当前时间,
// 避免缓存被视为过期而触发在线刷新(桌面 app 无登录时刷新失败导致列表空)。
func BuildModelsCache(models []string, clientVersion string, windows map[string]int) []byte {
	ver := strings.TrimSpace(clientVersion)
	if ver == "" {
		ver = "0.0.0"
	}
	cache := ModelsCache{
		FetchedAt:     time.Now().UTC().Format(time.RFC3339),
		ClientVersion: ver,
		Models:        buildCatalogEntries(models, windows),
	}
	data, _ := json.MarshalIndent(cache, "", "  ")
	return data
}

// writeJSONAtomically 以临时文件 + 改名原子写 JSON 数据到指定路径。
func writeJSONAtomically(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codex: mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".models-*.json")
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
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ApplyToLocalModelCatalog 把模型目录(model_catalog_json 格式)写入指定路径。
// windows 为模型 id → 上下文窗口(tokens),目录条目据此同步每个模型的窗口。
func ApplyToLocalModelCatalog(path string, models []string, windows map[string]int) error {
	return writeJSONAtomically(path, BuildModelCatalog(models, windows))
}

// ApplyToLocalModelsCache 把模型缓存(models_cache.json 格式,桌面 app 读)写入
// 指定路径。client_version 自动探测本地 codex 版本,确保桌面 app 不因版本不匹配
// 拒绝缓存(见 DetectCodexVersion)。windows 语义同 ApplyToLocalModelCatalog。
func ApplyToLocalModelsCache(path string, models []string, windows map[string]int) error {
	return writeJSONAtomically(path, BuildModelsCache(models, DetectCodexVersion(), windows))
}

// ApplyToLocalAuth 把密钥写入 ~/.codex/auth.json 的 OPENAI_API_KEY 字段,使
// codex 跳过 ChatGPT 登录、直接用该 key 鉴权。保留 auth.json 的其余字段
// (如官方登录态),只覆盖 OPENAI_API_KEY;文件不存在时创建;key 为空时不动
// (避免清掉用户已有的鉴权)。采用临时文件 + 改名原子写。
func ApplyToLocalAuth(path, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	auth := make(map[string]string)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &auth); err != nil {
			return fmt.Errorf("codex: parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 新文件,空 map。
	default:
		return fmt.Errorf("codex: read %s: %w", path, err)
	}
	auth["OPENAI_API_KEY"] = key
	out, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("codex: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.json")
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

// ApplyToLocalConfig 把预设写入本地 ~/.codex/config.toml:替换/追加单一
// [model_providers.bsrouter] 块(base_url/wire_api,密钥不在此文件,由 auth.json 提供),
// 并设置顶层 model_provider="bsrouter"、model_catalog_json(指向 BSRouter 生成的
// 模型目录文件);预设的 model/reasoning_effort 仅在非空时写入,为空时删除对应
// 顶层键(最后一次应用生效)。采用行级定向编辑:保留文件其余内容
// (注释/其它配置/格式),文件不存在时创建。密钥写入由调用方经 ApplyToLocalAuth
// 完成(auth.json 的 OPENAI_API_KEY)。
func ApplyToLocalConfig(path string, cfg Config, modelCatalogJson string) error {
	block := buildProviderBlock(cfg)
	// 要保证的顶层键:model_provider 恒为 bsrouter;model_catalog_json 指向生成的
	// 模型目录;model/reasoning_effort 仅在预设非空时写入(空则删除旧残留)。
	topKeys := []topKey{
		{key: "model_provider", value: bsrouterProvider, present: true},
		{key: "model_catalog_json", value: strings.TrimSpace(modelCatalogJson), present: strings.TrimSpace(modelCatalogJson) != ""},
		{key: "model", value: strings.TrimSpace(cfg.Model), present: strings.TrimSpace(cfg.Model) != ""},
		{key: "model_reasoning_effort", value: strings.TrimSpace(cfg.ReasoningEffort), present: strings.TrimSpace(cfg.ReasoningEffort) != ""},
	}

	lines, err := readLines(path)
	if err != nil {
		return err
	}

	// 第一遍:就地替换顶层键(不改变行数),同时记录 bsrouter 块区间(供整块替换)。
	topActive := true // 首个 [table] 之前为顶层
	blockStart, blockEnd := -1, -1
	handled := make(map[string]bool)
	for i := 0; i < len(lines); i++ {
		ln := strings.TrimSpace(lines[i])
		if isTableHeader(ln) {
			topActive = false
			if isBsrouterHeader(headerName(ln)) {
				blockStart, blockEnd = i, i+1
				for j := i + 1; j < len(lines); j++ {
					if isTableHeader(strings.TrimSpace(lines[j])) {
						break
					}
					blockEnd = j + 1
				}
			}
			continue
		}
		if !topActive || ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		for _, tk := range topKeys {
			if strings.HasPrefix(ln, tk.key+"=") || strings.HasPrefix(ln, tk.key+" =") {
				if tk.present {
					indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
					lines[i] = indent + tk.key + " = \"" + escapeToml(tk.value) + "\""
				} else {
					lines[i] = "" // 删除该顶层键(留空行占位,后续压缩)
				}
				handled[tk.key] = true
				break
			}
		}
	}

	// 整块替换 bsrouter 段(此刻块区间仍有效:替换在插入/压缩之前执行)。
	if blockStart >= 0 {
		replacement := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
		lines = append(lines[:blockStart], append(replacement, lines[blockEnd:]...)...)
	}

	// 压缩因删除键产生的连续空行。
	lines = collapseBlankLines(lines)

	// 未出现的顶层键:插到文件头(跟随文件头部注释之后),保持顶部可读。
	var missing []topKey
	for _, tk := range topKeys {
		if tk.present && !handled[tk.key] {
			missing = append(missing, tk)
		}
	}
	var ins []string
	for _, tk := range missing {
		ins = append(ins, tk.key+" = \""+escapeToml(tk.value)+"\"")
	}
	if len(ins) > 0 {
		insertIdx := 0
		for insertIdx < len(lines) {
			t := strings.TrimSpace(lines[insertIdx])
			if t == "" || strings.HasPrefix(t, "#") {
				insertIdx++
				continue
			}
			break
		}
		head := make([]string, 0, len(ins)+1)
		head = append(head, ins...)
		if insertIdx < len(lines) {
			head = append(head, "")
		}
		lines = append(lines[:insertIdx], append(head, lines[insertIdx:]...)...)
	}

	// bsrouter 块不存在:追加到文件末尾(分隔空行)。
	if blockStart < 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(strings.TrimSuffix(block, "\n"), "\n")...)
	}

	return writeLines(path, lines)
}

// isTableHeader 判断一行是否为 TOML 表头([name] 或 [[name]]),忽略注释/空白。
func isTableHeader(ln string) bool {
	if ln == "" || strings.HasPrefix(strings.TrimSpace(ln), "#") {
		return false
	}
	return headerName(ln) != ""
}

// headerName 返回表头去掉行尾注释后的规范化形态(如 "[model_providers.bsrouter]"
// 或 "[[x]]"),供 isTableHeader 与块匹配共用。支持表头后跟行内注释
// (TOML 允许 `[a.b] # note`):定位最外层 ] 之后若仅剩空白或注释即视为表头;
// 引号键(如 ["a#b"])按最后 ] 判定不受影响;非表头行返回空串。
func headerName(ln string) string {
	ln = strings.TrimSpace(ln)
	idx := strings.LastIndex(ln, "]")
	if idx < 0 {
		return ""
	}
	head := ln[:idx+1]
	rest := strings.TrimSpace(ln[idx+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return ""
	}
	if !strings.HasPrefix(head, "[") || !strings.HasSuffix(head, "]") {
		return ""
	}
	return head
}

// isBsrouterHeader 判断规范化表头是否指向 model_providers.bsrouter
// (兼容 [x] 与 [[x]] 数组表形态)。
func isBsrouterHeader(head string) bool {
	return strings.Trim(head, "[]") == "model_providers."+bsrouterProvider
}

// topKey 是 ApplyToLocalConfig 要管理的顶层键。
type topKey struct {
	key     string
	value   string
	present bool // false 表示应从配置中删除该键
}

// buildProviderBlock 渲染 [model_providers.bsrouter] 块文本。密钥不进 config.toml:
// apply-local 时由调用方把密钥写入 auth.json 的 OPENAI_API_KEY(codex 借此跳过
// ChatGPT 登录);一键命令则经 env_key 环境变量注入(远程终端自包含)。
func buildProviderBlock(cfg Config) string {
	var b strings.Builder
	b.WriteString("[model_providers." + bsrouterProvider + "]\n")
	b.WriteString("name = \"" + escapeToml(bsrouterProvider) + "\"\n")
	b.WriteString("base_url = \"" + escapeToml(cfg.BaseURL) + "\"\n")
	b.WriteString("wire_api = \"responses\"\n")
	// requires_openai_auth 让 Codex App/TUI 视为有账号能力,从而展示自定义
	// provider 的模型列表(opencodex 文档:缺少该标记时 app 的账号门控 UI 隐藏)。
	b.WriteString("requires_openai_auth = true\n")
	return b.String()
}

// readLines 读取文件全部行;文件不存在返回空切片。剥离开头 UTF-8 BOM,
// 避免写回时把 BOM 带进第一行(Windows 工具常给文件加 BOM,codex 容忍但
// 写回应保持干净)。
func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex: read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	s := strings.TrimSuffix(string(data), "\n")
	return strings.Split(s, "\n"), nil
}

// writeLines 以临时文件 + 改名原子写回全部行。
func writeLines(path string, lines []string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("codex: mkdir %s: %w", dir, err)
	}
	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(out); err != nil {
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

// collapseBlankLines 把连续空行压缩为至多一个,并去掉文件开头/末尾的连续空行。
func collapseBlankLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	prevBlank := false
	for _, ln := range lines {
		blank := strings.TrimSpace(ln) == ""
		if blank {
			if prevBlank || len(out) == 0 {
				continue
			}
			prevBlank = true
		} else {
			prevBlank = false
		}
		out = append(out, ln)
	}
	// 去末尾空行。
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}
