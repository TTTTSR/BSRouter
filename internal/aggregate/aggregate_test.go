package aggregate

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"BSRouter/internal/provider"
)

// newMgr 构造供应商管理器(含 openai/azure/deepseek 及同名模型 gpt-4o)与聚合管理器。
func newMgr(t *testing.T) (*Manager, *provider.Manager) {
	t.Helper()
	pm, err := provider.NewManager(filepath.Join(t.TempDir(), "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string, models ...string) {
		t.Helper()
		cfgs := make([]provider.ModelConfig, 0, len(models))
		for _, m := range models {
			cfgs = append(cfgs, provider.ModelConfig{Name: m})
		}
		if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: name, BaseURL: "http://x", Models: cfgs}); err != nil {
			t.Fatal(err)
		}
	}
	add("openai", "gpt-4o", "gpt-5")
	add("azure", "gpt-4o")
	add("deepseek", "deepseek-chat")

	am, err := NewManager(filepath.Join(t.TempDir(), "aggregates.json"), pm)
	if err != nil {
		t.Fatal(err)
	}
	return am, pm
}

func TestModelsDerivation(t *testing.T) {
	am, _ := newMgr(t)
	models := am.Models()
	// gpt-4o(openai,azure)、gpt-5(openai)、deepseek-chat(deepseek) 各是一个聚合。
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	byName := map[string]Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	g := byName["gpt-4o"]
	if len(g.Members) != 2 || g.Members[0] != "azure" || g.Members[1] != "openai" {
		t.Errorf("gpt-4o members = %v, want [azure openai]", g.Members)
	}
	if len(g.Available) != 0 {
		t.Errorf("gpt-4o available = %v, want empty", g.Available)
	}
	// 单一供应商模型也是聚合。
	if d := byName["deepseek-chat"]; len(d.Members) != 1 || d.Members[0] != "deepseek" {
		t.Errorf("deepseek-chat = %+v", d)
	}
}

func TestNextRoundRobin(t *testing.T) {
	am, _ := newMgr(t)
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		m, ok := am.Next("gpt-4o")
		if !ok {
			t.Fatal("Next should be ok")
		}
		seen[m]++
	}
	if seen["openai"] != 2 || seen["azure"] != 2 {
		t.Errorf("round-robin distribution = %v, want openai=2 azure=2", seen)
	}
}

func TestNextCompositeNotAggregate(t *testing.T) {
	am, _ := newMgr(t)
	// 含 @ 的合成 id 不是聚合名(模型名禁含 @)。
	if _, ok := am.Next("openai@gpt-4o"); ok {
		t.Error("composite id should not be an aggregate")
	}
	// 不存在的模型名。
	if _, ok := am.Next("no-such-model"); ok {
		t.Error("unknown model should not be an aggregate")
	}
}

func TestSetMembersExcludeInclude(t *testing.T) {
	am, _ := newMgr(t)
	// 剔除 openai,只留 azure。
	if err := am.SetMembers("gpt-4o", []string{"azure"}); err != nil {
		t.Fatal(err)
	}
	m, ok := am.Next("gpt-4o")
	if !ok || m != "azure" {
		t.Errorf("after exclude, Next = %q, want azure", m)
	}
	// 添加回 openai → 两成员轮询。
	if err := am.SetMembers("gpt-4o", []string{"openai", "azure"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := am.Next("gpt-4o"); !ok {
		t.Error("Next should be ok after include")
	}
	// 非法成员(不含该模型的供应商)。
	if err := am.SetMembers("gpt-4o", []string{"deepseek"}); err == nil {
		t.Error("member without the model should fail")
	}
	// 不存在的模型名。
	if err := am.SetMembers("no-such", []string{"openai"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetMembers unknown err = %v, want ErrNotFound", err)
	}
}

func TestPersistence(t *testing.T) {
	pm, err := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"openai", "azure"} {
		if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: name, BaseURL: "http://x", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(t.TempDir(), "aggregates.json")
	am, err := NewManager(file, pm)
	if err != nil {
		t.Fatal(err)
	}
	if err := am.SetMembers("gpt-4o", []string{"azure"}); err != nil {
		t.Fatal(err)
	}
	// 重载后剔除仍在。
	am2, err := NewManager(file, pm)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := am2.Next("gpt-4o")
	if !ok || m != "azure" {
		t.Errorf("after reload, Next = %q, want azure", m)
	}
}

func TestConcurrentNext(t *testing.T) {
	am, _ := newMgr(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := am.Next("gpt-4o"); !ok {
				t.Error("Next failed")
			}
		}()
	}
	wg.Wait()
}

func TestTryOrderRotation(t *testing.T) {
	am, _ := newMgr(t)
	// gpt-4o 成员 [azure openai],轮询起点推进。
	first, ok := am.TryOrder("gpt-4o")
	if !ok || len(first) != 2 || first[0] != "azure" || first[1] != "openai" {
		t.Errorf("first TryOrder = %v, want [azure openai]", first)
	}
	second, ok := am.TryOrder("gpt-4o")
	if !ok || second[0] != "openai" || second[1] != "azure" {
		t.Errorf("second TryOrder = %v, want [openai azure]", second)
	}
}

func TestTryOrderSkipsBanned(t *testing.T) {
	am, _ := newMgr(t)
	am.Ban("gpt-4o", "azure")
	order, ok := am.TryOrder("gpt-4o")
	if !ok || len(order) != 1 || order[0] != "openai" {
		t.Errorf("after ban azure, TryOrder = %v, want [openai]", order)
	}
	// Next 同样跳过冷却成员。
	if m, ok := am.Next("gpt-4o"); !ok || m != "openai" {
		t.Errorf("Next after ban = %q, want openai", m)
	}
}

func TestTryOrderAllBannedFailOpen(t *testing.T) {
	am, _ := newMgr(t)
	am.Ban("gpt-4o", "azure", "openai")
	// 全部成员冷却时故障开放:返回全量照常尝试,避免模型整体不可用。
	order, ok := am.TryOrder("gpt-4o")
	if !ok || len(order) != 2 {
		t.Errorf("all banned TryOrder = %v, want fail-open full members", order)
	}
}

func TestBanNonMemberNoop(t *testing.T) {
	am, _ := newMgr(t)
	// deepseek 不拥有 gpt-4o,ban 应为 no-op。
	am.Ban("gpt-4o", "deepseek")
	order, ok := am.TryOrder("gpt-4o")
	if !ok || len(order) != 2 {
		t.Errorf("ban non-member should be noop, TryOrder = %v", order)
	}
	// 冷却仅存内存:重新加载(剔除名单持久化不受影响)后冷却清空。
	if m, ok := am.Next("gpt-4o"); !ok || (m != "azure" && m != "openai") {
		t.Errorf("Next = %q, want a valid member", m)
	}
}

func TestCooldownExpiry(t *testing.T) {
	am, _ := newMgr(t)
	am.Ban("gpt-4o", "azure")
	// 把冷却截止改为过去时间,模拟 10 分钟后到期恢复。
	am.mu.Lock()
	am.cooldown["gpt-4o"]["azure"] = time.Now().Add(-time.Second)
	am.mu.Unlock()
	order, ok := am.TryOrder("gpt-4o")
	if !ok || len(order) != 2 {
		t.Errorf("after expiry TryOrder = %v, want both members back", order)
	}
}
