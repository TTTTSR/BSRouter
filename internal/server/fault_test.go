package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/fault"
	"BSRouter/internal/provider"
)

// TestFaultRecordedOnUpstreamError 验证上游返回"余额不足"时,故障被记录(用户模式
// 命中硬编码特定故障),并可经 /manage/v1/faults 列表与删除。
func TestFaultRecordedOnUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired) // 402
		fmt.Fprint(w, `{"error":{"message":"Insufficient Balance"}}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	fm, err := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(mgr).WithFaults(fm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	faults := fm.List()
	if len(faults) != 1 {
		t.Fatalf("fault count = %d, want 1", len(faults))
	}
	if faults[0].Category != fault.CategoryInsufficientBalance {
		t.Errorf("category = %q, want insufficient_balance", faults[0].Category)
	}
	if faults[0].Provider != "oa" || faults[0].Model != "oa@gpt-4o" {
		t.Errorf("fault provider/model = %q/%q", faults[0].Provider, faults[0].Model)
	}

	// 经管理端点读取。
	lr, err := http.Get(srv.URL + "/manage/v1/faults")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Mode   string        `json:"mode"`
		Faults []fault.Fault `json:"faults"`
	}
	if err := json.NewDecoder(lr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	lr.Body.Close()
	if list.Mode != "user" || len(list.Faults) != 1 {
		t.Fatalf("list = %+v", list)
	}
	id := list.Faults[0].ID

	// 经管理端点删除。
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/manage/v1/faults/"+id, nil)
	dr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dr.Body.Close()
	if dr.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", dr.StatusCode)
	}
	if fm.Count() != 0 {
		t.Fatalf("count after delete = %d, want 0", fm.Count())
	}
}

// TestFaultUserModeSkipsGenericError 验证用户模式下普通上游错误(非硬编码特定故障)
// 不被记录。
func TestFaultUserModeSkipsGenericError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	t.Cleanup(upstream.Close)

	mgr, _ := provider.NewManager(filepath.Join(t.TempDir(), "p.json"))
	if err := mgr.Add(provider.Config{Kind: provider.KindCompletion, Name: "oa", BaseURL: upstream.URL, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	fm, err := fault.NewManager(filepath.Join(t.TempDir(), "faults.json"), fault.ModeUser)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(mgr).WithFaults(fm).Handler())
	defer srv.Close()

	body := `{"model":"oa@gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if fm.Count() != 0 {
		t.Fatalf("user mode recorded generic error: count=%d", fm.Count())
	}
}
