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

// Config 是一条聚合模型的剔除/优先级/负载均衡配置(JSON 持久化形态)。聚合本身派生自供应商。
type Config struct {
	Name        string   `json:"name"`
	Excluded    []string `json:"excluded,omitempty"`
	Order       []string `json:"order,omitempty"`       // 成员优先级顺序(故障转移/负载均衡按此流转)
	LoadBalance *bool    `json:"load_balance,omitempty"` // 负载均衡开关;nil 视为关闭(默认关)
}

// Model 是聚合模型的 API 响应形态。
type Model struct {
	Name        string   `json:"name"`
	Members     []string `json:"members"`     // 当前聚合的供应商(拥有该模型且未被剔除,按优先级排序)
	Available   []string `json:"available"`   // 可添加回来的供应商(= 拥有该模型但被剔除)
	LoadBalance bool     `json:"load_balance"` // 该聚合是否启用轮询负载均衡
}

// Manager 派生聚合模型并持久化剔除名单/优先级/负载均衡开关;另有运行时故障转移冷却(仅内存)。
type Manager struct {
	mu          sync.RWMutex
	excluded    map[string]map[string]bool // 模型名 -> 剔除的供应商名
	order       map[string][]string        // 模型名 -> 成员优先级顺序(用户配置)
	loadBalance map[string]bool            // 模型名 -> 是否启用轮询负载均衡
	cooldown    map[string]map[string]time.Time // 模型名 -> 供应商名 -> 冷却截止(故障转移禁用)
	filePath    string
	rr          map[string]int64 // 模型名 -> 轮询计数器
	pm          *provider.Manager
}

// NewManager 从指定 JSON 文件加载剔除名单;文件不存在视为空。
func NewManager(filePath string, pm *provider.Manager) (*Manager, error) {
	m := &Manager{
		excluded:    make(map[string]map[string]bool),
		order:       make(map[string][]string),
		loadBalance: make(map[string]bool),
		cooldown:    make(map[string]map[string]time.Time),
		filePath:    filePath,
		rr:          make(map[string]int64),
		pm:          pm,
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
		if len(c.Order) > 0 {
			m.order[c.Name] = c.Order
		}
		if c.LoadBalance != nil {
			m.loadBalance[c.Name] = *c.LoadBalance
		}
	}
	return nil
}

// save 将当前剔除名单/优先级/负载均衡开关写回本地 JSON,临时文件 + 改名原子写。
// 写回所有有持久化配置的模型(剔除名单 / 优先级 / 负载均衡任一非空),避免仅设置
// 负载均衡开关而未剔除成员时配置丢失。
func (m *Manager) save() error {
	names := make(map[string]bool, len(m.excluded)+len(m.order)+len(m.loadBalance))
	for name := range m.excluded {
		names[name] = true
	}
	for name := range m.order {
		names[name] = true
	}
	for name := range m.loadBalance {
		names[name] = true
	}
	cfgs := make([]Config, 0, len(names))
	for name := range names {
		excl := make([]string, 0, len(m.excluded[name]))
		for p := range m.excluded[name] {
			excl = append(excl, p)
		}
		sort.Strings(excl)
		c := Config{Name: name, Excluded: excl}
		if order, ok := m.order[name]; ok && len(order) > 0 {
			c.Order = order
		}
		if lb, ok := m.loadBalance[name]; ok {
			c.LoadBalance = &lb // false 也显式写出,确保下次加载语义一致
		}
		cfgs = append(cfgs, c)
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

// members 返回有效成员 = candidates − excluded,按用户配置的优先级顺序排序:
// 先按 m.order[name] 顺序输出(过滤无效/去重),order 未覆盖的剩余成员按字母序追加;
// order 为空时纯字母序(候选天然有序,兼容旧行为)。
func (m *Manager) members(name string) []string {
	excl := m.excluded[name]
	cands := m.candidates(name)
	remaining := make(map[string]bool, len(cands))
	for _, p := range cands {
		if !excl[p] {
			remaining[p] = true
		}
	}
	out := make([]string, 0, len(remaining))
	for _, p := range m.order[name] {
		if remaining[p] {
			out = append(out, p)
			delete(remaining, p)
		}
	}
	for _, p := range cands { // 候选天然字母序
		if remaining[p] {
			out = append(out, p)
		}
	}
	return out
}

// Members 返回聚合模型的有效成员(按优先级顺序,只读、无副作用:不推进轮询、
// 不跳过冷却)。非聚合/无成员时返回 false。供直通判定等需要"全量成员"的场景。
func (m *Manager) Members(name string) ([]string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	members := m.members(name)
	if len(members) == 0 {
		return nil, false
	}
	return members, true
}

// LoadBalanceOf 返回聚合模型是否启用负载均衡(只读)。模型不存在时返回 false。
func (m *Manager) LoadBalanceOf(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.loadBalance[name]
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
		out = append(out, Model{Name: name, Members: members, Available: avail, LoadBalance: m.loadBalance[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TryOrder 返回聚合模型的故障转移尝试顺序(并发安全)。负载均衡开启时从轮询位置旋转
// (起点每请求 +1,分散请求到不同渠道);关闭时起点固定 0,按用户配置的优先级顺序稳定
// 流转(默认行为,不轮询)。跳过冷却中(故障转移刚禁用)的成员;全部成员冷却时返回全量
// (故障开放,避免模型整体不可用 10 分钟)。模型不是可调用聚合(无成员/含 @ 的合成 id)
// 时返回 false,交由供应商前缀解析处理。
func (m *Manager) TryOrder(name string) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members := m.members(name)
	if len(members) == 0 {
		return nil, false
	}
	i := int64(0)
	if m.loadBalance[name] {
		// 旋转起点仍按全量成员推进,避免轮询位置停在被冷却的成员上。
		i = m.rr[name] % int64(len(members))
		m.rr[name]++
	}
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

// SetMembers 设置聚合模型的成员(校验 ⊆ candidates),持久化 excluded = candidates − members
// 与成员优先级顺序 order(传入 members 的顺序即故障转移/负载均衡的流转顺序)。
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
	seen := make(map[string]bool, len(members))
	for _, p := range members {
		if !inCands[p] {
			return fmt.Errorf("aggregate: member %q does not have model %q", p, name)
		}
		if seen[p] {
			return fmt.Errorf("aggregate: duplicate member %q", p)
		}
		seen[p] = true
	}
	newExcl := make(map[string]bool)
	for _, p := range cands {
		if !containsString(members, p) {
			newExcl[p] = true
		}
	}
	m.excluded[name] = newExcl
	m.order[name] = members // 保序:成员顺序即渠道优先级
	if err := m.save(); err != nil {
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

// SetLoadBalance 设置聚合模型的负载均衡开关并持久化。模型不存在时返回 ErrNotFound。
func (m *Manager) SetLoadBalance(name string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.excluded[name]; !ok && len(m.candidates(name)) == 0 {
		return fmt.Errorf("%w: no model %q", ErrNotFound, name)
	}
	m.loadBalance[name] = enabled
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
