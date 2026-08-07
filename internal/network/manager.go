package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	// ErrPersist 表示出口配置写入本地 JSON 失败。
	ErrPersist = errors.New("network: persist failed")
)

// Config 是用户配置的出口地址(JSON 持久化形态),用于 NAT 部署下替代自动探测。
// EgressHost 为出口 IP 或域名(不含协议/端口),EgressPort 为公网映射端口。
type Config struct {
	EgressHost string `json:"egress_host,omitempty"`
	EgressPort string `json:"egress_port,omitempty"`
}

// Validate 校验出口配置:EgressHost 必填且不含协议/空白/路径;端口为空默认 "80",
// 否则必须为 1–65535 的数字。
func (c Config) Validate() error {
	host := strings.TrimSpace(c.EgressHost)
	if host == "" {
		return errors.New("network: egress_host is required")
	}
	if strings.ContainsAny(host, " /:@?#[]") {
		return fmt.Errorf("network: egress_host %q must be a plain IP or hostname (no scheme/port/path)", host)
	}
	if strings.ContainsAny(host, "\r\n") {
		return errors.New("network: egress_host must not contain newlines")
	}
	if c.EgressPort == "" {
		return nil // 默认 80
	}
	if p, err := strconv.Atoi(c.EgressPort); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("network: egress_port %q must be a number in 1-65535", c.EgressPort)
	}
	return nil
}

// Manager 负责出口地址的增改与本地 JSON 持久化。
type Manager struct {
	mu       sync.RWMutex
	cfg      Config
	filePath string
}

// NewManager 从指定 JSON 文件加载出口配置;文件不存在视为空。
func NewManager(filePath string) (*Manager, error) {
	m := &Manager{filePath: filePath}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load 启动时读取本地 JSON 中的出口配置。
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("network: read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("network: parse config %s: %w", m.filePath, err)
	}
	m.cfg = cfg
	return nil
}

// save 将当前出口配置写回本地 JSON,临时文件 + 改名原子写。
func (m *Manager) save() error {
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".network-*.json")
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

// Get 返回当前出口配置。
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Set 更新出口配置并持久化;校验失败返回错误,持久化失败回滚内存态并返回 ErrPersist。
func (m *Manager) Set(cfg Config) error {
	cfg.EgressHost = strings.TrimSpace(cfg.EgressHost)
	cfg.EgressPort = strings.TrimSpace(cfg.EgressPort)
	if cfg.EgressPort == "" {
		cfg.EgressPort = "80"
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.cfg
	m.cfg = cfg
	if err := m.save(); err != nil {
		m.cfg = old // 回滚内存态
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}
