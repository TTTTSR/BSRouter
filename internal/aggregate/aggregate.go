// Package aggregate 管理聚合模型:模型名 N 的聚合 = 当前拥有模型名 N 的供应商
// 集合(自动派生,供应商增删自动跟随)− 用户剔除名单(JSON 持久化)。调用时按
// 轮询在成员供应商间负载均衡。合成 id 为 "{供应商}@{模型}",聚合为不含 @ 的裸模型名。
package aggregate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"BSRouter/internal/provider"
)

var (
	// ErrNotFound 表示模型名不存在(无供应商拥有该模型)。
	ErrNotFound = errors.New("aggregate: not found")
	// ErrPersist 表示配置写入本地 JSON 失败。
	ErrPersist = errors.New("aggregate: persist failed")
)

// cooldownDuration 故障转移失败供应商的冷却时长。仅存内存(重启即清除):
// 属运行时压力削减保护,不入 aggregates.json 持久化。
const cooldownDuration = 10 * time.Minute

// Config 是一条聚合模型的剔除配置(JSON 持久化形态)。聚合本身派生自供应商。
type Config struct {
	Name     string   `json:"name"`
	Excluded []string `json:"excluded,omitempty"`
}

// Model 是聚合模型的 API 响应形态。
type Model struct {
	Name      string   `json:"name"`
	Members   []string `json:"members"`   // 当前聚合的供应商(拥有该模型且未被剔除)
	Available []string `json:"available"` // 可添加回来的供应商(= 拥有该模型但被剔除)
}

// Manager 派生聚合模型并持久化剔除名单;另有运行时故障转移冷却(仅内存)。
type Manager struct {
	mu       sync.RWMutex
	excluded map[string]map[string]bool // 模型名 -> 剔除的供应商名
	cooldown map[string]map[string]time.Time // 模型名 -> 供应商名 -> 冷却截止(故障转移禁用)
	filePath string
	rr       map[string]int64 // 模型名 -> 轮询计数器
	pm       *provider.Manager
}

// NewManager 从指定 JSON 文件加载剔除名单;文件不存在视为空。
func NewManager(filePath string, pm *provider.Manager) (*Manager, error) {
	m := &Manager{
		excluded: make(map[string]map[string]bool),
		cooldown: make(map[string]map[string]time.Time),
		filePath: filePath,
		rr:       make(map[string]int64),
		pm:       pm,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load 启动时读取本地 JSON 中的剔除名单。
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("aggregate: read config: %w", err)
	}
	var cfgs []Config
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return fmt.Errorf("aggregate: parse config %s: %w", m.filePath, err)
	}
	for _, c := range cfgs {
		if c.Name == "" {
			return fmt.Errorf("aggregate: invalid entry in %s", m.filePath)
		}
		set := make(map[string]bool, len(c.Excluded))
		for _, p := range c.Excluded {
			set[p] = true
		}
		m.excluded[c.Name] = set
	}
	return nil
}

// save 将当前剔除名单写回本地 JSON,临时文件 + 改名原子写。
func (m *Manager) save() error {
	cfgs := make([]Config, 0, len(m.excluded))
	for name, set := range m.excluded {
		excl := make([]string, 0, len(set))
		for p := range set {
			excl = append(excl, p)
		}
		sort.Strings(excl)
		cfgs = append(cfgs, Config{Name: name, Excluded: excl})
	}
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Name < cfgs[j].Name })
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.filePath)
	tmp, err := os.CreateTemp(dir, ".aggregates-*.json")
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

// candidates 返回拥有模型名 name 的供应商(按供应商名排序)。
func (m *Manager) candidates(name string) []string {
	var out []string
	for _, c := range m.pm.List() {
		for _, md := range c.Models {
			if md.Name == name {
				out = append(out, c.Name)
				break
			}
		}
	}
	return out
}

