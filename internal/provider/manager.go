package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	// ErrNotFound 表示供应商不存在。
	ErrNotFound = errors.New("provider: not found")
	// ErrExists 表示供应商已存在。
	ErrExists = errors.New("provider: already exists")
	// ErrPersist 表示配置写入本地 JSON 失败(服务器端错误)。
	ErrPersist = errors.New("provider: persist failed")
)

// Manager 负责供应商配置的注册、增删改查与本地 JSON 持久化。
type Manager struct {
	mu        sync.RWMutex
	providers map[string]Provider
	filePath  string
}

// NewManager 从指定 JSON 文件加载供应商配置;文件不存在时视为空配置。
func NewManager(filePath string) (*Manager, error) {
	m := &Manager{providers: make(map[string]Provider), filePath: filePath}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load 启动时读取本地 JSON 中的供应商配置。
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("provider: read config: %w", err)
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return fmt.Errorf("provider: parse config %s: %w", m.filePath, err)
	}
	for _, cfg := range cfgs {
		if err := cfg.Validate(); err != nil {
			return err
		}
		p, err := New(cfg)
		if err != nil {
			return err
		}
		m.providers[cfg.Name] = p
	}
	return nil
}

// save 将当前配置写回本地 JSON,采用临时文件 + 改名避免写入中断损坏配置。
func (m *Manager) save() error {
	data, err := json.MarshalIndent(m.snapshot(), "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.filePath)
	tmp, err := os.CreateTemp(dir, ".providers-*.json")
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

// snapshot 收集所有供应商配置(按名称排序)。
func (m *Manager) snapshot() []Config {
	cfgs := make([]Config, 0, len(m.providers))
	for _, p := range m.providers {
		cfgs = append(cfgs, p.Config())
	}
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Name < cfgs[j].Name })
	return cfgs
}

// Add 添加供应商;已存在时返回 ErrExists,持久化失败时回滚并返回 ErrPersist。
func (m *Manager) Add(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[cfg.Name]; ok {
		return fmt.Errorf("%w: %s", ErrExists, cfg.Name)
	}
	p, err := New(cfg)
	if err != nil {
		return err
	}
	m.providers[cfg.Name] = p
	if err := m.save(); err != nil {
		delete(m.providers, cfg.Name) // 回滚内存态,保持与磁盘一致
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Update 修改供应商配置;不存在时返回 ErrNotFound,持久化失败时回滚并返回 ErrPersist。
func (m *Manager) Update(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.providers[cfg.Name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, cfg.Name)
	}
	p, err := New(cfg)
	if err != nil {
		return err
	}
	m.providers[cfg.Name] = p
	if err := m.save(); err != nil {
		m.providers[cfg.Name] = old // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Delete 删除供应商;不存在时返回 ErrNotFound,持久化失败时回滚并返回 ErrPersist。
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.providers[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	delete(m.providers, name)
	if err := m.save(); err != nil {
		m.providers[name] = old // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// SetModels 仅更新供应商的模型列表,保留其余字段(api_key/base_url 等),
// 避免"整份替换"覆盖并发的配置修改;不存在时返回 ErrNotFound。
func (m *Manager) SetModels(name string, models []ModelConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.providers[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	cfg := p.Config()
	cfg.Models = models
	np, err := New(cfg)
	if err != nil {
		return err
	}
	m.providers[name] = np
	if err := m.save(); err != nil {
		m.providers[name] = p // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Get 按名称查询供应商;不存在时返回 ErrNotFound。
func (m *Manager) Get(name string) (Provider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.providers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return p, nil
}

// List 返回所有供应商配置,按名称排序。
func (m *Manager) List() []Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot()
}

// contextMarkerRe 匹配上下文标记的内容:[<数字>] / [<数字>k] / [<数字>m],大小写不敏感。
// 覆盖 Claude Code 的 [1M] 以及按上下文窗口派生的 [128k] / [200k] / [1m] 等后缀。
var contextMarkerRe = regexp.MustCompile(`(?i)^\d+(?:k|m)?$`)

// StripContextMarker 剥离模型 id 末尾的上下文标记(如 [1M] / [128k] / [200k] / [1m])。
// 该标记是 Claude Code 客户端侧的上下文窗口声明,真实上游通常不接受带标记的模型名
// (Anthropic/Bedrock 会 400、OpenAI 兼容接口拒绝、部分厂商启发式回退),网关需在
// 路由/转发前剥掉;幂等——若已剥离则原样返回。
func StripContextMarker(model string) string {
	i := strings.LastIndex(model, "[")
	if i < 0 || !strings.HasSuffix(model, "]") {
		return model
	}
	tag := model[i+1 : len(model)-1]
	if contextMarkerRe.MatchString(tag) {
		return model[:i]
	}
	return model
}

// Resolve 将 "{供应商名}@{模型名}" 解析为对应供应商与模型名。
// 分隔符为 "@",供应商名与模型名禁止含 "@"(见 Config.Validate),
// 故按首个 "@" 精确切分即可,无需前缀匹配。入口先剥离 [1M] 等上下文标记。
func (m *Manager) Resolve(model string) (Provider, string, error) {
	model = StripContextMarker(model)
	m.mu.RLock()
	defer m.mu.RUnlock()
	name, rest, ok := strings.Cut(model, "@")
	if !ok || rest == "" {
		return nil, "", fmt.Errorf("%w: no provider for model %q", ErrNotFound, model)
	}
	p, exists := m.providers[name]
	if !exists {
		return nil, "", fmt.Errorf("%w: no provider for model %q", ErrNotFound, model)
	}
	return p, rest, nil
}
