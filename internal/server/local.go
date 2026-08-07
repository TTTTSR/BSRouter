package server

import (
	"net"
	"net/http"
	"strings"
)

// isLocalRequest 判断请求是否来自本机回环地址(localhost / 127.0.0.1 / ::1)。
// 用于门控"覆盖本地 Claude Code 配置"等需要访问本机文件的本地能力:仅当网关与
// 客户端在同一台机器时,网关才能写客户端用户的 ~/.claude/settings.json。
func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	// Host 头为 localhost / ::1 也视为本地(覆盖经 localhost 访问的场景)。
	h, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		h = r.Host
	}
	h = strings.Trim(h, "[]")
	return strings.EqualFold(h, "localhost") || h == "::1"
}

// handleLocalStatus 返回当前请求是否来自本机,供前端决定是否启用本地配置覆盖功能。
func (s *Server) handleLocalStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"local": isLocalRequest(r)})
}
