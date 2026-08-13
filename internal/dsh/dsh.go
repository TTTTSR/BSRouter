// Package dsh 管理 DeepSeek Harness (dsh) 运行配置预设:一条预设对应把 BSRouter
// 作为一条自定义供应商写入本地 `~/.dsh/settings.yaml` 的 llm-pi-ai.providers map。
//
// dsh 是 DeepSeek Harness 的本地配置(yaml 文件,Windows %USERPROFILE%\.dsh\settings.yaml),
// 顶层是几个映射:ui-onboarding / llm-pi-ai(LLM 供应商配置)/ agent-default-model。
// llm-pi-ai.providers 是供应商 map,每个供应商一条,支持:
//
//	<name>:
//	  displayName: <可选显示名>
//	  apiKeyEnv: <环境变量名,harness 从该变量读密钥>
//	  apiKey: <可选,内联密钥(apply-local 直接把真实 key 写进文件)>
//	  api: <wire 接口格式,如 anthropic-messages / openai-completions / responses>
//	  baseURL: <网关统一 API 入口>
//	  models:
//	    - id: <模型 id>
//	      name: <可选显示名>
//	      contextWindow: <上下文窗口 tokens>
//	      maxTokens: <最大输出 tokens>
//
// 本包实现把 BSRouter 作为自定义供应商覆盖进该配置:保留其余内置/自定义供应商与
// 顶层其它字段,只写 llm-pi-ai.providers.<name>。apply-local 把网关模型列表(预设
// 直接配置的 models 优先,留空回退网关全部可路由模型)写入供应商 models,按模型
// 上下文窗口换算 contextWindow 与 maxTokens。
//
// 鉴权密钥两条注入途径(用户决策):「复制一键命令」时把真实密钥放进命令
// (设置 apiKeyEnv 指向的环境变量,再启动 harness);「应用本地」时直接把真实密钥
// 内联写进 settings.yaml 文件(apiKey 字段)。两条路径都可以把密钥送达 harness。
//
// 由于 BSRouter 是零外部依赖(纯 Go 标准库),dsh 配置又是 YAML,本包内置一个
// 最小按缩进的 YAML 块编辑器(见 upsertMappingPath),在保留其余内容的前提下
// 替换/插入 llm-pi-ai.providers.<name> 块。写入采用临时文件 + 改名原子写。
package dsh

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
	ErrNotFound = errors.New("dsh: not found")
	// ErrExists 表示同名预设已存在。
	ErrExists = errors.New("dsh: already exists")
	// ErrPersist 表示配置写入本地 JSON 失败。
	ErrPersist = errors.New("dsh: persist failed")
)

// DefaultAPI 是写入供应商条目的默认接口格式(anthropic-messages),对应 BSRouter
// 统一 API 的 /api/v1/messages(与用户现有 bsr 示例一致)。
const DefaultAPI = "anthropic-messages"

// defaultContextTokens 是模型未配置上下文窗口时写入 contextWindow 的默认值(200k)。
const defaultContextTokens = 200000

// defaultMaxTokens 是模型未配置最大输出 tokens 时写入 maxTokens 的默认值。
const defaultMaxTokens = 65536

// llmPiAIKey / providersKey 是 settings.yaml 里操作的关键路径段。
const (
	llmPiAIKey   = "llm-pi-ai"
	providersKey = "providers"
)

