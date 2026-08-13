package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"BSRouter/internal/aggregate"
	"BSRouter/internal/fault"
	"BSRouter/internal/provider"
)

// postStatus POST 一段 JSON 并返回状态码与响应体。
func postStatus(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// waitBlocked 轮询等待供应商被故障阻塞(记录故障与响应完成之间存在并发窗口)。
func waitBlocked(t *testing.T, fm *fault.Manager, name string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, blocked := fm.Block(name); blocked {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestBlockedProviderStandalone 验证单独请求被故障禁用的供应商时返回 503 与阻塞原因。
func TestBlockedProviderStandalone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired) // 402 余额不足
		fmt.Fprint(w, `{"error":{"message":"Insufficient Balance"}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	fm, _ := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	srv := httptest.NewServer(New(mgr).WithFaults(fm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	// 第一次:上游余额不足,记故障并返回 502。
	if s, _ := postStatus(t, srv.URL+"/api/v1/chat/completions", body); s != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", s)
	}
	if !waitBlocked(t, fm, "oa") {
		t.Fatal("oa should be blocked after insufficient balance")
	}
	// 第二次:被阻塞,返回 503 + 阻塞原因。
	s, b := postStatus(t, srv.URL+"/api/v1/chat/completions", body)
	if s != http.StatusServiceUnavailable {
		t.Fatalf("blocked status = %d, want 503", s)
	}
	if !strings.Contains(b, "blocked") || !strings.Contains(b, "insufficient balance") {
		t.Errorf("blocked response = %q, want blocked reason", b)
	}
}

// TestBlockedProviderAggregateSkips 验证聚合请求跳过被故障禁用的成员,只走健康成员。
func TestBlockedProviderAggregateSkips(t *testing.T) {
	var aCalls int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aCalls, 1)
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"error":{"message":"Insufficient Balance"}}`)
	}))
	t.Cleanup(a.Close)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(b.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "pa", BaseURL: a.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "pb", BaseURL: b.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am, err := aggregate.NewManager(filepath.Join(t.TempDir(), "agg.json"), mgr)
	if err != nil {
		t.Fatal(err)
	}
	fm, _ := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	srv := httptest.NewServer(New(mgr).WithAggregates(am).WithFaults(fm).Handler())
	defer srv.Close()

	// 先单独请求 pa 触发余额不足 → pa 被阻塞。
	postStatus(t, srv.URL+"/api/v1/chat/completions", `{"model":"pa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if !waitBlocked(t, fm, "pa") {
		t.Fatal("pa should be blocked")
	}

	// 聚合请求应跳过 pa,只走 pb。
	atomic.StoreInt32(&aCalls, 0)
	s, body := postStatus(t, srv.URL+"/api/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if s != http.StatusOK {
		t.Fatalf("aggregate status = %d, want 200 (body %s)", s, body)
	}
	if !strings.Contains(body, "from-b") {
		t.Errorf("aggregate response = %q, want from-b", body)
	}
	if atomic.LoadInt32(&aCalls) != 0 {
		t.Errorf("blocked provider pa was called %d times, want 0", atomic.LoadInt32(&aCalls))
	}
}

// TestBlockedMemberRecordedMidFailover 验证故障转移中途即记录:成员 pa 报余额不足但
// 后续成员 pb 成功时,pa 仍被记录并阻塞(而不是等整个请求失败才记)。
func TestBlockedMemberRecordedMidFailover(t *testing.T) {
	pa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"error":{"message":"Insufficient Balance"}}`)
	}))
	t.Cleanup(pa.Close)
	pb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"from-pb"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(pb.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "pa", BaseURL: pa.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "pb", BaseURL: pb.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am, err := aggregate.NewManager(filepath.Join(t.TempDir(), "agg.json"), mgr)
	if err != nil {
		t.Fatal(err)
	}
	fm, _ := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	srv := httptest.NewServer(New(mgr).WithAggregates(am).WithFaults(fm).Handler())
	defer srv.Close()

	// 聚合请求:pa(余额不足)→ pb(成功)。整体成功,但 pa 应已被中途记录并阻塞。
	s, body := postStatus(t, srv.URL+"/api/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if s != http.StatusOK {
		t.Fatalf("aggregate status = %d, want 200 (body %s)", s, body)
	}
	if !strings.Contains(body, "from-pb") {
		t.Errorf("response = %q, want from-pb", body)
	}
	if !waitBlocked(t, fm, "pa") {
		t.Fatal("pa should be blocked mid-failover even though pb succeeded")
	}
	// 单独请求被阻塞的 pa → 503。
	if s, _ := postStatus(t, srv.URL+"/api/v1/chat/completions", `{"model":"pa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`); s != http.StatusServiceUnavailable {
		t.Fatalf("pa standalone status = %d, want 503", s)
	}
}

