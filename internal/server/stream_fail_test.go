// 流式失败的可观测性测试:截断/断流/客户端断开/超时/重试在请求日志中的记录行为。
package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"BSRouter/internal/gateway"
	"BSRouter/internal/logger"
	"BSRouter/internal/provider"
)

// 转换路径截断(completion 上游发部分内容后 EOF,无 [DONE]/finish_reason):
// 客户端应收到 error 事件,且请求日志记录 error 与 forward_response。
func TestStreamTruncationLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		// 无 [DONE]、无 finish_reason → 截断
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "og", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/gw.log.jsonl"
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"og@deepseek-v4-flash","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (SSE committed before truncation); body=%s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "event: error") || !strings.Contains(string(data), "upstream stream ended") {
		t.Errorf("client should receive error event, got:\n%s", data)
	}

	entries := readLogEntries(t, logPath)
	var e *logger.Entry
	for i := range entries {
		if entries[i].Path == "/api/v1/messages" {
			e = &entries[i]
			break
		}
	}
	if e == nil {
		t.Fatalf("no log entry")
	}
	if e.Status != 200 {
		t.Errorf("log status = %d, want 200", e.Status)
	}
	if !strings.Contains(e.Error, "upstream stream ended") {
		t.Errorf("log error = %q, want truncation error recorded", e.Error)
	}
	if e.ForwardResponse == "" {
		t.Errorf("forward_response should be recorded for the truncated upstream body")
	}
}

// 客户端中断且上游正常流式:中断表现为"取消"类错误(context canceled),而非被误报成
// 上游截断/断流。这是可接受行为——客户端取消会传播到上游请求产生读错误,与"上游慢导致
// 客户端超时放弃"(用户关心的 218 字节失败)在网关层面无法区分,故统一记录 cancellation。
func TestStreamClientDisconnectShowsCancellation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			fmt.Fprintf(w, `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`+"\n\n")
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "og", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/gw.log.jsonl"
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler())
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reqBody := `{"model":"og@deepseek-v4-flash","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	httpReq := "POST /api/v1/messages HTTP/1.1\r\nHost: " + addr + "\r\nContent-Type: application/json\r\nContent-Length: " + fmt.Sprint(len(reqBody)) + "\r\nConnection: close\r\n\r\n" + reqBody
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		t.Fatal(err)
	}
	// 读一段流式内容后主动中断(客户端断开)。
	br := make([]byte, 4096)
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := conn.Read(br); err != nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	conn.Close()

	// 等网关把日志落盘。
	waitLog(t, logPath, "/api/v1/messages")
	entries := readLogEntries(t, logPath)
	var e *logger.Entry
	for i := range entries {
		if entries[i].Path == "/api/v1/messages" {
			e = &entries[i]
			break
		}
	}
	if e == nil {
		t.Fatalf("no log entry")
	}
	// 中断可能体现为空或 "context canceled" 等取消类消息,但绝不能误报成上游截断。
	if strings.Contains(e.Error, "upstream stream ended") {
		t.Errorf("log error = %q, must NOT claim upstream truncation for a client abort", e.Error)
	}
}

// idle 超时:上游发首个角色块后停更超过阈值 → 客户端收到 error 事件、日志记录 "idle timeout"。
func TestStreamIdleTimeoutLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""},"finish_reason":null}]}`+"\n\n")
		fl.Flush()
		time.Sleep(400 * time.Millisecond) // 超过 idle 超时
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "og", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/gw.log.jsonl"
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	srv := httptest.NewServer(New(m).WithLogger(lg).WithStreamIdleTimeout(50 * time.Millisecond).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"og@deepseek-v4-flash","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(data), "event: error") || !strings.Contains(string(data), "idle timeout") {
		t.Errorf("client should receive idle-timeout error event, got:\n%s", data)
	}

	entries := readLogEntries(t, logPath)
	var e *logger.Entry
	for i := range entries {
		if entries[i].Path == "/api/v1/messages" {
			e = &entries[i]
			break
		}
	}
	if e == nil {
		t.Fatalf("no log entry")
	}
	if !strings.Contains(e.Error, "idle timeout") {
		t.Errorf("log error = %q, want idle timeout recorded", e.Error)
	}
}

// idle 超时默认禁用:不配置 WithStreamIdleTimeout 时,上游慢但最终完成不被误杀。
func TestStreamIdleTimeoutDisabledByDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""},"finish_reason":null}]}`+"\n\n")
		fl.Flush()
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, `data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "og", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/gw.log.jsonl"
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	// 默认 New(m) 不配置 idle 超时 → 流应正常走完。
	srv := httptest.NewServer(New(m).WithLogger(lg).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"og@deepseek-v4-flash","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(data), "idle timeout") || strings.Contains(string(data), "event: error") {
		t.Errorf("stream should complete normally without idle timeout, got:\n%s", data)
	}
	entries := readLogEntries(t, logPath)
	var e *logger.Entry
	for i := range entries {
		if entries[i].Path == "/api/v1/messages" {
			e = &entries[i]
			break
		}
	}
	if e != nil && strings.Contains(e.Error, "idle timeout") {
		t.Errorf("log error = %q, want no idle timeout", e.Error)
	}
}