// Config 是一条 dsh 运行配置预设,也是本地 JSON 持久化的存储格式。
// 与 zcode 预设一致:只配置 api_key(可选)与模型列表,接口格式 api / base_url /
// apiKeyEnv / display_name 均由 apply-local 自动派生。
type Config struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// APIKey 是写入供应商条目的鉴权密钥(掩码返回)。apply-local 直接把真实密钥
	// 内联写进 settings.yaml(apiKey 字段);留空注入系统默认 key。
	APIKey string `json:"api_key,omitempty"`
	// Models 是预设直接配置的模型列表(网关可路由 id:"{供应商}@{模型}" 合成或
	// 聚合裸名)。留空回退网关全部可路由模型。
	Models    []string  `json:"models,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate 校验配置字段。
func (c Config) Validate() error {
	if c.Name == "" {
		return errors.New("dsh: name is required")
	}
	if strings.Contains(c.Name, "/") {
		return fmt.Errorf("dsh: name %q must not contain '/'", c.Name)
	}
	if strings.ContainsAny(c.Name, "\r\n") {
		return fmt.Errorf("dsh: name %q must not contain newlines", c.Name)
	}
	if c.APIKey != "" && strings.ContainsAny(c.APIKey, "\r\n") {
		return fmt.Errorf("dsh %q: api_key must not contain newlines", c.Name)
	}
	seen := make(map[string]bool, len(c.Models))
	for i, m := range c.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			return fmt.Errorf("dsh %q: models[%d] must not be empty", c.Name, i)
		}
		if strings.ContainsAny(m, "\r\n \t") {
			return fmt.Errorf("dsh %q: models[%d] must not contain whitespace or newlines", c.Name, i)
		}
		if seen[m] {
			return fmt.Errorf("dsh %q: duplicate model %q", c.Name, m)
		}
		seen[m] = true
	}
	return nil
}

// EffectiveAPIKeyEnv 返回实际使用的环境变量名(始终按名称派生,与 zcode 一致自动填充)。
func (c Config) EffectiveAPIKeyEnv() string {
	return DerivedAPIKeyEnv(c.Name)
}

// DerivedAPIKeyEnv 从预设名派生环境变量名:字母数字转大写,补 _API_KEY 后缀。
func DerivedAPIKeyEnv(name string) string {
	var b strings.Builder
	for _, r := range name {
		isLower := r >= 'a' && r <= 'z'
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		switch {
		case r == '-':
			b.WriteRune('_')
		case r == '_' || isLower || isUpper || isDigit:
			b.WriteRune(r)
		}
	}
	s := strings.ToUpper(b.String())
	if s == "" {
		s = "BSROUTER"
	}
	return s + "_API_KEY"
}

// Manager 负责 dsh 预设的增删改查与本地 JSON 持久化。
type Manager struct {
	mu       sync.RWMutex
	presets  map[string]Config
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

func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dsh: read config: %w", err)
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return fmt.Errorf("dsh: parse config %s: %w", m.filePath, err)
	}
	for _, c := range cfgs {
		if c.Name == "" {
			return fmt.Errorf("dsh: invalid entry in %s", m.filePath)
		}
		m.presets[c.Name] = c
	}
	return nil
}

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
	return atomicWriteFile(m.filePath, data)
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
		m.presets[name] = old
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

// DefaultSettingsPath 返回本地 dsh 的 settings.yaml 路径(用户主目录下的 .dsh)。
func DefaultSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("dsh: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".dsh", "settings.yaml"), nil
}

// atomicWriteFile 临时文件 + 改名原子写(目录不存在则创建)。
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("dsh: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
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

// ---- YAML 块编辑器(零依赖,按缩进操作)----

// yamlLine 是解析后的一行:raw 为原始文本,indent 为缩进空格数,body 为去缩进文本。
// key 非空表示这是一个映射键行;isListItem 表示 "- " 列表项。
type yamlLine struct {
	raw         string
	indent      int
	body        string
	key         string
	inlineValue string
	isListItem  bool
}

// splitYAMLLines 把 YAML 文本按行拆分并解析每行的缩进与键形态。
func splitYAMLLines(text string) []yamlLine {
	rawLines := strings.Split(text, "\n")
	lines := make([]yamlLine, 0, len(rawLines))
	for _, rl := range rawLines {
		rl = strings.TrimSuffix(rl, "\r")
		trimLeft := strings.TrimLeft(rl, " \t")
		indent := len(rl) - len(trimLeft)
		body := trimLeft
		l := yamlLine{raw: rl, indent: indent, body: body}
		if body != "" && !strings.HasPrefix(body, "#") {
			work := body
			if strings.HasPrefix(work, "- ") {
				l.isListItem = true
				work = strings.TrimSpace(work[2:])
			} else if work == "-" {
				l.isListItem = true
				work = ""
			}
			if work != "" {
				if ci := strings.Index(work, ":"); ci >= 0 {
					key := strings.TrimSpace(work[:ci])
					if key != "" && !strings.Contains(key, " ") {
						l.key = key
						l.inlineValue = strings.TrimSpace(work[ci+1:])
					}
				}
			}
		}
		lines = append(lines, l)
	}
	return lines
}

// upsertMappingPath 在 YAML 行序列中,让从 path[0] 到 path[len-1] 的嵌套映射键存在,
// 并把末级映射键的内容替换为 content。path 是逐层键名,如 ["llm-pi-ai","providers","dev"]。
// content 是"要写进末级键之下的一组行"(不含键行,以 0 基准缩进,列表项除外)。
func upsertMappingPath(lines []yamlLine, path []string, content []yamlLine) ([]yamlLine, bool) {
	return upsertRec(lines, path, content, 0, 0, true)
}

// upsertRec 递归实现 upsertMappingPath。
// depth 为当前路径深度;parentKeyIndent 为父键缩进;isRoot 表示正在处理顶层。
func upsertRec(lines []yamlLine, path []string, content []yamlLine, depth int, parentKeyIndent int, isRoot bool) ([]yamlLine, bool) {
	key := path[depth]

	// 找 key 的映射键行。顶层缩进必须为 0;后续级必须 > parentKeyIndent 且取最小缩进
	// 的那个(直接子级)。listitem 不算映射键。
	found := -1
	foundIndent := -1
	for i, l := range lines {
		if l.key != key || l.isListItem {
			continue
		}
		if isRoot {
			if l.indent == 0 {
				found = i
				foundIndent = l.indent
				break
			}
		} else if l.indent > parentKeyIndent {
			if found < 0 || l.indent < foundIndent {
				found = i
				foundIndent = l.indent
			}
		}
	}

	if found < 0 {
		// key 不存在。
		insertIndent := 0
		if !isRoot {
			insertIndent = parentKeyIndent + 2
		}
		if depth == len(path)-1 {
			// 末级缺失:在末尾插入 key 空键 + content。
			aligned := alignContent(content, insertIndent+2)
			out := make([]yamlLine, 0, len(lines)+len(aligned)+1)
			out = append(out, lines...)
			out = append(out, makeKeyEmpty(insertIndent, key))
			out = append(out, aligned...)
			return out, true
		}
		// 中间级缺失:先在末尾插入空键,再递进。
		out := make([]yamlLine, 0, len(lines)+1)
		out = append(out, lines...)
		out = append(out, makeKeyEmpty(insertIndent, key))
		return upsertRec(out, path, content, depth+1, insertIndent, false)
	}

	// key 存在。计算其子树范围 [found, end):下一个同级映射键行(缩进 <= foundIndent)。
	end := len(lines)
	for i := found + 1; i < len(lines); i++ {
		if lines[i].body != "" && !lines[i].isListItem && lines[i].key != "" && lines[i].indent <= foundIndent {
			end = i
			break
		}
	}

	if depth == len(path)-1 {
		// 末级:整块替换子树 [found+1, end) 为 content,保留键行 lines[found] 及其内联值。
		aligned := alignContent(content, foundIndent+2)
		out := make([]yamlLine, 0, len(lines)-(end-found-1)+len(aligned))
		out = append(out, lines[:found+1]...)
		out = append(out, aligned...)
		out = append(out, lines[end:]...)
		return out, true
	}

	// 非末级:进入子树 lines[found+1 : end] 递归。
	sub := lines[found+1 : end]
	newSub, ok := upsertRec(sub, path, content, depth+1, foundIndent, false)
	if ok {
		out := make([]yamlLine, 0, len(lines)+len(newSub))
		out = append(out, lines[:found]...)
		out = append(out, lines[found])
		out = append(out, newSub...)
		out = append(out, lines[end:]...)
		return out, true
	}
	return lines, false
}

// makeKeyEmpty 生成一个 "key:"(无子树)的行。
func makeKeyEmpty(indent int, key string) yamlLine {
	pad := strings.Repeat(" ", indent)
	return yamlLine{raw: pad + key + ":", indent: indent, key: key, body: key + ":"}
}

// alignContent 把 content 行整体缩进对齐到 baseIndent(以 content 自身最小缩进为基准)。
func alignContent(content []yamlLine, baseIndent int) []yamlLine {
	out := make([]yamlLine, len(content))
	if len(content) == 0 {
		return out
	}
	minInd := content[0].indent
	for _, l := range content {
		if l.indent < minInd {
			minInd = l.indent
		}
	}
	shift := baseIndent - minInd
	for i, l := range content {
		ni := l.indent + shift
		nraw := strings.Repeat(" ", ni) + l.body
		out[i] = yamlLine{raw: nraw, indent: ni, body: l.body, key: l.key, isListItem: l.isListItem, inlineValue: l.inlineValue}
	}
	return out
}

// renderYAML 把行序列渲染回文本(保留原始未改动行的 raw),文末保留单个换行。
func renderYAML(lines []yamlLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.raw)
		b.WriteString("\n")
	}
	return b.String()
}

// ensureTopKey 确保顶层 key 存在(不存在则追加到尾)。
func ensureTopKey(lines []yamlLine, key string) ([]yamlLine, bool) {
	for _, l := range lines {
		if l.key == key && l.indent == 0 {
			return lines, false
		}
	}
	return append(lines, makeKeyEmpty(0, key)), true
}

// ensureChildKey 确保 parentKey 的子树下存在 childKey:找 parentKey 顶层键行及其子块,
// 若 childKey 缺失则在其子块末尾补齐空键。
func ensureChildKey(lines []yamlLine, parentKey, childKey string) ([]yamlLine, bool) {
	pIdx := -1
	pIndent := -1
	for i, l := range lines {
		if l.key == parentKey && l.indent == 0 {
			pIdx = i
			pIndent = l.indent
			break
		}
	}
	if pIdx < 0 {
		return lines, false
	}
	end := len(lines)
	for i := pIdx + 1; i < len(lines); i++ {
		if lines[i].body != "" && !lines[i].isListItem && lines[i].key != "" && lines[i].indent <= pIndent {
			end = i
			break
		}
	}
	for i := pIdx + 1; i < end; i++ {
		if lines[i].key == childKey && lines[i].indent > pIndent {
			return lines, false
		}
	}
	curIndent := pIndent + 2
	for i := pIdx + 1; i < end; i++ {
		if lines[i].key != "" && lines[i].indent > pIndent && lines[i].indent < curIndent {
			curIndent = lines[i].indent
		}
	}
	lead := makeKeyEmpty(curIndent, childKey)
	out := make([]yamlLine, 0, len(lines)+1)
	out = append(out, lines[:end]...)
	out = append(out, lead)
	out = append(out, lines[end:]...)
	return out, true
}

// ---- 供应商条目构建(dsh settings.yaml 的 wire 格式)----

// ProviderSpec 描述 apply-local 要写入的一个 dsh 供应商条目(providers map 里一条)。
type ProviderSpec struct {
	Name        string         // 供应商键名(providers map 的 key)
	DisplayName string         // displayName(空 → 不写,harness 回退 Name)
	APIKey      string         // 内联 apiKey(apply-local 直接把真实 key 写进文件)
	APIKeyEnv   string         // apiKeyEnv 环境变量名(必填)
	API         string         // api 接口格式(默认 DefaultAPI)
	BaseURL     string         // baseURL(网关统一 API 入口)
	Models      []string       // 写入的模型 id 列表
	Windows     map[string]int // 模型 id → 上下文窗口(tokens),未配置回退默认 200k
}

// BuildProviderBlock 由一个 ProviderSpec 生成供应商条目块(0 基准缩进的行序列),
// 经 upsertMappingPath + alignContent 对齐到 providers map 层。
func BuildProviderBlock(spec ProviderSpec) []yamlLine {
	api := spec.API
	if api == "" {
		api = DefaultAPI
	}
	// 不输出键名行(键由 upsertMappingPath 负责),从 displayName 开始。
	var body strings.Builder
	if spec.DisplayName != "" {
		body.WriteString("  displayName: " + plainScalar(spec.DisplayName))
	}
	if spec.APIKey != "" {
		if body.Len() > 0 {
			body.WriteString("\n")
		}
		body.WriteString("  apiKey: " + plainScalar(spec.APIKey))
	}
	if body.Len() > 0 {
		body.WriteString("\n")
	}
	body.WriteString("  apiKeyEnv: " + plainScalar(spec.APIKeyEnv))
	body.WriteString("\n  api: " + plainScalar(api))
	body.WriteString("\n  baseURL: " + plainScalar(spec.BaseURL))
	if len(spec.Models) > 0 {
		body.WriteString("\n  models:")
		for _, id := range spec.Models {
			win := spec.Windows[id]
			if win <= 0 {
				win = defaultContextTokens
			}
			body.WriteString("\n    - id: " + plainScalar(id))
			body.WriteString("\n      contextWindow: " + itoa(win))
			body.WriteString("\n      maxTokens: " + itoa(defaultMaxTokens))
		}
	}
	return splitYAMLLines(body.String())
}
func plainScalar(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.ContainsAny(s, "\r\n")
	if !needsQuote {
		switch s[0] {
		case '-', '?', '&', '*', '!', '|', '>', '%', '@', '`':
			needsQuote = true
		}
	}
	if !needsQuote {
		return s
	}
	quoted := strings.ReplaceAll(s, `\`, `\\`)
	quoted = strings.ReplaceAll(quoted, `"`, `\"`)
	return `"` + quoted + `"`
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// ApplyToLocalSettings 把 spec 覆盖进本地 dsh settings.yaml 的 llm-pi-ai.providers
// map:保留其它内置/自定义供应商与顶层其它字段,只替换/插入 spec.Name 这条供应商。
// 文件不存在时创建。写入采用临时文件 + 改名原子写。
func ApplyToLocalSettings(path string, spec ProviderSpec) error {
	var text string
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		text = string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))) // 剥离 UTF-8 BOM
	case errors.Is(err, os.ErrNotExist):
		text = ""
	default:
		return fmt.Errorf("dsh: read %s: %w", path, err)
	}

	lines := splitYAMLLines(text)
	// 1) 顶层 llm-pi-ai 存在。
	lines, _ = ensureTopKey(lines, llmPiAIKey)
	// 2) llm-pi-ai 下 providers 存在。
	lines, _ = ensureChildKey(lines, llmPiAIKey, providersKey)
	// 3) 写 name 块。
	block := BuildProviderBlock(spec)
	lines, _ = upsertMappingPath(lines, []string{llmPiAIKey, providersKey, spec.Name}, block)

	out := strings.TrimRight(renderYAML(lines), "\n") + "\n"
	return atomicWriteFile(path, []byte(out))
}

