// Package claude 管理 Claude Code 运行配置预设:一条预设对应一套 Claude Code
// 环境变量(镜像 settings.json 的 env 块),可一键生成 PowerShell / bash
// 启动命令,实现多终端环境分隔。
package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound 表示预设不存在。
	ErrNotFound = errors.New("claude: not found")
	// ErrExists 表示同名预设已存在。
	ErrExists = errors.New("claude: already exists")
	// ErrPersist 表示配置写入本地 JSON 失败。
	ErrPersist = errors.New("claude: persist failed")
)

// reservedEnv 是一等字段对应的环境变量名,extra_env 不得覆盖,避免歧义。
var reservedEnv = map[string]bool{
	"ANTHROPIC_BASE_URL":                 true,
	"ANTHROPIC_API_KEY":                  true,
	"ANTHROPIC_AUTH_TOKEN":               true,
	"ANTHROPIC_MODEL":                    true,
	"CLAUDE_CODE_SUBAGENT_MODEL":         true,
	"ANTHROPIC_SMALL_FAST_MODEL":         true,
	"ANTHROPIC_DEFAULT_FABLE_MODEL":      true,
	"ANTHROPIC_DEFAULT_FABLE_MODEL_NAME": true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL":       true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME":  true,
	"ANTHROPIC_DEFAULT_SONNET_MODEL":     true,
	"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":      true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME": true,
	"DISABLE_AUTOUPDATER":                true,
}

// envKeyRe 是环境变量名的合法形态(与 POSIX 环境变量命名一致)。
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config 是一条 Claude Code 运行配置预设,也是本地 JSON 持久化的存储格式。
// 字段镜像 Claude Code settings.json env 块的条目。
type Config struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	BaseURL     string `json:"base_url"` // ANTHROPIC_BASE_URL

	// 鉴权:api_key 与 auth_token 二选一(不能同时);列表/单查均掩码。
	APIKey    string `json:"api_key,omitempty"`    // ANTHROPIC_API_KEY(x-api-key)
	AuthToken string `json:"auth_token,omitempty"` // ANTHROPIC_AUTH_TOKEN(Bearer)

	// 模型。
	Model          string `json:"model,omitempty"`            // ANTHROPIC_MODEL(主模型)
	SubagentModel  string `json:"subagent_model,omitempty"`   // CLAUDE_CODE_SUBAGENT_MODEL
	SmallFastModel string `json:"small_fast_model,omitempty"` // ANTHROPIC_SMALL_FAST_MODEL(旧版)
	// 模型档位:对应 ANTHROPIC_DEFAULT_{TIER}_MODEL / _NAME 对。
	FableModel      string `json:"fable_model,omitempty"`
	FableModelName  string `json:"fable_model_name,omitempty"`
	OpusModel       string `json:"opus_model,omitempty"`
	OpusModelName   string `json:"opus_model_name,omitempty"`
	SonnetModel     string `json:"sonnet_model,omitempty"`
	SonnetModelName string `json:"sonnet_model_name,omitempty"`
	HaikuModel      string `json:"haiku_model,omitempty"`
	HaikuModelName  string `json:"haiku_model_name,omitempty"`

	// 其它。
	DisableAutoupdater bool              `json:"disable_autoupdater,omitempty"` // DISABLE_AUTOUPDATER=1
	ExtraEnv           map[string]string `json:"extra_env,omitempty"`           // 任意附加环境变量
	CreatedAt          time.Time         `json:"created_at"`
}

// Validate 校验配置字段。
func (c Config) Validate() error {
	if c.Name == "" {
		return errors.New("claude: name is required")
	}
	if strings.Contains(c.Name, "/") {
		return fmt.Errorf("claude: name %q must not contain '/'", c.Name)
	}
	if c.BaseURL == "" {
		return fmt.Errorf("claude %q: base_url is required", c.Name)
	}
	// base_url 必须是合法 http(s) 地址,避免 file:// 等非网络协议。
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("claude %q: base_url must be a valid http(s) URL", c.Name)
	}
	if strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.AuthToken) != "" {
		return fmt.Errorf("claude %q: api_key and auth_token are mutually exclusive", c.Name)
	}
	// 任意字段值不得含换行:命令是逐行生成的,换行会注入额外命令。
	for name, v := range map[string]string{
		"base_url": c.BaseURL, "api_key": c.APIKey, "auth_token": c.AuthToken,
		"model": c.Model, "subagent_model": c.SubagentModel, "small_fast_model": c.SmallFastModel,
		"fable_model": c.FableModel, "fable_model_name": c.FableModelName,
		"opus_model": c.OpusModel, "opus_model_name": c.OpusModelName,
		"sonnet_model": c.SonnetModel, "sonnet_model_name": c.SonnetModelName,
		"haiku_model": c.HaikuModel, "haiku_model_name": c.HaikuModelName,
	} {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("claude %q: %s must not contain newlines", c.Name, name)
		}
	}
	for k, v := range c.ExtraEnv {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("claude %q: invalid env key %q", c.Name, k)
		}
		if reservedEnv[k] {
			return fmt.Errorf("claude %q: env key %q is reserved", c.Name, k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("claude %q: env %q value must not contain newlines", c.Name, k)
		}
	}
	return nil
}

