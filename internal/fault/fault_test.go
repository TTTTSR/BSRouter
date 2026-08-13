package fault

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyKnownFault(t *testing.T) {
	cases := []string{
		"upstream returned 402: {\"error\":{\"message\":\"Insufficient Balance\"}}",
		"insufficient_quota: You exceeded your current quota",
		"余额不足,请充值",
		"provider error: insufficient funds",
	}
	for _, msg := range cases {
		// 命中特定故障(文本匹配,状态码 0 不干扰):无论 user/dev 模式都记录。
		if cat, ok := classify(msg, 0, true, ModeUser, BlockConfig{}); !ok || cat != CategoryInsufficientBalance {
			t.Errorf("classify(%q, user) = (%q,%v), want insufficient_balance", msg, cat, ok)
		}
		if cat, ok := classify(msg, 0, true, ModeDev, BlockConfig{}); !ok || cat != CategoryInsufficientBalance {
			t.Errorf("classify(%q, dev) = (%q,%v), want insufficient_balance", msg, cat, ok)
		}
	}
}

func TestClassifyByStatus(t *testing.T) {
	// 402 → 余额不足(默认),429 → 限流(默认),与模式无关。
	if cat, ok := classify("upstream returned 402", 402, true, ModeUser, BlockConfig{}); !ok || cat != CategoryInsufficientBalance {
		t.Errorf("classify(402) = (%q,%v), want insufficient_balance", cat, ok)
	}
	if cat, ok := classify("upstream returned 429", 429, true, ModeUser, BlockConfig{}); !ok || cat != CategoryRateLimited {
		t.Errorf("classify(429) = (%q,%v), want rate_limited", cat, ok)
	}
	// 状态码匹配优先于文本:429 状态即使带余额不足文本也归为限流。
	if cat, ok := classify("Insufficient Balance", 429, true, ModeUser, BlockConfig{}); !ok || cat != CategoryRateLimited {
		t.Errorf("classify(429+text) = (%q,%v), want rate_limited", cat, ok)
	}
	// 自定义错误码:该供应商把 429 当作余额不足。
	ins := 429
	if cat, ok := classify("x", 429, true, ModeUser, BlockConfig{InsufficientStatus: &ins}); !ok || cat != CategoryInsufficientBalance {
		t.Errorf("classify(429, ins=429) = (%q,%v), want insufficient_balance", cat, ok)
	}
	// 自定义错误码:该供应商把 403 当作限流(与余额不足默认 402 不冲突)。
	rl := 403
	if cat, ok := classify("x", 403, true, ModeUser, BlockConfig{RateLimitStatus: &rl}); !ok || cat != CategoryRateLimited {
		t.Errorf("classify(403, rl=403) = (%q,%v), want rate_limited", cat, ok)
	}
	// 禁用限流阻塞(错误码 0):429 不再归为限流,文本不匹配 → 用户模式不记录。
	disabled := 0
	if cat, ok := classify("x", 429, true, ModeUser, BlockConfig{RateLimitStatus: &disabled}); ok {
		t.Errorf("classify(429, rl=0) = (%q,%v), want not recorded", cat, ok)
	}
	// 显式关闭限流阻塞(开关 false):即使状态码匹配也不归为限流。
	off := false
	if cat, ok := classify("x", 429, true, ModeUser, BlockConfig{RateLimitEnabled: &off}); ok {
		t.Errorf("classify(429, enabled=false) = (%q,%v), want not recorded", cat, ok)
	}
	// 显式启用限流阻塞(开关 true,与 nil 等价)。
	on := true
	if cat, ok := classify("x", 429, true, ModeUser, BlockConfig{RateLimitEnabled: &on}); !ok || cat != CategoryRateLimited {
		t.Errorf("classify(429, enabled=true) = (%q,%v), want rate_limited", cat, ok)
	}
}

