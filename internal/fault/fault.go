// Package fault 记录网关运行中的故障(上游特定错误如余额不足/限流、内部错误、上游错误),
// 供管理界面陈列展示并逐条删除。分两种捕捉模式(由启动参数指定,前端不提供切换):
//   - user(用户模式):只记录可阻塞的特定故障(余额不足 402、限流 429,状态码可按供应商自定义);
//   - dev(开发模式):记录所有错误(内部错误与上游错误)。
//
// 故障以 JSON 持久化到本地文件(默认配置目录 faults.json),新条目在前,超出上限丢弃最旧。
// 阻塞语义:
//   - insufficient_balance(余额不足):阻塞直到用户手动删除故障;
//   - rate_limited(限流):阻塞 rateLimitBlockDuration(默认 2 小时)后自动解除,也可手动删除提前解除。
package fault

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Mode 是故障捕捉模式。
type Mode string

const (
	// ModeUser 仅记录可阻塞的特定故障(余额不足/限流)。
	ModeUser Mode = "user"
	// ModeDev 记录所有错误(内部错误与上游错误)。
	ModeDev Mode = "dev"
)

// 故障分类(category)取值。
const (
	// CategoryInsufficientBalance 余额不足:阻塞直到手动解除。
	CategoryInsufficientBalance = "insufficient_balance"
	// CategoryRateLimited 限流/频率限制:阻塞 rateLimitBlockDuration 后自动解除。
	CategoryRateLimited = "rate_limited"
	// CategoryUpstream 上游错误(dev 模式)。
	CategoryUpstream = "upstream"
	// CategoryInternal 网关内部错误(dev 模式)。
	CategoryInternal = "internal"
)

// 上游 HTTP 状态码 → 阻塞分类的默认映射(可按供应商经 RateLimitStatus /
// InsufficientBalanceStatus 覆盖;覆盖值为 0 表示禁用该分类的阻塞)。
const (
	DefaultInsufficientBalanceStatus = 402
	DefaultRateLimitStatus           = 429
)

// rateLimitBlockDuration 限流故障的默认自动解除时长(供应商可经 RateLimitDurationMinutes 覆盖)。
const rateLimitBlockDuration = 2 * time.Hour

// ErrNotFound 表示故障 ID 不存在。
var ErrNotFound = errors.New("fault: not found")

// BlockedError 表示供应商因特定故障(如余额不足)被禁用。经 writeRouteError 映射为 503,
// 单独请求该供应商的模型时作为阻塞原因返回给客户端。
type BlockedError struct {
	Provider string // 被禁用的供应商名;聚合全部成员被禁用时为空
	Reason   string // 阻塞原因(对外展示)
}

// Error 实现 error 接口。Provider 为空时表示聚合全部成员被禁用,直接返回原因。
func (e *BlockedError) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("provider %q is blocked: %s", e.Provider, e.Reason)
	}
	if e.Reason != "" {
		return e.Reason
	}
	return "blocked"
}

// blockingCategories 是触发供应商禁用的故障分类,映射到对外展示的阻塞原因。
// 新增可阻塞的特定故障时,在此追加条目即可(其 category 须由 classify 产生)。
var blockingCategories = map[string]string{
	CategoryInsufficientBalance: "insufficient balance (余额不足)",
	CategoryRateLimited:         "rate limited (限流)",
}

// maxFaults 限制持久化的故障条数(超出丢弃最旧),避免文件无限增长。
const maxFaults = 500

// maxMessage 限制单条故障内容长度(按 rune 边界截断),避免超大错误体撑爆故障文件。
const maxMessage = 4096

