package logger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoggerWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Log(Entry{Timestamp: "2026-01-01T00:00:00Z", Method: "GET", Path: "/v1/models", Status: 200, DurationMS: 5, Model: "openai@gpt-4o", Provider: "openai"})
	l.Log(Entry{Timestamp: "2026-01-01T00:00:01Z", Method: "POST", Path: "/v1/chat/completions", Status: 502, DurationMS: 120})

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lines []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q is not valid JSON: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	if lines[0]["path"] != "/v1/models" || lines[0]["provider"] != "openai" || lines[0]["status"] != float64(200) {
		t.Errorf("first line = %+v", lines[0])
	}
	if lines[1]["status"] != float64(502) {
		t.Errorf("second line status = %v, want 502", lines[1]["status"])
	}
}

func TestLoggerConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const goroutines, perGoroutine = 20, 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				l.Log(Entry{Method: "GET", Path: "/v1/models", Status: 200})
			}
		}()
	}
	wg.Wait()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not valid JSON: %v", err)
		}
		n++
	}
	if want := goroutines * perGoroutine; n != want {
		t.Errorf("lines = %d, want %d", n, want)
	}
}

func TestLoggerRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 5; i++ {
		l.Log(Entry{Timestamp: fmt.Sprintf("t%d", i), Method: "GET", Path: fmt.Sprintf("/p%d", i), Status: 200})
	}

	entries, err := l.Recent(3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// 最新的在前。
	if entries[0].Path != "/p4" || entries[2].Path != "/p2" {
		t.Errorf("recent order = %+v", entries)
	}
}

// Recent 带过滤:只返回满足条件的行(最新在前);管理端点的日志可据此从列表剔除。
func TestLoggerRecentFiltered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// 交替写入 /api 与 /manage 日志,API 数量不足 n 时应扩大窗口找全。
	for i := 0; i < 6; i++ {
		l.Log(Entry{Timestamp: fmt.Sprintf("t%d", i), Method: "GET", Path: fmt.Sprintf("/api/p%d", i), Status: 200})
		l.Log(Entry{Timestamp: fmt.Sprintf("m%d", i), Method: "GET", Path: fmt.Sprintf("/manage/m%d", i), Status: 200})
	}

	keepAPI := func(e Entry) bool { return strings.HasPrefix(e.Path, "/api") }
	entries, err := l.Recent(10, keepAPI)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 {
		t.Fatalf("entries = %d, want 6 (all /api lines)", len(entries))
	}
	// 最新在前,且都是 /api 路径。
	if entries[0].Path != "/api/p5" {
		t.Errorf("newest entry = %q, want /api/p5", entries[0].Path)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, "/api") {
			t.Errorf("entry leaked non-api path: %s", e.Path)
		}
	}

	// 截断请求:只返回最近 2 条 API 日志(跳过中间的 /manage)。
	entries2, err := l.Recent(2, keepAPI)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) != 2 || entries2[0].Path != "/api/p5" || entries2[1].Path != "/api/p4" {
		t.Errorf("filtered top-2 = %+v", entries2)
	}
}