// members 返回有效成员 = candidates − excluded。
func (m *Manager) members(name string) []string {
	excl := m.excluded[name]
	var out []string
	for _, p := range m.candidates(name) {
		if !excl[p] {
			out = append(out, p)
		}
	}
	return out
}

// Models 返回所有聚合模型(仅 ≥1 成员),按名称排序。
func (m *Manager) Models() []Model {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make(map[string]bool)
	for _, c := range m.pm.List() {
		for _, md := range c.Models {
			names[md.Name] = true
		}
	}
	out := make([]Model, 0, len(names))
	for name := range names {
		members := m.members(name)
		if len(members) == 0 {
			continue
		}
		avail := make([]string, 0, len(m.excluded[name]))
		for p := range m.excluded[name] {
			avail = append(avail, p)
		}
		sort.Strings(avail)
		out = append(out, Model{Name: name, Members: members, Available: avail})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TryOrder 返回聚合模型的故障转移尝试顺序(并发安全):从轮询位置旋转,跳过
// 冷却中(故障转移刚禁用)的成员。全部成员冷却时返回全量(故障开放,避免模型整体
// 不可用 10 分钟)。模型不是可调用聚合(无成员/含 @ 的合成 id)时返回 false,
// 交由供应商前缀解析处理。
func (m *Manager) TryOrder(name string) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members := m.members(name)
	if len(members) == 0 {
		return nil, false
	}
	// 旋转起点仍按全量成员推进,避免轮询位置停在被冷却的成员上。
	i := m.rr[name] % int64(len(members))
	m.rr[name]++
	ordered := make([]string, 0, len(members))
	for k := 0; k < len(members); k++ {
		ordered = append(ordered, members[(int(i)+k)%len(members)])
	}
	now := time.Now()
	active := make([]string, 0, len(ordered))
	for _, p := range ordered {
		if until, ok := m.cooldown[name][p]; ok && now.Before(until) {
			continue
		}
		active = append(active, p)
	}
	if len(active) == 0 {
		return ordered, true // 故障开放:全部冷却时照常尝试
	}
	return active, true
}

// Next 轮询选一个成员供应商(并发安全)。已冷却的成员会被跳过。模型不是可调用聚合
// (无成员/含 @ 的合成 id)时返回 false,交由供应商前缀解析处理。
func (m *Manager) Next(name string) (string, bool) {
	order, ok := m.TryOrder(name)
	if !ok || len(order) == 0 {
		return "", false
	}
	return order[0], true
}

// Ban 把模型 name 下的供应商加入冷却(禁用)cooldownDuration,仅当其当前为该聚合的
// 有效成员(非成员 no-op,天然处理成员被剔除/供应商已删的竞态)。冷却仅存内存、
// 不持久化:重启即清除。故障转移中失败成员在后续请求中不再被轮到,以削减请求压力。
func (m *Manager) Ban(name string, providers ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	valid := make(map[string]bool)
	for _, p := range m.members(name) {
		valid[p] = true
	}
	until := time.Now().Add(cooldownDuration)
	for _, p := range providers {
		if !valid[p] {
			continue
		}
		if m.cooldown[name] == nil {
			m.cooldown[name] = make(map[string]time.Time)
		}
		m.cooldown[name][p] = until
	}
}

// SetMembers 设置聚合模型的成员(校验 ⊆ candidates),持久化 excluded = candidates − members。
func (m *Manager) SetMembers(name string, members []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cands := m.candidates(name)
	if len(cands) == 0 {
		return fmt.Errorf("%w: no model %q", ErrNotFound, name)
	}
	inCands := make(map[string]bool, len(cands))
	for _, p := range cands {
		inCands[p] = true
	}
	for _, p := range members {
		if !inCands[p] {
			return fmt.Errorf("aggregate: member %q does not have model %q", p, name)
		}
	}
	newExcl := make(map[string]bool)
	for _, p := range cands {
		if !containsString(members, p) {
			newExcl[p] = true
		}
	}
	m.excluded[name] = newExcl
	if err := m.save(); err != nil {
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