// Manager 负责 Claude 预设的增删改查与本地 JSON 持久化。
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
		return fmt.Errorf("claude: read config: %w", err)
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return fmt.Errorf("claude: parse config %s: %w", m.filePath, err)
	}
	for _, c := range cfgs {
		if c.Name == "" {
			return fmt.Errorf("claude: invalid entry in %s", m.filePath)
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
	tmp, err := os.CreateTemp(dir, ".claude-*.json")
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

// envKV 是命令中要写入的一条环境变量。
type envKV struct {
	key, value string
}

// envPairs 按固定顺序收集预设要写入的环境变量(空值跳过)。
// 顺序:BASE_URL → 鉴权 → MODEL → 档位 → 子代理/旧版 → DISABLE_AUTOUPDATER → extra_env(键排序)。
func envPairs(cfg Config) []envKV {
	var pairs []envKV
	add := func(k, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		pairs = append(pairs, envKV{k, v})
	}
	add("ANTHROPIC_BASE_URL", cfg.BaseURL)
	// 鉴权:auth_token 优先(Bearer),否则 api_key(x-api-key);由 Validate 保证至多一个。
	if strings.TrimSpace(cfg.AuthToken) != "" {
		add("ANTHROPIC_AUTH_TOKEN", cfg.AuthToken)
	} else {
		add("ANTHROPIC_API_KEY", cfg.APIKey)
	}
	add("ANTHROPIC_MODEL", cfg.Model)
	add("ANTHROPIC_DEFAULT_FABLE_MODEL", cfg.FableModel)
	add("ANTHROPIC_DEFAULT_FABLE_MODEL_NAME", cfg.FableModelName)
	add("ANTHROPIC_DEFAULT_OPUS_MODEL", cfg.OpusModel)
	add("ANTHROPIC_DEFAULT_OPUS_MODEL_NAME", cfg.OpusModelName)
	add("ANTHROPIC_DEFAULT_SONNET_MODEL", cfg.SonnetModel)
	add("ANTHROPIC_DEFAULT_SONNET_MODEL_NAME", cfg.SonnetModelName)
	add("ANTHROPIC_DEFAULT_HAIKU_MODEL", cfg.HaikuModel)
	add("ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME", cfg.HaikuModelName)
	add("CLAUDE_CODE_SUBAGENT_MODEL", cfg.SubagentModel)
	add("ANTHROPIC_SMALL_FAST_MODEL", cfg.SmallFastModel)
	if cfg.DisableAutoupdater {
		add("DISABLE_AUTOUPDATER", "1")
	}
	keys := make([]string, 0, len(cfg.ExtraEnv))
	for k := range cfg.ExtraEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(k, cfg.ExtraEnv[k])
	}
	return pairs
}

// cleanupVars 返回命令末尾需要清理的未用鉴权变量,避免与父 shell 已导出的
// ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN 冲突。
func cleanupVars(cfg Config) []string {
	used := ""
	if strings.TrimSpace(cfg.AuthToken) != "" {
		used = "ANTHROPIC_AUTH_TOKEN"
	} else if strings.TrimSpace(cfg.APIKey) != "" {
		used = "ANTHROPIC_API_KEY"
	}
	switch used {
	case "ANTHROPIC_AUTH_TOKEN":
		return []string{"ANTHROPIC_API_KEY"}
	case "ANTHROPIC_API_KEY":
		return []string{"ANTHROPIC_AUTH_TOKEN"}
	default: // 未配置鉴权:两个都清,防止继承父 shell 的鉴权变量。
		return []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}
	}
}

// EnvVars 返回预设对应的环境变量集合(与 BuildCommand 生成的命令一致),
// 供"覆盖本地 Claude Code 配置"等场景直接写入 settings.json 的 env 块。
func (c Config) EnvVars() map[string]string {
	out := make(map[string]string, 8)
	for _, p := range envPairs(c) {
		out[p.key] = p.value
	}
	return out
}

// DefaultSettingsPath 返回本地 Claude Code 的 settings.json 路径(用户主目录下的 .claude)。
func DefaultSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claude: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// ApplyToLocalSettings 将预设的环境变量覆盖到本地 settings.json 的 env 块:
// 保留 settings.json 的其余字段与 env 块中未涉及的键;同时清理预设未使用的鉴权变量
// (避免 ANTHROPIC_API_KEY 与 ANTHROPIC_AUTH_TOKEN 并存冲突)。文件不存在时创建,
// 写入采用临时文件 + 改名原子写。
func ApplyToLocalSettings(path string, cfg Config) error {
	env := cfg.EnvVars()
	cleanup := cleanupVars(cfg)

	var settings map[string]any
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("claude: parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		settings = make(map[string]any)
	default:
		return fmt.Errorf("claude: read %s: %w", path, err)
	}

	envBlock, _ := settings["env"].(map[string]any)
	if envBlock == nil {
		envBlock = make(map[string]any)
	}
	for k, v := range env {
		envBlock[k] = v
	}
	for _, k := range cleanup {
		delete(envBlock, k)
	}
	settings["env"] = envBlock

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("claude: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
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

// BuildCommand 由预设生成两个 shell 的一键启动命令(设置环境变量后启动 claude)。
// 纯函数,结果确定,不依赖任何外部状态。
func BuildCommand(cfg Config) Command {
	pairs := envPairs(cfg)
	cleanup := cleanupVars(cfg)

	ps := make([]string, 0, len(pairs)+len(cleanup)+1)
	sh := make([]string, 0, len(pairs)+len(cleanup)+1)
	for _, p := range pairs {
		ps = append(ps, `$env:`+p.key+` = "`+escapePS(p.value)+`"`)
		sh = append(sh, `export `+p.key+`="`+escapeSh(p.value)+`"`)
	}
	for _, v := range cleanup {
		ps = append(ps, `Remove-Item Env:`+v+` -ErrorAction SilentlyContinue`)
		sh = append(sh, `unset `+v)
	}
	ps = append(ps, "claude")
	sh = append(sh, "claude")

	return Command{
		PowerShell: strings.Join(ps, "\n"),
		Bash:       strings.Join(sh, "\n"),
	}
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