func TestClassifyModeGating(t *testing.T) {
	// 非特定故障的普通上游错误:用户模式不记录,dev 模式记录为 upstream。
	if cat, ok := classify("upstream returned 500", 500, true, ModeUser, BlockConfig{}); ok {
		t.Errorf("classify(user) = (%q,%v), want not recorded", cat, ok)
	}
	if cat, ok := classify("upstream returned 500", 500, true, ModeDev, BlockConfig{}); !ok || cat != CategoryUpstream {
		t.Errorf("classify(dev) = (%q,%v), want upstream", cat, ok)
	}
	// 内部错误:dev 模式记录为 internal,用户模式不记录。
	if cat, ok := classify("decode request: bad json", 0, false, ModeUser, BlockConfig{}); ok {
		t.Errorf("classify(user, internal) = (%q,%v), want not recorded", cat, ok)
	}
	if cat, ok := classify("decode request: bad json", 0, false, ModeDev, BlockConfig{}); !ok || cat != CategoryInternal {
		t.Errorf("classify(dev, internal) = (%q,%v), want internal", cat, ok)
	}
}

func TestManagerRecordListDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeUser)
	if err != nil {
		t.Fatal(err)
	}
	// 用户模式:普通错误不记录。
	m.Record(Input{Error: "upstream returned 500", Status: 502, UpstreamStatus: 500, Upstream: true})
	if m.Count() != 0 {
		t.Fatalf("user mode recorded non-known fault: count=%d", m.Count())
	}
	// 用户模式:余额不足记录。
	m.Record(Input{Error: "upstream returned 402: Insufficient Balance", Status: 502, UpstreamStatus: 402, Upstream: true, Model: "a@m", Provider: "a"})
	if m.Count() != 1 {
		t.Fatalf("count = %d, want 1", m.Count())
	}
	faults := m.List()
	if len(faults) != 1 {
		t.Fatalf("list len = %d, want 1", len(faults))
	}
	f := faults[0]
	if f.Category != CategoryInsufficientBalance || f.Model != "a@m" || f.Provider != "a" {
		t.Errorf("fault = %+v", f)
	}
	if f.ID == "" || f.Timestamp == "" {
		t.Errorf("fault missing id/timestamp: %+v", f)
	}
	// 删除。
	if err := m.Delete(f.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("count after delete = %d, want 0", m.Count())
	}
	// 再次删除应 ErrNotFound。
	if err := m.Delete(f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing = %v, want ErrNotFound", err)
	}
}

func TestManagerDevModeRecordsAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeDev)
	if err != nil {
		t.Fatal(err)
	}
	m.Record(Input{Error: "upstream returned 500", Status: 502, UpstreamStatus: 500, Upstream: true})
	m.Record(Input{Error: "decode request", Status: 400})
	if m.Count() != 2 {
		t.Fatalf("dev count = %d, want 2", m.Count())
	}
	// 最新在前。
	list := m.List()
	if list[0].Category != CategoryInternal {
		t.Errorf("first category = %q, want internal", list[0].Category)
	}
	if list[1].Category != CategoryUpstream {
		t.Errorf("second category = %q, want upstream", list[1].Category)
	}
}

func TestManagerPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeDev)
	if err != nil {
		t.Fatal(err)
	}
	m.Record(Input{Error: "余额不足", Status: 502, UpstreamStatus: 402, Upstream: true, Model: "x"})
	id := m.List()[0].ID

	// 重新加载:故障应从磁盘读回。
	m2, err := NewManager(path, ModeDev)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Count() != 1 {
		t.Fatalf("reload count = %d, want 1", m2.Count())
	}
	got := m2.List()[0]
	if got.ID != id || got.Category != CategoryInsufficientBalance || got.Model != "x" {
		t.Errorf("reloaded fault = %+v", got)
	}
}

func TestManagerCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeDev)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxFaults+20; i++ {
		m.Record(Input{Error: "e", Status: 500})
	}
	if m.Count() != maxFaults {
		t.Fatalf("count = %d, want cap %d", m.Count(), maxFaults)
	}
}

func TestBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeDev)
	if err != nil {
		t.Fatal(err)
	}

	// 余额不足故障触发供应商阻塞。
	m.Record(Input{Error: "Insufficient Balance", Status: 502, UpstreamStatus: 402, Upstream: true, Provider: "oa"})
	if reason, blocked := m.Block("oa"); !blocked || reason == "" {
		t.Fatalf("Block(oa) = %q/%v, want blocked", reason, blocked)
	}
	// 其他供应商不受影响。
	if _, blocked := m.Block("ob"); blocked {
		t.Errorf("Block(ob) should be false")
	}

	// dev 模式的普通上游错误(非可阻塞分类)不触发阻塞。
	m.Record(Input{Error: "upstream 500", Status: 502, UpstreamStatus: 500, Upstream: true, Provider: "ob"})
	if _, blocked := m.Block("ob"); blocked {
		t.Errorf("Block(ob) with non-blocking category should be false")
	}

	// 删除余额不足故障后解除阻塞。
	for _, f := range m.List() {
		if f.Provider == "oa" {
			if err := m.Delete(f.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, blocked := m.Block("oa"); blocked {
		t.Errorf("Block(oa) should be false after deleting the fault")
	}
}

func TestRateLimitedBlockAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeUser)
	if err != nil {
		t.Fatal(err)
	}
	// 429 → 限流故障,带自动解除时间,并阻塞。
	m.Record(Input{Error: "upstream returned 429", Status: 502, UpstreamStatus: 429, Upstream: true, Provider: "oa"})
	f := m.List()[0]
	if f.Category != CategoryRateLimited {
		t.Fatalf("category = %q, want rate_limited", f.Category)
	}
	if f.ExpiresAt == "" {
		t.Fatal("rate_limited fault should have expires_at")
	}
	if _, blocked := m.Block("oa"); !blocked {
		t.Fatal("oa should be blocked by rate limit")
	}
	// 过期后自动解除并清理。
	m.faults[0].ExpiresAt = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
	if _, blocked := m.Block("oa"); blocked {
		t.Error("oa should be unblocked after expiry")
	}
	if m.Count() != 0 {
		t.Errorf("expired fault should be purged, count=%d", m.Count())
	}
	// 再次 429 重新阻塞。
	m.Record(Input{Error: "429 again", Status: 502, UpstreamStatus: 429, Upstream: true, Provider: "oa"})
	if _, blocked := m.Block("oa"); !blocked {
		t.Error("oa should be blocked again after a fresh 429")
	}
}

func TestRateLimitedManualDeleteUnblocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeUser)
	if err != nil {
		t.Fatal(err)
	}
	m.Record(Input{Error: "429", Status: 502, UpstreamStatus: 429, Upstream: true, Provider: "oa"})
	if _, blocked := m.Block("oa"); !blocked {
		t.Fatal("oa should be blocked")
	}
	f := m.List()[0]
	if err := m.Delete(f.ID); err != nil {
		t.Fatal(err)
	}
	if _, blocked := m.Block("oa"); blocked {
		t.Error("oa should be unblocked after manual delete")
	}
}

func TestRateLimitedCustomDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeUser)
	if err != nil {
		t.Fatal(err)
	}
	m.Record(Input{Error: "429", Status: 502, UpstreamStatus: 429, Upstream: true, Provider: "oa", RateLimitDurationMinutes: 5})
	f := m.List()[0]
	if f.ExpiresAt == "" {
		t.Fatal("rate_limited fault should have expires_at")
	}
	exp, err := time.Parse(time.RFC3339Nano, f.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	diff := exp.Sub(time.Now())
	if diff < 4*time.Minute || diff > 6*time.Minute {
		t.Errorf("expires_at diff = %v, want ~5 minutes", diff)
	}
}

func TestBlockingFaultDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults.json")
	m, err := NewManager(path, ModeUser)
	if err != nil {
		t.Fatal(err)
	}
	// 同一供应商持续 429:去重为一条,并刷新内容(自动解除时间随之顺延)。
	m.Record(Input{Error: "429 #1", Status: 502, UpstreamStatus: 429, Upstream: true, Provider: "oa"})
	m.Record(Input{Error: "429 #2", Status: 502, UpstreamStatus: 429, Upstream: true, Provider: "oa"})
	if m.Count() != 1 {
		t.Fatalf("dedup failed, count=%d", m.Count())
	}
	if got := m.List()[0].Message; !strings.Contains(got, "429 #2") {
		t.Errorf("message = %q, want refreshed to #2", got)
	}
	// 不同供应商不互相去重。
	m.Record(Input{Error: "429", Status: 502, UpstreamStatus: 429, Upstream: true, Provider: "ob"})
	if m.Count() != 2 {
		t.Errorf("count = %d, want 2", m.Count())
	}
}

func TestTruncateRunes(t *testing.T) {
	// 多字节字符截断不应产生半字符:第 7 字节落在「世」的中间,应整体回退到「好」。
	s := truncateRunes("你好世界", 7)
	if s != "你好…" {
		t.Errorf("truncateRunes = %q", s)
	}
	if len(truncateRunes("abc", 10)) != 3 {
		t.Errorf("short string should be unchanged")
	}
}
