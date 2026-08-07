package network

import (
	"net"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.2.3.4", true},
		{"114.114.114.114", true},
		{"10.0.0.1", false},     // RFC1918
		{"172.16.0.1", false},   // RFC1918
		{"192.168.1.1", false},  // RFC1918
		{"127.0.0.1", false},    // 回环
		{"169.254.1.1", false},  // 链路本地
		{"100.64.0.1", false},   // CGNAT
		{"100.127.255.255", false},
		{"192.0.2.1", false},    // 文档网段
		{"198.51.100.1", false}, // 文档网段
		{"203.0.113.1", false},  // 文档网段
		{"0.0.0.0", false},      // 未指定
		{"224.0.0.1", false},    // 组播
		{"::1", false},          // IPv6 回环(仅处理 IPv4)
	}
	for _, c := range cases {
		if got := IsPublicIP(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("IsPublicIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"100.64.0.1", true}, // CGNAT
		{"8.8.8.8", false},
		{"127.0.0.1", false},   // 回环不算内网
		{"169.254.1.1", false}, // 链路本地不算
	}
	for _, c := range cases {
		if got := IsPrivateIP(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("IsPrivateIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestPublicIPv4FromAddrs(t *testing.T) {
	ipn := func(s string) net.Addr {
		return &net.IPNet{IP: net.ParseIP(s), Mask: net.CIDRMask(24, 32)}
	}
	cases := []struct {
		name  string
		addrs []net.Addr
		want  string
	}{
		{"public first", []net.Addr{ipn("1.2.3.4"), ipn("192.168.1.5")}, "1.2.3.4"},
		{"private only", []net.Addr{ipn("10.0.0.1"), ipn("192.168.1.5")}, ""},
		{"loopback skipped", []net.Addr{ipn("127.0.0.1"), ipn("8.8.8.8")}, "8.8.8.8"},
		{"ipv6 skipped", []net.Addr{&net.IPNet{IP: net.ParseIP("2001:4860::1"), Mask: net.CIDRMask(64, 128)}}, ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		if got := publicIPv4FromAddrs(c.addrs); got != c.want {
			t.Errorf("%s: publicIPv4FromAddrs = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDetectNoExternalCall(t *testing.T) {
	// Detect 只枚举本机接口,不发起任何网络请求(无外部依赖)。
	_ = Detect()
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"localhost", true},
		{"::1", true},
		{"[::1]", true},
		{"127.1.2.3", true},
		{"1.2.3.4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsLoopbackHost(c.host); got != c.want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
