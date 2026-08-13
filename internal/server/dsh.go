package server

import (
	"errors"
	"net/http"
	"strings"

	"BSRouter/internal/dsh"
)

// ---- DeepSeek Harness (dsh) 配置预设端点 ----

// handleAddDshPreset 新增预设(body 提供配置),返回掩码后的配置。
func (s *Server) handleAddDshPreset(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg dsh.Config
	if !decodeJSON(w, r, "dsh preset config", &cfg) {
		return
	}
	if strings.Contains(cfg.APIKey, "****") {
		writeError(w, http.StatusBadRequest, errors.New("api_key must not be a masked value"))
		return
	}
	if err := s.dsh.Add(cfg); err != nil {
		writeDshPresetError(w, err)
		return
	}
	stored, err := s.dsh.Get(cfg.Name)
	if err != nil {
		writeDshPresetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sanitizeDshPreset(stored))
}

// handleListDshPresets 返回全部预设(按名称排序,api_key 掩码)。
func (s *Server) handleListDshPresets(w http.ResponseWriter, r *http.Request) {
	cfgs := s.dsh.List()
	out := make([]dsh.Config, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, sanitizeDshPreset(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetDshPreset 返回单个预设(api_key 掩码);不存在时返回 404。
func (s *Server) handleGetDshPreset(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.dsh.Get(r.PathValue("name"))
	if err != nil {
		writeDshPresetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeDshPreset(cfg))
}

// handleUpdateDshPreset 修改预设;名称以路径为准,不存在时返回 404。
// 未提供新密钥或提交的是掩码值时,保留原密钥。
func (s *Server) handleUpdateDshPreset(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg dsh.Config
	if !decodeJSON(w, r, "dsh preset config", &cfg) {
		return
	}
	cfg.Name = r.PathValue("name")
	if existing, err := s.dsh.Get(cfg.Name); err == nil {
		if strings.Contains(cfg.APIKey, "****") && cfg.APIKey != maskKey(existing.APIKey) {
			writeError(w, http.StatusBadRequest, errors.New("api_key must not be a masked value"))
			return
		}
		if cfg.APIKey == "" || cfg.APIKey == maskKey(existing.APIKey) {
			cfg.APIKey = existing.APIKey
		}
	}
	if err := s.dsh.Update(cfg); err != nil {
		writeDshPresetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeDshPreset(cfg))
}

// handleDeleteDshPreset 删除预设;不存在时返回 404。
func (s *Server) handleDeleteDshPreset(w http.ResponseWriter, r *http.Request) {
	if err := s.dsh.Delete(r.PathValue("name")); err != nil {
		writeDshPresetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDshPresetCommand 返回预设对应的一键启动命令(PowerShell / bash):把 apiKeyEnv
// 指向的环境变量设为真实密钥,再启动 dsh harness。受 /manage 鉴权保护,响应不落日志。
func (s *Server) handleDshPresetCommand(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.dsh.Get(r.PathValue("name"))
	if err != nil {
		writeDshPresetError(w, err)
		return
	}
	if cfg.APIKey == "" {
		if dk := s.gatewayDefaultKey(); dk != "" {
			cfg.APIKey = dk
		}
	}
	cmd := dsh.BuildCommand(cfg)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        cfg.Name,
		"powershell":  cmd.PowerShell,
		"bash":        cmd.Bash,
		"api_key_env": cfg.EffectiveAPIKeyEnv(),
	})
}

// handleApplyDshPresetLocal 将预设覆盖到本地 ~/.dsh/settings.yaml 的
// llm-pi-ai.providers map(保留其它内置/自定义供应商与顶层字段)。仅当请求来自本机
// (回环地址)时允许;真实文件写入由后端完成。未配置密钥的预设注入系统默认 key。
// 生效模型列表 = 预设配置的 Models,留空回退网关全部可路由模型;仍为空(网关无模型)
// 时 400 拒绝覆盖。baseURL 留空派生网关统一 API 入口(http://127.0.0.1:<端口>/api)。
func (s *Server) handleApplyDshPresetLocal(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeError(w, http.StatusForbidden, errors.New("apply-local requires accessing the gateway from the local machine (127.0.0.1 / localhost)"))
		return
	}
	cfg, err := s.dsh.Get(r.PathValue("name"))
	if err != nil {
		writeDshPresetError(w, err)
		return
	}
	if cfg.APIKey == "" {
		if dk := s.gatewayDefaultKey(); dk != "" {
			cfg.APIKey = dk
		}
	}
	// 生效模型列表:预设直接配置的 models 优先;留空回退网关全部可路由模型。
	models := cfg.Models
	if len(models) == 0 {
		models = s.allModelIDs()
	}
	if len(models) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no models configured; add models to the dsh preset or to providers.json before applying local dsh config"))
		return
	}

	// 与 zcode 一致自动填充:display_name/接口格式/环境变量名/入口均由预设名与网关派生。
	spec := dsh.ProviderSpec{
		Name:        cfg.Name,
		DisplayName: cfg.Name,
		APIKey:      cfg.APIKey,
		APIKeyEnv:   cfg.EffectiveAPIKeyEnv(),
		API:         dsh.DefaultAPI,
		BaseURL:     s.defaultDshOrigin() + "/api",
		Models:      models,
		Windows:     s.modelContextWindows(models),
	}
	path := s.dshSettingsPath
	if path == "" {
		path, err = dsh.DefaultSettingsPath()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := dsh.ApplyToLocalSettings(path, spec); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied":     true,
		"path":        path,
		"provider":    cfg.Name,
		"api":         dsh.DefaultAPI,
		"api_key_env": cfg.EffectiveAPIKeyEnv(),
		"models":      len(models),
	})
}

// defaultDshOrigin 返回网关本机监听入口的主机头(本地 http://127.0.0.1:<端口>),
// 供统一 API 入口派生 base_url(dsh 写本机配置,保持回环)。
func (s *Server) defaultDshOrigin() string {
	port := "18154"
	if s.deployment != nil && s.deployment.ListenPort != "" {
		port = s.deployment.ListenPort
	}
	return "http://127.0.0.1:" + port
}

// writeDshPresetError 映射管理端点错误状态:不存在 -> 404,已存在 -> 409,
// 持久化失败 -> 500,其余(配置非法)-> 400。
func writeDshPresetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dsh.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, dsh.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, dsh.ErrPersist):
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// sanitizeDshPreset 掩码 api_key,其余字段原样。
func sanitizeDshPreset(cfg dsh.Config) dsh.Config {
	cfg.APIKey = maskKey(cfg.APIKey)
	return cfg
}