// Fault 是一条已记录的故障。
type Fault struct {
	ID             string `json:"id"`
	Timestamp      string `json:"timestamp"`                 // RFC3339Nano,故障发生时间
	Category       string `json:"category"`                  // insufficient_balance / rate_limited / upstream / internal
	Message        string `json:"message"`                   // 故障内容(已抹除密钥、截断)
	Model          string `json:"model,omitempty"`           // 出错的模型
	Provider       string `json:"provider,omitempty"`        // 出错的供应商
	Status         int    `json:"status,omitempty"`          // 网关回给客户端的 HTTP 状态
	UpstreamStatus int    `json:"upstream_status,omitempty"` // 上游 HTTP 状态(0=非上游)
	ExpiresAt      string `json:"expires_at,omitempty"`      // RFC3339Nano 自动解除时间;空 = 持久阻塞(需手动删除解除)
}

// Input 是网关对一次候选错误的描述,由 Record 判定是否值得记录及分类。
type Input struct {
	Error          string // 错误信息(已抹除密钥、截断);为空时按 Status 合成
	Status         int    // 网关回给客户端的 HTTP 状态
	UpstreamStatus int    // 上游 HTTP 状态
	Upstream       bool   // 错误是否归因于上游供应商(false = 网关内部错误)
	Model          string
	Provider       string
	// 该供应商自定义的阻塞配置(缺失/零值 = 用默认或禁用,见 BlockConfig)。
	RateLimitStatus          *int  // 限流错误码(nil=429;0=禁用;N=自定义)
	RateLimitEnabled         *bool // 限流阻塞开关(nil=启用;false=禁用)
	RateLimitDurationMinutes int   // 限流阻塞时长(分钟;0=默认 120)
	InsufficientBalanceStatus *int // 余额不足错误码(nil=402;0=禁用;N=自定义)
}

// BlockConfig 是分类时使用的阻塞配置(来自供应商自定义,缺失项用默认)。
type BlockConfig struct {
	InsufficientStatus *int  // nil=402;0=禁用;N=自定义
	RateLimitStatus    *int  // nil=429;0=禁用;N=自定义
	RateLimitEnabled   *bool // nil=启用;false=禁用
}

// knownFault 描述一个硬编码的特定故障:category + 命中关键字(大小写不敏感子串)。
type knownFault struct {
	category string
	keywords []string
}

// knownFaults 是硬编码的特定故障列表(文本匹配兜底,兼容非标准状态码的上游;
// 用户模式下与状态码匹配并列,都会记录)。新增特定故障在此追加一个条目即可。
var knownFaults = []knownFault{
	{
		category: CategoryInsufficientBalance,
		keywords: []string{
			"insufficient balance",
			"insufficient_balance",
			"insufficient funds",
			"insufficient_credit",
			"insufficient credits",
			"insufficient quota",
			"insufficient_quota",
			"exceeded your current quota",
			"out of credit",
			"not enough balance",
			"not enough credit",
			"balance is insufficient",
			"余额不足",
			"额度不足",
			"余额耗尽",
			"欠费",
		},
	},
}

// classify 判定一条错误是否值得记录及分类,返回 (category, 是否记录)。判定顺序:
//  1. 上游状态码命中该供应商自定义(或默认)的余额不足/限流错误码 → 对应可阻塞分类
//     (限流仅在 RateLimitEnabled 时参与匹配);
//  2. 文本匹配硬编码特定故障(余额不足关键字,兼容非标准状态码的上游);
//  3. 未命中时仅 dev 模式记录,按 upstream 归为上游错误/内部错误。
// cfg 为该供应商自定义阻塞配置(缺失项用默认,见 effectiveBlockStatus / rateLimitEnabled)。
func classify(msg string, upstreamStatus int, upstream bool, mode Mode, cfg BlockConfig) (string, bool) {
	if st, ok := effectiveBlockStatus(cfg.InsufficientStatus, DefaultInsufficientBalanceStatus); ok && upstreamStatus == st {
		return CategoryInsufficientBalance, true
	}
	if rateLimitEnabled(cfg.RateLimitEnabled) {
		if st, ok := effectiveBlockStatus(cfg.RateLimitStatus, DefaultRateLimitStatus); ok && upstreamStatus == st {
			return CategoryRateLimited, true
		}
	}
	if cat, ok := matchKnownFault(msg); ok {
		return cat, true
	}
	if mode != ModeDev {
		return "", false
	}
	if upstream {
		return CategoryUpstream, true
	}
	return CategoryInternal, true
}

