// Package network 描述网关的部署形态:检测本机网卡是否直连公网(零外部请求),
// 并提供出口地址(出口 IP + 映射端口)的 JSON 持久化。网关无法探测 NAT 后的
// 出口端口,故 NAT 部署下出口地址必须由用户填写。
package network

import (
	"net"
	"strings"
)

// Detection 是一次对本机网卡的一次性检测结果(仅接口地址,不调用任何外部服务)。
type Detection struct {
	DirectPublicIP string // 网卡直连的公网 IPv4(无 NAT);空表示 NAT/内网
	PrivateIP      string // 首个内网 IPv4(信息用,可能为空)
}

// Detect 枚举本机接口地址,返回直连公网 IPv4 与首个内网 IPv4。
func Detect() Detection {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return Detection{}
	}
	d := Detection{}
	for _, a := range addrs {
		ip := addrIP(a)
		if ip == nil || ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		switch {
		case IsPublicIP(ip):
			if d.DirectPublicIP == "" {
				d.DirectPublicIP = ip.String()
			}
		case IsPrivateIP(ip):
			if d.PrivateIP == "" {
				d.PrivateIP = ip.String()
			}
		}
	}
	return d
}

// addrIP 从 net.Addr 提取 IP;非 IP 网络(如 unix socket)返回 nil。
func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	return nil
}

// IsPublicIP 判断是否为公网 IPv4:排除回环、私有(RFC1918)、链路本地、
// 组播、未指定、CGNAT(100.64/10)与文档/示例网段。
func IsPublicIP(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false // 仅处理 IPv4(广告仅 IPv4)
	}
	if ip4.IsLoopback() || ip4.IsPrivate() || ip4.IsLinkLocalUnicast() ||
		ip4.IsMulticast() || ip4.IsUnspecified() {
		return false
	}
	// CGNAT 共享地址空间 100.64.0.0/10。
	if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return false
	}
	// 文档/示例网段 192.0.2.0/24、198.51.100.0/24、203.0.113.0/24。
	if (ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 2) ||
		(ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100) ||
		(ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113) {
		return false
	}
	return true
}

// IsPrivateIP 判断是否为内网 IPv4:RFC1918(10/8、172.16/12、192.168/16)或
// CGNAT 100.64/10。回环/链路本地不算(另由 IsPublicIP 排除)。
func IsPrivateIP(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
		return false
	}
	if ip4.IsPrivate() {
		return true // net.IP.IsPrivate 覆盖 RFC1918(以及 IPv6 ULA)
	}
	// CGNAT 共享地址空间。
	return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

// publicIPv4FromAddrs 从一组网络接口地址中找直连公网 IPv4(供测试注入合成地址)。
func publicIPv4FromAddrs(addrs []net.Addr) string {
	d := Detection{}
	for _, a := range addrs {
		ip := addrIP(a)
		if ip == nil || ip.IsLoopback() || ip.To4() == nil || !IsPublicIP(ip) {
			continue
		}
		d.DirectPublicIP = ip.String()
		break
	}
	return d.DirectPublicIP
}

// IsLoopbackHost 判断主机名/地址是否为回环(IP 为回环或 "localhost")。
func IsLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}
