// Package apikey 管理下游模型请求使用的 API Key:生成、查询、删除与本地 JSON 持久化。
// Key 与网关自身的鉴权 key 相互独立,专供模型请求(/api)鉴权。
package apikey

import (
	"crypto/rand"
	"crypto/subtle"
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
	// ErrNotFound 表示 Key 不存在。
	ErrNotFound = errors.New("apikey: not found")
	// ErrExists 表示同名 Key 已存在。
	ErrExists = errors.New("apikey: already exists")
	// ErrPersist 表示配置写入本地 JSON 失败。
	ErrPersist = errors.New("apikey: persist failed")
)

// Config 是一条受管 API Key,也是 JSON 持久化的存储格式。
type Config struct {
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

// Manager 负责 API Key 的注册、增删查与本地 JSON 持久化。
type Manager struct {
	mu       sync.RWMutex
	keys     map[string]Config // name -> Config
	filePath string
}

// NewManager 从指定 JSON 文件加载 API Key;文件不存在时视为空。
func NewManager(filePath string) (*Manager, error) {
	m := &Manager{keys: make(map[string]Config), filePath: filePath}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load 启动时读取本地 JSON 中的 API Key。
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("apikey: read config: %w", err)
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return fmt.Errorf("apikey: parse config %s: %w", m.filePath, err)
	}
	for _, c := range cfgs {
		if c.Name == "" || c.Key == "" {
			return fmt.Errorf("apikey: invalid entry in %s", m.filePath)
		}
		m.keys[c.Name] = c
	}
	return nil
}

// save 将当前配置写回本地 JSON,采用临时文件 + 改名避免写入中断损坏配置。
func (m *Manager) save() error {
	cfgs := make([]Config, 0, len(m.keys))
	for _, c := range m.keys {
		cfgs = append(cfgs, c)
	}
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Name < cfgs[j].Name })
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.filePath)
	tmp, err := os.CreateTemp(dir, ".apikeys-*.json")
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

// Generate 生成一个新 API Key(格式 "sk-" + 64 位 a-zA-Z0-9)。
// 名称已存在时返回 ErrExists,持久化失败时回滚并返回 ErrPersist。
func (m *Manager) Generate(name string) (Config, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Config{}, fmt.Errorf("apikey: name is required")
	}
	if strings.Contains(name, "/") {
		return Config{}, fmt.Errorf("apikey: name must not contain '/'")
	}
	cfg := Config{Name: name, Key: generate(), CreatedAt: time.Now()}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[name]; ok {
		return Config{}, fmt.Errorf("%w: %s", ErrExists, name)
	}
	m.keys[name] = cfg
	if err := m.save(); err != nil {
		delete(m.keys, name)
		return Config{}, fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return cfg, nil
}

// Get 返回指定名称的 API Key(含完整 key);不存在时返回 ErrNotFound。
func (m *Manager) Get(name string) (Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.keys[name]
	if !ok {
		return Config{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return c, nil
}

// List 返回全部 API Key(按名称排序)。
func (m *Manager) List() []Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfgs := make([]Config, 0, len(m.keys))
	for _, c := range m.keys {
		cfgs = append(cfgs, c)
	}
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Name < cfgs[j].Name })
	return cfgs
}

// Delete 删除指定名称的 API Key;不存在时返回 ErrNotFound。
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.keys[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	delete(m.keys, name)
	if err := m.save(); err != nil {
		m.keys[name] = old // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// Valid 判断某个 Key 是否为受管的有效 Key(常数时间比较)。
func (m *Manager) Valid(key string) bool {
	if key == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.keys {
		if subtle.ConstantTimeCompare([]byte(c.Key), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

// Count 返回受管 Key 的数量。
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys)
}

// generate 生成 "sk-" + 64 位 a-zA-Z0-9 随机字符串。
func generate() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		panic("apikey: crypto/rand unavailable: " + err.Error())
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "sk-" + string(b)
}