// rateLimitEnabled 判断限流阻塞是否启用:nil 视为启用(默认),false 为禁用。
func rateLimitEnabled(enabled *bool) bool {
	return enabled == nil || *enabled
}

// effectiveBlockStatus 返回生效的错误码:custom 为 nil 时用默认 def;*0 表示禁用(返回
// ok=false);*N 表示该供应商自定义的 N。仅对 4xx/5xx 生效(校验已保证)。
func effectiveBlockStatus(custom *int, def int) (int, bool) {
	if custom == nil {
		return def, true
	}
	if *custom == 0 {
		return 0, false
	}
	return *custom, true
}

// matchKnownFault 在错误信息中匹配硬编码的特定故障(大小写不敏感子串)。
func matchKnownFault(msg string) (string, bool) {
	if msg == "" {
		return "", false
	}
	lower := strings.ToLower(msg)
	for _, kf := range knownFaults {
		for _, kw := range kf.keywords {
			if strings.Contains(lower, kw) {
				return kf.category, true
			}
		}
	}
	return "", false
}

// Manager 记录故障并持久化到本地 JSON(新条目在前)。
type Manager struct {
	mu       sync.RWMutex
	mode     Mode
	faults   []Fault // 最新在前
	filePath string
}

// NewManager 从指定 JSON 文件加载故障;文件不存在视为空。mode 为用户/开发模式。
func NewManager(filePath string, mode Mode) (*Manager, error) {
	m := &Manager{mode: mode, filePath: filePath}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load 启动时读取本地 JSON 中的故障(按时间倒序排、截断到上限)。
func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fault: read config: %w", err)
	}
	var faults []Fault
	if err := json.Unmarshal(data, &faults); err != nil {
		return fmt.Errorf("fault: parse config %s: %w", m.filePath, err)
	}
	sortFaultsDesc(faults)
	m.faults = trimFaults(faults)
	return nil
}