// TestRateLimitBlock429 验证上游 429 触发限流阻塞(2h 自动解除),单独请求返回 503,
// 故障带自动解除时间。
func TestRateLimitBlock429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // 429
		fmt.Fprint(w, `{"error":{"message":"rate limit exceeded"}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	fm, _ := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	srv := httptest.NewServer(New(mgr).WithFaults(fm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	if s, _ := postStatus(t, srv.URL+"/api/v1/chat/completions", body); s != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", s)
	}
	if !waitBlocked(t, fm, "oa") {
		t.Fatal("oa should be blocked by 429")
	}
	faults := fm.List()
	if len(faults) != 1 || faults[0].Category != fault.CategoryRateLimited {
		t.Fatalf("faults = %+v, want one rate_limited", faults)
	}
	if faults[0].ExpiresAt == "" {
		t.Fatal("rate_limited fault should carry expires_at")
	}
	if s, b := postStatus(t, srv.URL+"/api/v1/chat/completions", body); s != http.StatusServiceUnavailable {
		t.Fatalf("blocked status = %d, body %s", s, b)
	}
}

// TestProviderBlockStatusOverride 验证按供应商自定义限流错误码:上游返回 403(被配置为
// 限流)时触发限流阻塞。
func TestProviderBlockStatusOverride(t *testing.T) {
	rl := 403
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":"quota-ish limit"}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}, RateLimitStatus: &rl}); err != nil {
		t.Fatal(err)
	}
	fm, _ := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	srv := httptest.NewServer(New(mgr).WithFaults(fm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	postStatus(t, srv.URL+"/api/v1/chat/completions", body)
	if !waitBlocked(t, fm, "oa") {
		t.Fatal("oa should be blocked via custom rate_limit_status=403")
	}
	faults := fm.List()
	if len(faults) != 1 || faults[0].Category != fault.CategoryRateLimited {
		t.Fatalf("faults = %+v, want one rate_limited", faults)
	}
}

// TestProviderRateLimitDisabled 验证供应商关闭限流阻塞后,429 不再触发阻塞(用户模式
// 不记故障,请求仍按上游错误返回 502)。
func TestProviderRateLimitDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limit"}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	rlOff := false
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}, RateLimitEnabled: &rlOff}); err != nil {
		t.Fatal(err)
	}
	fm, _ := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	srv := httptest.NewServer(New(mgr).WithFaults(fm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	if s, _ := postStatus(t, srv.URL+"/api/v1/chat/completions", body); s != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (upstream still errored)", s)
	}
	if _, blocked := fm.Block("oa"); blocked {
		t.Error("oa should not be blocked when rate-limit block is disabled")
	}
	if fm.Count() != 0 {
		t.Errorf("count = %d, want 0 (user mode + rate-limit disabled)", fm.Count())
	}
}

// TestProviderInsufficientStatusOverride 验证按供应商自定义余额不足错误码:上游 429(被
// 配置为余额不足)触发持久阻塞(无自动解除时间)。
func TestProviderInsufficientStatusOverride(t *testing.T) {
	ins := 429
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"balance exhausted"}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}, InsufficientBalanceStatus: &ins}); err != nil {
		t.Fatal(err)
	}
	fm, _ := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	srv := httptest.NewServer(New(mgr).WithFaults(fm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	postStatus(t, srv.URL+"/api/v1/chat/completions", body)
	if !waitBlocked(t, fm, "oa") {
		t.Fatal("oa should be blocked via custom insufficient_balance_status=429")
	}
	faults := fm.List()
	if len(faults) != 1 || faults[0].Category != fault.CategoryInsufficientBalance {
		t.Fatalf("faults = %+v, want one insufficient_balance", faults)
	}
	if faults[0].ExpiresAt != "" {
		t.Fatalf("insufficient_balance should be persistent, got expires_at=%q", faults[0].ExpiresAt)
	}
}

// TestBlockUnblocksAfterDelete 验证用户删除故障(已处理)后恢复正常转发。
func TestBlockUnblocksAfterDelete(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusPaymentRequired)
			fmt.Fprint(w, `{"error":{"message":"Insufficient Balance"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k",
		Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	fm, _ := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	srv := httptest.NewServer(New(mgr).WithFaults(fm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	if s, _ := postStatus(t, srv.URL+"/api/v1/chat/completions", body); s != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", s)
	}
	if !waitBlocked(t, fm, "oa") {
		t.Fatal("oa should be blocked")
	}
	if s, _ := postStatus(t, srv.URL+"/api/v1/chat/completions", body); s != http.StatusServiceUnavailable {
		t.Fatalf("blocked status = %d, want 503", s)
	}

	// 删除 oa 的余额不足故障(表示用户已处理)。
	for _, f := range fm.List() {
		if f.Provider == "oa" {
			if err := fm.Delete(f.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 恢复正常转发(上游第 2 次起返回成功)。
	if s, b := postStatus(t, srv.URL+"/api/v1/chat/completions", body); s != http.StatusOK {
		t.Fatalf("after delete status = %d, body %s", s, b)
	}
}
