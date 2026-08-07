package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BSRouter/internal/network"
)

func newNetMgr(t *testing.T) *network.Manager {
	t.Helper()
	nm, err := network.NewManager(filepath.Join(t.TempDir(), "network.json"))
	if err != nil {
		t.Fatal(err)
	}
	return nm
}

// newNetServer 构造启用了网络状态与 claude 预设的测试服务。
func newNetServer(t *testing.T, dep *Deployment, nm *network.Manager) *httptest.Server {
	t.Helper()
	cm := newClaudeMgr(t)
	srv := httptest.NewServer(New(newMgr(t)).WithDeployment(dep).WithNetworkManager(nm).WithClaudePresets(cm).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestNetworkEndpoint(t *testing.T) {
	nm := newNetMgr(t)
	dep := &Deployment{Mode: ModeNAT, ListenPort: "18154"}
	srv := newNetServer(t, dep, nm)

	// GET: nat 未配置 → remote,无 advertised_base。
	resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/network", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d; body=%s", resp.StatusCode, b)
	}
	var st networkStatus
	if err := json.Unmarshal([]byte(b), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Remote || st.Mode != ModeNAT || st.AdvertisedBase != "" {
		t.Errorf("initial network = %+v, want remote+nat+no base", st)
	}

	// PUT 设置出口 → advertised_base 生效。
	resp, b = doJSON(t, srv, http.MethodPut, "/manage/v1/network", `{"egress_host":"1.2.3.4","egress_port":"443"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%s", resp.StatusCode, b)
	}
	if err := json.Unmarshal([]byte(b), &st); err != nil {
		t.Fatal(err)
	}
	if st.EgressHost != "1.2.3.4" || st.EgressPort != "443" || st.AdvertisedBase != "http://1.2.3.4:443" {
		t.Errorf("after PUT network = %+v", st)
	}

	// 非法出口 → 400。
	if resp, b := doJSON(t, srv, http.MethodPut, "/manage/v1/network", `{"egress_host":"http://bad","egress_port":"443"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid PUT status = %d; body=%s", resp.StatusCode, b)
	}
}

func TestNetworkDirectMode(t *testing.T) {
	dep := &Deployment{Mode: ModeDirect, ListenPort: "18154", DirectPublicIP: "1.2.3.4"}
	srv := newNetServer(t, dep, newNetMgr(t))

	resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/network", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d; body=%s", resp.StatusCode, b)
	}
	var st networkStatus
	if err := json.Unmarshal([]byte(b), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Remote || st.Mode != ModeDirect || st.DirectPublicIP != "1.2.3.4" || st.AdvertisedBase != "http://1.2.3.4:18154" {
		t.Errorf("direct network = %+v", st)
	}
}

func TestEffectiveBaseURL(t *testing.T) {
	t.Run("nat unconfigured no rewrite", func(t *testing.T) {
		s := &Server{deployment: &Deployment{Mode: ModeNAT, ListenPort: "18154"}}
		if got := s.effectiveBaseURL("http://127.0.0.1:18154/api"); got != "http://127.0.0.1:18154/api" {
			t.Errorf("unconfigured rewrite = %q", got)
		}
	})
	t.Run("nat configured rewrite loopback", func(t *testing.T) {
		nm := newNetMgr(t)
		if err := nm.Set(network.Config{EgressHost: "1.2.3.4", EgressPort: "443"}); err != nil {
			t.Fatal(err)
		}
		s := &Server{deployment: &Deployment{Mode: ModeNAT, ListenPort: "18154"}, netm: nm}
		got := s.effectiveBaseURL("http://127.0.0.1:18154/api/term-a")
		if got != "http://1.2.3.4:443/api/term-a" {
			t.Errorf("rewrite = %q, want http://1.2.3.4:443/api/term-a", got)
		}
	})
	t.Run("direct rewrite loopback with path and query", func(t *testing.T) {
		s := &Server{deployment: &Deployment{Mode: ModeDirect, ListenPort: "18154", DirectPublicIP: "1.2.3.4"}}
		got := s.effectiveBaseURL("http://localhost:8080/api/x?k=v")
		if got != "http://1.2.3.4:18154/api/x?k=v" {
			t.Errorf("rewrite with query = %q", got)
		}
	})
	t.Run("non-loopback unchanged", func(t *testing.T) {
		nm := newNetMgr(t)
		if err := nm.Set(network.Config{EgressHost: "1.2.3.4", EgressPort: "443"}); err != nil {
			t.Fatal(err)
		}
		s := &Server{deployment: &Deployment{Mode: ModeNAT, ListenPort: "18154"}, netm: nm}
		if got := s.effectiveBaseURL("http://gw.example.com/api"); got != "http://gw.example.com/api" {
			t.Errorf("non-loopback changed = %q", got)
		}
	})
	t.Run("no deployment unchanged", func(t *testing.T) {
		s := &Server{}
		if got := s.effectiveBaseURL("http://127.0.0.1:18154/api"); got != "http://127.0.0.1:18154/api" {
			t.Errorf("no deployment changed = %q", got)
		}
	})
}

// addDevPreset 往测试服务新增一条 base_url 指向回环的预设。
func addDevPreset(t *testing.T, srv *httptest.Server) {
	t.Helper()
	body := `{"name":"dev","base_url":"http://127.0.0.1:18154/api/term-a","model":"m"}`
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, b)
	}
}