// Command 是一键启动命令(设置 apiKeyEnv 环境变量后启动 harness)。
type Command struct {
	PowerShell string `json:"powershell"`
	Bash       string `json:"bash"`
}

// BuildCommand 由预设生成两个 shell 的一键命令:把 apiKeyEnv 指向的环境变量设为
// apiKey,然后启动 dsh harness(Windows 用 %HOMEDRIVE%%HOMEPATH%\.dsh 下可执行)。
// 纯函数,结果确定。apiKeyEnv 留空时按预设名派生。
func BuildCommand(cfg Config) Command {
	envName := cfg.EffectiveAPIKeyEnv()
	ps := make([]string, 0, 2)
	sh := make([]string, 0, 2)
	if cfg.APIKey != "" {
		ps = append(ps, `$env:`+envName+` = "`+escapePS(cfg.APIKey)+`"`)
		sh = append(sh, `export `+envName+`="`+escapeSh(cfg.APIKey)+`"`)
	}
	ps = append(ps, "dsh")
	sh = append(sh, "dsh")
	return Command{
		PowerShell: strings.Join(ps, "\n"),
		Bash:       strings.Join(sh, "\n"),
	}
}

// escapePS 转义 PowerShell 双引号字符串字面量内容。
// escapePS 转义 PowerShell 双引号字符串字面量内容。
func escapePS(s string) string {
	return strings.NewReplacer(
		"`", "``",
		"\"", "`\"",
		"$", "`$",
	).Replace(s)
}

// escapeSh 转义 bash 双引号字符串字面量内容。
func escapeSh(s string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"$", "\\$",
		"`", "\\`",
	).Replace(s)
}
