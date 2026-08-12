package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"BSRouter/internal/logger"
	"BSRouter/internal/network"
	"BSRouter/internal/server"
)

func TestResolveDeployment(t *testing.T) {
	cases := []struct {
		name       string
		addr       string
		publicAddr string
		det        network.Detection
		wantMode   string
		wantIP     string
		wantBase   string
	}{
		{"loopback is local", "127.0.0.1:18154", "", network.Detection{DirectPublicIP: "1.2.3.4"}, server.ModeLocal, "", ""},
		{"localhost is local", "localhost:18154", "", network.Detection{}, server.ModeLocal, "", ""},
		{"all interfaces with direct public", ":18154", "", network.Detection{DirectPublicIP: "1.2.3.4"}, server.ModeDirect, "1.2.3.4", "http://1.2.3.4:18154"},
		{"0.0.0.0 with direct public", "0.0.0.0:18154", "", network.Detection{DirectPublicIP: "9.9.9.9"}, server.ModeDirect, "9.9.9.9", "http://9.9.9.9:18154"},
		{"all interfaces without public is nat", ":18154", "", network.Detection{PrivateIP: "10.0.0.1"}, server.ModeNAT, "", ""},
		{"explicit public bind is direct", "1.2.3.4:18154", "", network.Detection{}, server.ModeDirect, "1.2.3.4", "http://1.2.3.4:18154"},
		{"explicit private bind without public is nat", "192.168.1.5:18154", "", network.Detection{PrivateIP: "192.168.1.5"}, server.ModeNAT, "", ""},
		{"public-addr overrides", ":18154", "https://gw.example.com", network.Detection{}, server.ModeNAT, "", ""},
		{"port default 80", ":0", "", network.Detection{DirectPublicIP: "1.2.3.4"}, server.ModeDirect, "1.2.3.4", "http://1.2.3.4:80"},
	}
	for _, c := range cases {
		dep := resolveDeployment(c.addr, c.publicAddr, c.det)
		if dep.Mode != c.wantMode {
			t.Errorf("%s: Mode = %q, want %q", c.name, dep.Mode, c.wantMode)
		}
		if dep.DirectPublicIP != c.wantIP {
			t.Errorf("%s: DirectPublicIP = %q, want %q", c.name, dep.DirectPublicIP, c.wantIP)
		}
		if c.publicAddr == "" && c.wantMode == server.ModeDirect {
			if base := "http://" + net.JoinHostPort(dep.DirectPublicIP, dep.ListenPort); base != c.wantBase {
				t.Errorf("%s: effective base = %q, want %q", c.name, base, c.wantBase)
			}
		}
	}
}

func TestResolveDeploymentPublicAddrBase(t *testing.T) {
	// -public-addr 注入后,/manage/v1/network 的 advertised_base 应直接用该地址。
	dep := resolveDeployment(":18154", "https://gw.example.com", network.Detection{})
	if dep.PublicAddr != "https://gw.example.com" {
		t.Fatalf("PublicAddr = %q", dep.PublicAddr)
	}
	nm, err := network.NewManager(filepath.Join(t.TempDir(), "network.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(nil).WithDeployment(dep).WithNetworkManager(nm).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/manage/v1/network")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		AdvertisedBase string `json:"advertised_base"`
		Remote         bool   `json:"remote"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if !st.Remote || st.AdvertisedBase != "https://gw.example.com" {
		t.Errorf("network = %+v, want remote with advertised_base https://gw.example.com", st)
	}
}

func TestDefaultLogName(t *testing.T) {
	name := defaultLogName()
	// 每次运行以启动时间戳命名:gateway-<YYYYMMDD-HHMMSS>.log.jsonl。
	if !regexp.MustCompile(`^gateway-\d{8}-\d{6}\.log\.jsonl$`).MatchString(name) {
		t.Errorf("defaultLogName() = %q, want gateway-<timestamp>.log.jsonl", name)
	}
}

func TestResolveLogDetail(t *testing.T) {
	// 显式 flag 优先。
	if got := resolveLogDetail(true, "full", ""); got != server.LogDetailFull {
		t.Errorf("explicit full = %q", got)
	}
	if got := resolveLogDetail(true, "default", ""); got != server.LogDetailDefault {
		t.Errorf("explicit default = %q", got)
	}
	// 显式非法 flag → 回落 default。
	if got := resolveLogDetail(true, "nope", ""); got != server.LogDetailDefault {
		t.Errorf("invalid explicit = %q, want default", got)
	}
	// 未显式传:读持久化文件。
	file := filepath.Join(t.TempDir(), "logdetail.json")
	if err := os.WriteFile(file, []byte(`{"detail":"full"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveLogDetail(false, "", file); got != server.LogDetailFull {
		t.Errorf("persisted full = %q", got)
	}
	// 持久化文件缺失 → default。
	if got := resolveLogDetail(false, "", filepath.Join(t.TempDir(), "none.json")); got != server.LogDetailDefault {
		t.Errorf("missing file = %q, want default", got)
	}
	// 持久化文件非法 → default。
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveLogDetail(false, "", bad); got != server.LogDetailDefault {
		t.Errorf("bad file = %q, want default", got)
	}
}

func TestMigrateFile(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "providers.json")
	if err := os.WriteFile(src, []byte(`[{"name":"x"}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(base, "cfg", "sub")
	dst := filepath.Join(dstDir, "providers.json")
	// 源存在、目标不存在 → 复制并创建目录。
	migrateFile(dst, src)
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != `[{"name":"x"}]` {
		t.Fatalf("migrate copy = %v %q", err, data)
	}
	// 目标已存在 → 绝不覆盖。
	if err := os.WriteFile(dst, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateFile(dst, src)
	if data, _ := os.ReadFile(dst); string(data) != "new" {
		t.Errorf("migrate overwrote existing dst: %q", data)
	}
	// 源不存在 → 无操作(不创建目录/文件)。
	migrateFile(filepath.Join(base, "cfg2", "no.json"), filepath.Join(base, "no.json"))
	if _, err := os.Stat(filepath.Join(base, "cfg2", "no.json")); !os.IsNotExist(err) {
		t.Errorf("migrate created dst without src: %v", err)
	}
	// 空目标 → no-op。
	migrateFile("", src)
}

func TestRunServerGracefulShutdown(t *testing.T) {
	// 找一个空闲端口。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	var hits atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	lg, err := logger.New(filepath.Join(t.TempDir(), "gw.log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	go func() { done <- runServer(ctx, addr, handler, lg) }()

	// 等服务起来,并发一个请求。
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}

	// 触发优雅退出:runServer 应正常返回。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not return after cancel")
	}

	// 服务已关闭,后续请求应失败。
	if resp, err := http.Get("http://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Error("server still accepting after shutdown")
	}
}
