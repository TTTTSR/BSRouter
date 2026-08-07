package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"BSRouter/internal/claude"
	"BSRouter/internal/network"
)

// ---- Claude Code 配置预设端点 ----

// handleAddClaudePreset 新增预设(body 提供配置),返回掩码后的配置。
func (s *Server) handleAddClaudePreset(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg claude.Config
	if !decodeJSON(w, r, "claude preset config", &cfg) {
		return
	}
	// 新增时没有可保留的原密钥,提交掩码值属于复制粘贴错误,直接拒绝。
	if strings.Contains(cfg.APIKey, "****") || strings.Contains(cfg.AuthToken, "****") {
		writeError(w, http.StatusBadRequest, errors.New("api_key / auth_token must not be a masked value"))
		return
	}
	if err := s.presets.Add(cfg); err != nil {
		writeClaudePresetError(w, err)
		return
	}
	// 返回存储后的配置(含 Add 设置的 CreatedAt)。
	stored, err := s.presets.Get(cfg.Name)
	if err != nil {
		writeClaudePresetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sanitizeClaudePreset(stored))
}

// handleListClaudePresets 返回全部预设(按名称排序,api_key/auth_token 掩码)。
func (s *Server) handleListClaudePresets(w http.ResponseWriter, r *http.Request) {
	cfgs := s.presets.List()
	out := make([]claude.Config, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, sanitizeClaudePreset(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetClaudePreset 返回单个预设(密钥掩码);不存在时返回 404。
func (s *Server) handleGetClaudePreset(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.presets.Get(r.PathValue("name"))
	if err != nil {
		writeClaudePresetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeClaudePreset(cfg))
}

// handleUpdateClaudePreset 修改预设;名称以路径为准,不存在时返回 404。
// 未提供新密钥或提交的是掩码值时,保留原密钥(api_key 与 auth_token 各自独立)。
func (s *Server) handleUpdateClaudePreset(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg claude.Config
	if !decodeJSON(w, r, "claude preset config", &cfg) {
		return
	}
	cfg.Name = r.PathValue("name")
	if existing, err := s.presets.Get(cfg.Name); err == nil {
		if cfg.APIKey == "" || cfg.APIKey == maskKey(existing.APIKey) {
			cfg.APIKey = existing.APIKey
		}
		if cfg.AuthToken == "" || cfg.AuthToken == maskKey(existing.AuthToken) {
			cfg.AuthToken = existing.AuthToken
		}
	}
	if err := s.presets.Update(cfg); err != nil {
		writeClaudePresetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeClaudePreset(cfg))
}

// handleDeleteClaudePreset 删除预设;不存在时返回 404。
func (s *Server) handleDeleteClaudePreset(w http.ResponseWriter, r *http.Request) {
	if err := s.presets.Delete(r.PathValue("name")); err != nil {
		writeClaudePresetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// claudeDefaultKeyName 是网关为 Claude 预设自动生成的默认受管 key 名称。
const claudeDefaultKeyName = "claude-default"

// claudeDefaultKey 返回 Claude 命令的默认鉴权 key,供未配置密钥的预设使用:
// 优先复用/生成受管 key claudeDefaultKeyName(在 /api 上被接受);
// 未启用受管 key 时回退网关 key;两者都不可用返回空(/api 完全开放,命令无需鉴权)。
// 懒生成:首次调用时创建并持久化到 keys.json,对客户端透明。
func (s *Server) claudeDefaultKey() string {
	if s.keys != nil {
		if k, err := s.keys.Get(claudeDefaultKeyName); err == nil {
			return k.Key
		}
		if k, err := s.keys.Generate(claudeDefaultKeyName); err == nil {
			return k.Key
		}
		// 并发下 Generate 可能返回 ErrExists,回退读取。
		if k, err := s.keys.Get(claudeDefaultKeyName); err == nil {
			return k.Key
		}
	}
	if s.apiKey != "" {
		return s.apiKey
	}
	return ""
}

// handleClaudePresetCommand 返回预设对应的一键启动命令(PowerShell / bash),
// 命令中嵌入真实密钥。预设未配置密钥时注入系统默认 key,用户无需在 Claude
// 配置中考虑鉴权。远程部署时,指向本机回环地址的 base_url 会被替换为网关对外
// 广告地址(动态派生,不改存储);NAT 部署且未配置出口地址时返回 warning 提醒。
// 该端点受 /manage 鉴权保护;真实密钥不落日志中间件
// (日志仅记录管理端点的路径与状态,不捕获响应体)。
func (s *Server) handleClaudePresetCommand(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.presets.Get(r.PathValue("name"))
	if err != nil {
		writeClaudePresetError(w, err)
		return
	}
	// 远程部署下替换回环 base_url(仅影响本命令,存储保持原样)。
	var warning string
	if s.effectiveBase() != "" {
		cfg.BaseURL = s.effectiveBaseURL(cfg.BaseURL)
	} else if s.remote() && isLoopbackBaseURL(cfg.BaseURL) {
		// NAT/远程部署但未配置出口地址:命令里仍是本机地址,远端连不上,提醒用户。
		warning = "当前为远程/NAT 部署且未配置出口地址,此命令可能无法从远端生效;请在 Claude 预设页填写出口 IP 与映射端口"
	}
	if cfg.APIKey == "" && cfg.AuthToken == "" {
		if dk := s.claudeDefaultKey(); dk != "" {
			cfg.AuthToken = dk
		}
	}
	cmd := claude.BuildCommand(cfg)
	out := map[string]any{
		"name":       cfg.Name,
		"powershell": cmd.PowerShell,
		"bash":       cmd.Bash,
	}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(w, http.StatusOK, out)
}

// isLoopbackBaseURL 判断 base_url 的 host 是否为回环地址(127.x / ::1 / localhost)。
func isLoopbackBaseURL(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return false
	}
	return network.IsLoopbackHost(u.Hostname())
}

// handleApplyClaudePresetLocal 将预设覆盖到本地 Claude Code settings.json 的 env 块。
// 仅当请求来自本机(回环地址)时允许;真实文件写入由后端完成(浏览器无法写本地文件)。
// 未配置密钥的预设注入系统默认 key(与命令端点一致)。
func (s *Server) handleApplyClaudePresetLocal(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeError(w, http.StatusForbidden, errors.New("apply-local requires accessing the gateway from the local machine (127.0.0.1 / localhost)"))
		return
	}
	cfg, err := s.presets.Get(r.PathValue("name"))
	if err != nil {
		writeClaudePresetError(w, err)
		return
	}
	if cfg.APIKey == "" && cfg.AuthToken == "" {
		if dk := s.claudeDefaultKey(); dk != "" {
			cfg.AuthToken = dk
		}
	}
	path := s.claudeSettingsPath
	if path == "" {
		path, err = claude.DefaultSettingsPath()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := claude.ApplyToLocalSettings(path, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": true, "path": path})
}

// writeClaudePresetError 映射管理端点错误状态:不存在 -> 404,已存在 -> 409,
// 持久化失败 -> 500,其余(配置非法)-> 400。
func writeClaudePresetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, claude.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, claude.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, claude.ErrPersist):
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// sanitizeClaudePreset 掩码 api_key 与 auth_token,其余字段原样。
func sanitizeClaudePreset(cfg claude.Config) claude.Config {
	cfg.APIKey = maskKey(cfg.APIKey)
	cfg.AuthToken = maskKey(cfg.AuthToken)
	return cfg
}
