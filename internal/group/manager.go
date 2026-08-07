package group

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var (
	// ErrNotFound 表示分组不存在。
	ErrNotFound = errors.New("group: not found")
	// ErrExists 表示分组已存在。
	ErrExists = errors.New("group: already exists")
	// ErrPersist 表示配置写入本地 JSON 失败。
	ErrPersist = errors.New("group: persist failed")
)

// Manager 负责分组的注册、增删改查与本地 JSON 持久化。
type Manager struct {
	mu       sync.RWMutex
	groups   map[string]Config
	filePath string
}

// NewManager 从指定 JSON 文件加载分组配置;文件不存在时视为空配置。
func NewManager(filePath string) (*Manager, error) {
	m := &Manager{groups: make(map[string]Config), filePath: filePath}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load 启动时读取本地 JSON 中的分组配置。
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("group: read config: %w", err)
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return fmt.Errorf("group: parse config %s: %w", m.filePath, err)
	}
	for _, c := range cfgs {
		c.URL = c.EffectiveURL()
		if err := c.Validate(); err != nil {
			return err
		}
		if _, ok := m.groups[c.Name]; ok {
			return fmt.Errorf("group: duplicate name %q in config", c.Name)
		}
		m.groups[c.Name] = c
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
	tmp, err := os.CreateTemp(dir, ".groups-*.json")
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

// snapshot 收集所有分组配置(按名称排序)。
func (m *Manager) snapshot() []Config {
	cfgs := make([]Config, 0, len(m.groups))
	for _, c := range m.groups {
		cfgs = append(cfgs, c)
	}
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Name < cfgs[j].Name })
	return cfgs
}

// normalize 校验并归一化分组配置(先补全默认 URL,再校验生效 URL)。
func (m *Manager) normalize(c Config) (Config, error) {
	c.URL = c.EffectiveURL()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// urlCollides 判断新 URL 与既有分组 URL 冲突:相同,或互为路径前缀且边界段为
// 分组的保留虚拟端点段 "v1"(此时长 URL 会遮蔽短 URL 的 /v1/... 端点)。
// 普通嵌套 URL(如 /team-a 与 /team-a/sub)是合法命名空间,不冲突。
func urlCollides(groups map[string]Config, u string) bool {
	for _, g := range groups {
		e := g.URL
		if e == u {
			return true
		}
		if len(u) > len(e) && strings.HasPrefix(u, e) && u[len(e)] == '/' {
			if rest := u[len(e)+1:]; rest == "v1" || strings.HasPrefix(rest, "v1/") {
				return true
			}
		}
		if len(e) > len(u) && strings.HasPrefix(e, u) && e[len(u)] == '/' {
			if rest := e[len(u)+1:]; rest == "v1" || strings.HasPrefix(rest, "v1/") {
				return true
			}
		}
	}
	return false
}

// Add 添加分组;已存在或 URL 冲突时返回错误,持久化失败时回滚并返回 ErrPersist。
func (m *Manager) Add(c Config) error {
	c, err := m.normalize(c)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[c.Name]; ok {
		return fmt.Errorf("%w: %s", ErrExists, c.Name)
	}
	if urlCollides(m.groups, c.URL) {
		return fmt.Errorf("group: url %q collides with an existing group", c.URL)
	}
	m.groups[c.Name] = c
	if err := m.save(); err != nil {
		delete(m.groups, c.Name) // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Update 修改分组配置;不存在时返回 ErrNotFound,持久化失败时回滚。
func (m *Manager) Update(c Config) error {
	c, err := m.normalize(c)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.groups[c.Name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, c.Name)
	}
	others := make(map[string]Config, len(m.groups))
	for n, g := range m.groups {
		if n != c.Name {
			others[n] = g
		}
	}
	if urlCollides(others, c.URL) {
		return fmt.Errorf("group: url %q collides with another group", c.URL)
	}
	m.groups[c.Name] = c
	if err := m.save(); err != nil {
		m.groups[c.Name] = old // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Delete 删除分组;不存在时返回 ErrNotFound,持久化失败时回滚。
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.groups[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	delete(m.groups, name)
	if err := m.save(); err != nil {
		m.groups[name] = old // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Get 按名称查询分组;不存在时返回 ErrNotFound。
func (m *Manager) Get(name string) (Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.groups[name]
	if !ok {
		return Config{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return c, nil
}

// List 返回所有分组配置,按名称排序。
func (m *Manager) List() []Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot()
}

// ResolveURL 按最长前缀匹配请求路径所属的分组,返回分组与剩余路径。
// 要求分组 URL 后是路径边界("/")或路径恰好等于 URL,避免 "/team-a" 误配 "/team-ab"。
func (m *Manager) ResolveURL(path string) (Config, string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best Config
	var bestLen int
	for _, c := range m.groups {
		u := c.URL
		if len(u) <= bestLen || !strings.HasPrefix(path, u) {
			continue
		}
		if len(path) > len(u) && path[len(u)] != '/' {
			continue
		}
		best = c
		bestLen = len(u)
	}
	if bestLen == 0 {
		return Config{}, "", false
	}
	rest := path[bestLen:]
	if rest == "" {
		rest = "/"
	}
	return best, rest, true
}