// 重试:第 1 次 503、第 2 次成功 → 返回 200,上游被调 2 次,日志无 error。
func TestStreamRetryTransient(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"type":"server_error","message":"Endpoint is unavailable."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "og", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/gw.log.jsonl"
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	srv := httptest.NewServer(New(m).WithLogger(lg).WithStreamRetries(2).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"og@deepseek-v4-flash","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, data)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls = %d, want 2", got)
	}
	entries := readLogEntries(t, logPath)
	for _, e := range entries {
		if e.Path == "/api/v1/messages" && e.Error != "" {
			t.Errorf("log error = %q, want empty after successful retry", e.Error)
		}
	}
}

// 重试耗尽:一直 503 → 返回 502,上游被调 3 次,日志记录最终错误。
func TestStreamRetryExhausted(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"type":"server_error","message":"Endpoint is unavailable."}}`)
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "og", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/gw.log.jsonl"
	lg, err := logger.New(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	srv := httptest.NewServer(New(m).WithLogger(lg).WithStreamRetries(2).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"og@deepseek-v4-flash","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", resp.StatusCode, data)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("upstream calls = %d, want 3 (initial + 2 retries)", got)
	}
	entries := readLogEntries(t, logPath)
	var e *logger.Entry
	for i := range entries {
		if entries[i].Path == "/api/v1/messages" {
			e = &entries[i]
			break
		}
	}
	if e == nil {
		t.Fatalf("no log entry")
	}
	if !strings.Contains(e.Error, "upstream returned 503") {
		t.Errorf("log error = %q, want 503 recorded", e.Error)
	}
}

// 非可重试错误(401)不重试 → 只调 1 次。
func TestStreamRetryNonRetryable(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	t.Cleanup(upstream.Close)

	m := newMgr(t)
	if err := m.Add(provider.Config{Kind: provider.KindCompletion, Name: "og", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m).WithStreamRetries(2).Handler())
	t.Cleanup(srv.Close)

	reqBody := `{"model":"og@deepseek-v4-flash","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (401 not retryable)", got)
	}
}

// retryableStreamError 分类:5xx 重试、4xx 不重试、连接/超时错误重试、ctx 取消不重试。
func TestStreamRetryableErrorClassification(t *testing.T) {
	s := New(nil)
	ctx := context.Background()
	if !s.retryableStreamError(ctx, fakeStatusErr{503}) {
		t.Error("503 should be retryable")
	}
	if !s.retryableStreamError(ctx, fakeStatusErr{502}) {
		t.Error("502 should be retryable")
	}
	if s.retryableStreamError(ctx, fakeStatusErr{401}) {
		t.Error("401 should NOT be retryable")
	}
	if s.retryableStreamError(ctx, fakeStatusErr{400}) {
		t.Error("400 should NOT be retryable")
	}
	if !s.retryableStreamError(ctx, &url.Error{Err: context.DeadlineExceeded}) {
		t.Error("connection error should be retryable")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if s.retryableStreamError(cctx, fakeStatusErr{503}) {
		t.Error("canceled ctx should NOT retry")
	}
}

type fakeStatusErr struct{ code int }

func (e fakeStatusErr) Error() string { return fmt.Sprintf("status %d", e.code) }
func (e fakeStatusErr) HTTPStatus() int { return e.code }

// 聚合 + 转换路径:成员 A 一直 503、成员 B 正常。客户端用 Anthropic 请求聚合裸名
// (completion 成员不支持 anthropic → 走转换路径 streamComplete)。WithStreamRetries(2)
// 时 A 内部重试 3 次后转移到 B(成员内重试与成员间故障转移共存)。
func TestAggregateStreamRetryThenFailover(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	badUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts["aup"]++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"type":"server_error","message":"down"}}`)
	}))
	t.Cleanup(badUp.Close)
	goodUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts["bup"]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(goodUp.Close)

	pm := newMgr(t)
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "aup", BaseURL: badUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Add(provider.Config{Kind: provider.KindCompletion, Name: "bup", BaseURL: goodUp.URL, APIKey: "k", Models: []provider.ModelConfig{{Name: "gpt-4o"}}}); err != nil {
		t.Fatal(err)
	}
	am := newAggMgr(t, pm)
	srv := httptest.NewServer(New(pm).WithAggregates(am).WithStreamRetries(2).Handler())
	t.Cleanup(srv.Close)

	// Anthropic 请求 → completion 聚合成员不支持 anthropic → 转换路径(streamComplete)。
	body := `{"model":"gpt-4o","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, data)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["aup"] != 3 {
		t.Errorf("aup hits = %d, want 3 (1 attempt + 2 retries before failover)", counts["aup"])
	}
	if counts["bup"] != 1 {
		t.Errorf("bup hits = %d, want 1", counts["bup"])
	}
}

func waitLog(t *testing.T, path, needle string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log entry %q not written within deadline:\n%s", needle, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var _ = gateway.FormatAnthropic // 保持 import 引用