// 命令端点:direct/nat-已配置 替换回环 base_url;nat 未配置返回 warning;列表不改存储。
func TestClaudeCommandRemoteBaseURL(t *testing.T) {
	t.Run("direct rewrites", func(t *testing.T) {
		srv := newNetServer(t, &Deployment{Mode: ModeDirect, ListenPort: "18154", DirectPublicIP: "1.2.3.4"}, newNetMgr(t))
		addDevPreset(t, srv)
		resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev/command", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("command status = %d; body=%s", resp.StatusCode, b)
		}
		if !strings.Contains(b, "http://1.2.3.4:18154/api/term-a") || strings.Contains(b, "127.0.0.1") {
			t.Errorf("command not rewritten:\n%s", b)
		}
		if strings.Contains(b, `"warning"`) {
			t.Errorf("direct command should not warn:\n%s", b)
		}
	})

	t.Run("nat unconfigured warns", func(t *testing.T) {
		srv := newNetServer(t, &Deployment{Mode: ModeNAT, ListenPort: "18154"}, newNetMgr(t))
		addDevPreset(t, srv)
		resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev/command", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("command status = %d; body=%s", resp.StatusCode, b)
		}
		if !strings.Contains(b, "127.0.0.1:18154") {
			t.Errorf("unconfigured command should keep loopback:\n%s", b)
		}
		if !strings.Contains(b, `"warning"`) {
			t.Errorf("unconfigured nat command should carry warning:\n%s", b)
		}
	})

	t.Run("nat configured rewrites", func(t *testing.T) {
		nm := newNetMgr(t)
		if err := nm.Set(network.Config{EgressHost: "1.2.3.4", EgressPort: "443"}); err != nil {
			t.Fatal(err)
		}
		srv := newNetServer(t, &Deployment{Mode: ModeNAT, ListenPort: "18154"}, nm)
		addDevPreset(t, srv)
		resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/claude-presets/dev/command", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("command status = %d; body=%s", resp.StatusCode, b)
		}
		if !strings.Contains(b, "http://1.2.3.4:443/api/term-a") || strings.Contains(b, "127.0.0.1") {
			t.Errorf("configured nat command not rewritten:\n%s", b)
		}
		if strings.Contains(b, `"warning"`) {
			t.Errorf("configured nat command should not warn:\n%s", b)
		}
	})

	t.Run("list keeps stored value", func(t *testing.T) {
		srv := newNetServer(t, &Deployment{Mode: ModeDirect, ListenPort: "18154", DirectPublicIP: "1.2.3.4"}, newNetMgr(t))
		addDevPreset(t, srv)
		resp, b := doJSON(t, srv, http.MethodGet, "/manage/v1/claude-presets", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list status = %d; body=%s", resp.StatusCode, b)
		}
		if !strings.Contains(b, "http://127.0.0.1:18154/api/term-a") {
			t.Errorf("list should keep stored loopback base_url:\n%s", b)
		}
	})
}

// apply-local 不替换回环(写网关本机 settings.json,回环最可靠)。
func TestApplyLocalKeepsLoopback(t *testing.T) {
	nm := newNetMgr(t)
	if err := nm.Set(network.Config{EgressHost: "1.2.3.4", EgressPort: "443"}); err != nil {
		t.Fatal(err)
	}
	cm := newClaudeMgr(t)
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	srv := httptest.NewServer(New(newMgr(t)).WithDeployment(&Deployment{Mode: ModeNAT, ListenPort: "18154"}).WithNetworkManager(nm).WithClaudePresets(cm).WithClaudeSettingsPath(settingsPath).Handler())
	defer srv.Close()

	if resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets", `{"name":"dev","base_url":"http://127.0.0.1:18154/api","model":"m"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; body=%s", resp.StatusCode, b)
	}
	resp, b := doJSON(t, srv, http.MethodPost, "/manage/v1/claude-presets/dev/apply-local", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply-local status = %d; body=%s", resp.StatusCode, b)
	}
	var out struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "http://127.0.0.1:18154/api") || strings.Contains(string(data), "1.2.3.4") {
		t.Errorf("apply-local should keep loopback base_url:\n%s", data)
	}
}
