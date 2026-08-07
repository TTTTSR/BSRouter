package server

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"BSRouter/internal/network"
)

// 部署形态标识。
const (
	ModeLocal  = "local"  // 绑定回环,本地部署
	ModeDirect = "direct" // 绑定非回环且网卡直连公网 IP,可自动广告
	ModeNAT    = "nat"    // 绑定非回环但无直连公网 IP,出口地址需用户填写
)

// Deployment 是网关的部署形态,由 cmd/gateway 在启动时判定并注入。
type Deployment struct {
	Mode           string // ModeLocal / ModeDirect / ModeNAT
	ListenPort     string // -addr 的监听端口
	DirectPublicIP string // 直连公网 IPv4(direct 模式;显式绑定公网 IP 时为该 IP)
	PublicAddr     string // -public-addr 手工覆盖(完整 base URL,最高优先)
}

// effectiveBase 返回生效的广告 base URL(去掉尾部 "/")。优先级:
// -public-addr(flag)> 用户配置的出口(持久化)> direct 模式自动广告(公网IP:监听端口)。
// 返回空串表示当前没有可用出口地址(local 部署或 nat 未配置)。
func (s *Server) effectiveBase() string {
	d := s.deployment
	if d == nil {
		return ""
	}
	if base := strings.TrimRight(d.PublicAddr, "/"); base != "" {
		return base
	}
	if s.netm != nil {
		if c := s.netm.Get(); c.EgressHost != "" {
			base := "http://" + c.EgressHost
			if c.EgressPort != "" {
				base += ":" + c.EgressPort
			}
			return base
		}
	}
	if d.Mode == ModeDirect && d.DirectPublicIP != "" {
		return "http://" + net.JoinHostPort(d.DirectPublicIP, d.ListenPort)
	}
	return ""
}

// remote 判断网关是否处于远程部署(后端不在用户本机)。
func (s *Server) remote() bool {
	d := s.deployment
	if d == nil {
		return false
	}
	return d.Mode != ModeLocal || d.PublicAddr != ""
}

// effectiveBaseURL 把指向本机回环地址的 base_url 替换为网关对外广告地址:
// 仅当广告地址非空且原 host 为回环时替换(保留 path/query),其余原样。
func (s *Server) effectiveBaseURL(base string) string {
	advertised := s.effectiveBase()
	if advertised == "" {
		return base
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return base
	}
	if !network.IsLoopbackHost(u.Hostname()) {
		return base
	}
	out := advertised
	if u.Path != "" {
		out += u.Path
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// networkStatus 是 /manage/v1/network 的响应形态。
type networkStatus struct {
	Remote         bool   `json:"remote"`
	Mode           string `json:"mode"`
	DirectPublicIP string `json:"direct_public_ip,omitempty"`
	EgressHost     string `json:"egress_host,omitempty"`
	EgressPort     string `json:"egress_port,omitempty"`
	AdvertisedBase string `json:"advertised_base,omitempty"`
}

// handleGetNetwork 返回网关部署形态与出口地址状态。
func (s *Server) handleGetNetwork(w http.ResponseWriter, r *http.Request) {
	st := s.networkStatus()
	writeJSON(w, http.StatusOK, st)
}

// handleSetNetwork 设置出口地址(body {egress_host, egress_port}),持久化。
func (s *Server) handleSetNetwork(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg network.Config
	if !decodeJSON(w, r, "network config", &cfg) {
		return
	}
	if err := s.netm.Set(cfg); err != nil {
		if errors.Is(err, network.ErrPersist) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.networkStatus())
}

// networkStatus 组装当前网络状态响应。
func (s *Server) networkStatus() networkStatus {
	st := networkStatus{
		Remote: s.remote(),
		Mode:   "local",
	}
	if s.deployment != nil {
		st.Mode = s.deployment.Mode
		st.DirectPublicIP = s.deployment.DirectPublicIP
	}
	if s.netm != nil {
		if c := s.netm.Get(); c.EgressHost != "" {
			st.EgressHost = c.EgressHost
			st.EgressPort = c.EgressPort
		}
	}
	st.AdvertisedBase = s.effectiveBase()
	return st
}