// save 将当前故障写回本地 JSON,临时文件 + 改名原子写。
func (m *Manager) save() error {
	data, err := json.MarshalIndent(m.faults, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".faults-*.json")
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

// Record 判定并记录一条故障:命中可阻塞故障(与模式无关)或 dev 模式下记录全部错误。
// 判定不记录时为 no-op;持久化失败非致命(内存态仍保留,本次运行内可见)。
// 可阻塞故障按 (provider, category) 去重:已存在有效阻塞时刷新该条(时间/内容/自动解除
// 时间)而非新增,避免持续 429 等把故障列表刷屏;非阻塞故障(dev 模式)逐条记录。
func (m *Manager) Record(in Input) {
	cfg := BlockConfig{
		InsufficientStatus: in.InsufficientBalanceStatus,
		RateLimitStatus:    in.RateLimitStatus,
		RateLimitEnabled:   in.RateLimitEnabled,
	}
	category, ok := classify(in.Error, in.UpstreamStatus, in.Upstream, m.mode, cfg)
	if !ok {
		return
	}
	msg := truncateRunes(in.Error, maxMessage)
	if msg == "" {
		msg = fmt.Sprintf("gateway error (status %d)", in.Status)
	}
	now := time.Now()
	f := Fault{
		ID:             newID(),
		Timestamp:      now.Format(time.RFC3339Nano),
		Category:       category,
		Message:        msg,
		Model:          in.Model,
		Provider:       in.Provider,
		Status:         in.Status,
		UpstreamStatus: in.UpstreamStatus,
	}
	if category == CategoryRateLimited {
		// 限流自动解除时长:供应商可经 RateLimitDurationMinutes(分钟)自定义,0 用默认 2 小时。
		dur := time.Duration(in.RateLimitDurationMinutes) * time.Minute
		if dur <= 0 {
			dur = rateLimitBlockDuration
		}
		f.ExpiresAt = now.Add(dur).Format(time.RFC3339Nano)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeExpiredLocked()
	if _, blocking := blockingCategories[category]; blocking && in.Provider != "" {
		for i := range m.faults {
			if m.faults[i].Provider == in.Provider && m.faults[i].Category == category {
				m.faults = append(m.faults[:i], m.faults[i+1:]...)
				m.faults = append([]Fault{f}, m.faults...)
				m.faults = trimFaults(m.faults)
				_ = m.save() // 非致命:失败时内存态仍可见
				return
			}
		}
	}
	m.faults = append([]Fault{f}, m.faults...)
	m.faults = trimFaults(m.faults)
	_ = m.save() // 非致命:失败时内存态仍可见
}

// List 返回全部故障(最新在前)的副本;已过期的可自动解除故障(如限流 2h)会被清理。
func (m *Manager) List() []Fault {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeExpiredLocked()
	out := make([]Fault, len(m.faults))
	copy(out, m.faults)
	return out
}

// Delete 删除指定 ID 的故障;不存在时返回 ErrNotFound,持久化失败回滚内存态并返回错误。
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i, f := range m.faults {
		if f.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	old := make([]Fault, len(m.faults))
	copy(old, m.faults)
	m.faults = append(m.faults[:idx], m.faults[idx+1:]...)
	if err := m.save(); err != nil {
		m.faults = old // 回滚内存态
		return fmt.Errorf("fault: persist failed: %w", err)
	}
	return nil
}

// Mode 返回当前故障捕捉模式。
func (m *Manager) Mode() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

// Block 返回供应商 name 当前是否被故障禁用及其原因:存在任一有效(未过期)的
// blockingCategories 故障且 Provider 为 name 即禁用。用户删除该供应商的这类故障
// (表示已处理)后 Block 返回 false,恢复正常;限流故障到期自动解除。
func (m *Manager) Block(name string) (reason string, blocked bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeExpiredLocked()
	for _, f := range m.faults {
		if f.Provider != name {
			continue
		}
		if reason, ok := blockingCategories[f.Category]; ok {
			return reason, true
		}
	}
	return "", false
}

// Count 返回当前故障条数(自动清理已过期项)。
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeExpiredLocked()
	return len(m.faults)
}

// purgeExpiredLocked 移除已过期的可自动解除故障(如限流 2h 后),并把清理结果持久化。
// 需在持有 m.mu.Lock 时调用;持久化失败非致命(内存态已清理,下次保存时落盘)。
func (m *Manager) purgeExpiredLocked() {
	now := time.Now()
	kept := m.faults[:0]
	changed := false
	for _, f := range m.faults {
		if f.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, f.ExpiresAt); err == nil && !now.Before(t) {
				changed = true
				continue
			}
		}
		kept = append(kept, f)
	}
	if changed {
		m.faults = kept
		_ = m.save()
	}
}

// newID 生成 16 位十六进制随机 ID(8 字节)。
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// sortFaultsDesc 按时间戳降序(最新在前)。所有时间戳均由同一 RFC3339Nano 格式生成,
// 字典序即时间序。
func sortFaultsDesc(f []Fault) {
	sort.Slice(f, func(i, j int) bool { return f[i].Timestamp > f[j].Timestamp })
}

// trimFaults 截断到 maxFaults(丢弃最旧的尾部;slice 已最新在前)。
func trimFaults(f []Fault) []Fault {
	if len(f) > maxFaults {
		return f[:maxFaults]
	}
	return f
}

// truncateRunes 按 rune 边界截断字符串至 maxBytes 字节,避免切断多字节字符。
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-size]
	}
	return s + "…"
}
