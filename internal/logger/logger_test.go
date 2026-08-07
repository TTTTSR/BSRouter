package logger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	entries, err := l.Recent(3)
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
