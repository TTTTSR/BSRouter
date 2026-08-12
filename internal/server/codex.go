package server

import (
	"errors"
	"net/http"
	"strings"

	"BSRouter/internal/codex"
)

// ---- OpenAI Codex 配置预设端点 ----

// handleListCodexNativeSlugs 返回支持接管的原生 OpenAI 模型 id(供前端 native-alias
// 表单选择;与 codex.NativeOpenAISlugs 同源)。
func (s *Server) handleListCodexNativeSlugs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"slugs": codex.NativeOpenAISlugs()})
}

// handleAddCodexPreset 新增预设(body 提供配置),返回掩码后的配置。
func (s *Server) handleAddCodexPreset(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg codex.Config
	if !decodeJSON(w, r, "codex preset config", &cfg) {
		return
	}
	// 新增时没有可保留的原密钥,提交掩码值属于复制粘贴错误,直接拒绝。
	if strings.Contains(cfg.APIKey, "****") {
		writeError(w, http.StatusBadRequest, errors.New("api_key must not be a masked value"))
		return
	}
	if err := s.codex.Add(cfg); err != nil {
		writeCodexPresetError(w, err)
		return
	}
	stored, err := s.codex.Get(cfg.Name)
	if err != nil {
		writeCodexPresetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sanitizeCodexPreset(stored))
}

// handleListCodexPresets 返回全部预设(按名称排序,api_key 掩码)。
func (s *Server) handleListCodexPresets(w http.ResponseWriter, r *http.Request) {
	cfgs := s.codex.List()
	out := make([]codex.Config, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, sanitizeCodexPreset(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetCodexPreset 返回单个预设(api_key 掩码);不存在时返回 404。
func (s *Server) handleGetCodexPreset(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.codex.Get(r.PathValue("name"))
	if err != nil {
		writeCodexPresetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeCodexPreset(cfg))
}

// handleUpdateCodexPreset 修改预设;名称以路径为准,不存在时返回 404。
// 未提供新密钥或提交的是掩码值时,保留原密钥。
func (s *Server) handleUpdateCodexPreset(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg codex.Config
	if !decodeJSON(w, r, "codex preset config", &cfg) {
		return
	}
	cfg.Name = r.PathValue("name")
	if existing, err := s.codex.Get(cfg.Name); err == nil {
		// 提交的掩码占位既不是新密钥,也不是对原密钥的合法回显(如原密钥为空
		// 或掩码不匹配)时直接拒绝,避免把字面 "****" 或错误掩码存成真实密钥。
		if strings.Contains(cfg.APIKey, "****") && cfg.APIKey != maskKey(existing.APIKey) {
			writeError(w, http.StatusBadRequest, errors.New("api_key must not be a masked value"))
			return
		}
		if cfg.APIKey == "" || cfg.APIKey == maskKey(existing.APIKey) {
			cfg.APIKey = existing.APIKey
		}
	}
	if err := s.codex.Update(cfg); err != nil {
		writeCodexPresetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeCodexPreset(cfg))
}

// handleDeleteCodexPreset 删除预设;不存在时返回 404。
func (s *Server) handleDeleteCodexPreset(w http.ResponseWriter, r *http.Request) {
	if err := s.codex.Delete(r.PathValue("name")); err != nil {
		writeCodexPresetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCodexPresetCommand 返回预设对应的一键启动命令(PowerShell / bash),
// 命令中嵌入真实密钥。预设未配置密钥时注入系统默认 key(与 Claude 预设同一
// 机制:gatewayDefaultKey)。远程部署时,指向本机回环地址的 base_url 会被替换为
// 网关对外广告地址(动态派生,不改存储);NAT 部署且未配置出口地址时返回 warning。
// 该端点受 /manage 鉴权保护;真实密钥不落日志中间件。
func (s *Server) handleCodexPresetCommand(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.codex.Get(r.PathValue("name"))
	if err != nil {
		writeCodexPresetError(w, err)
		return
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = s.defaultCodexBaseURL()
	}
	// 远程部署下替换回环 base_url(仅影响本命令,存储保持原样)。
	var warning string
	if s.effectiveBase() != "" {
		cfg.BaseURL = s.effectiveBaseURL(cfg.BaseURL)
	} else if s.remote() && isLoopbackBaseURL(cfg.BaseURL) {
		warning = "当前为远程/NAT 部署且未配置出口地址,此命令可能无法从远端生效;请在 Codex 预设页填写出口 IP 与映射端口"
	}
	if cfg.APIKey == "" {
		if dk := s.gatewayDefaultKey(); dk != "" {
			cfg.APIKey = dk
		}
	}
	cmd := codex.BuildCommand(cfg)
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

// handleApplyCodexPresetLocal 将预设覆盖到本地 ~/.codex/config.toml(单一
// bsrouter 块)+ ~/.codex/auth.json(把密钥写入 OPENAI_API_KEY,codex 借此跳过
// ChatGPT 登录直接用网关 key)。仅当请求来自本机(回环地址)时允许;真实文件写入
// 由后端完成。未配置密钥的预设注入系统默认 key(与命令端点一致)。
func (s *Server) handleApplyCodexPresetLocal(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeError(w, http.StatusForbidden, errors.New("apply-local requires accessing the gateway from the local machine (127.0.0.1 / localhost)"))
		return
	}
	cfg, err := s.codex.Get(r.PathValue("name"))
	if err != nil {
		writeCodexPresetError(w, err)
		return
	}
	if cfg.APIKey == "" {
		if dk := s.gatewayDefaultKey(); dk != "" {
			cfg.APIKey = dk
		}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = s.defaultCodexBaseURL()
	}
	path := s.codexConfigPath
	if path == "" {
		path, err = codex.DefaultConfigPath()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	// 生成模型目录(预设直接配置的模型列表,每个模型 + 自动分配的裸原生 slug 行),
	// 让 codex 列出真实模型:
	// - bsrouter-models.json(model_catalog_json 格式,CLI/TUI 读)
	// - models_cache.json(桌面 app 读)
	models := s.effectiveModels(cfg)
	if len(models) == 0 {
		// 拒绝用空列表覆盖已有模型目录:预设未配置模型且网关供应商无模型时,
		// 写空文件会破坏 codex 已有模型列表,且无法回滚。
		writeError(w, http.StatusBadRequest, errors.New("no models configured; add models to the codex preset or to providers.json before applying local codex config"))
		return
	}
	// 每个模型按供应商配置的上下文窗口(k)换算成 tokens,目录条目据此同步窗口。
	windows := s.modelContextWindows(models)
	catPath := s.codexModelCatalogPath
	if catPath == "" {
		catPath, err = codex.DefaultModelCatalogPath()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := codex.ApplyToLocalModelCatalog(catPath, models, windows); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cachePath := s.codexModelsCachePath
	if cachePath == "" {
		cachePath, err = codex.DefaultModelsCachePath()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := codex.ApplyToLocalModelsCache(cachePath, models, windows); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// config.toml 的 model_catalog_json 用绝对路径(相对路径 codex 可能解析不到)。
	if err := codex.ApplyToLocalConfig(path, cfg, catPath); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 写 auth.json:把密钥放进 OPENAI_API_KEY,codex 无需 ChatGPT 登录。
	authPath := s.codexAuthPath
	if authPath == "" {
		authPath, err = codex.DefaultAuthPath()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := codex.ApplyToLocalAuth(authPath, cfg.APIKey); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied":       true,
		"path":          path,
		"auth_path":     authPath,
		"model_catalog":  catPath,
		"models_cache":  cachePath,
	})
}

// allModelIDs 聚合网关全部可路由模型 id:各供应商模型合成 "{供应商}@{模型}" +
// 聚合裸名。复用 allModelEntries(/api/v1/models 同源),保证 codex 模型目录与
// 网关模型列表永远一致。
func (s *Server) allModelIDs() []string {
	entries := s.allModelEntries()
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// writeCodexPresetError 映射管理端点错误状态:不存在 -> 404,已存在 -> 409,
// 持久化失败 -> 500,其余(配置非法)-> 400。
func writeCodexPresetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, codex.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, codex.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, codex.ErrPersist):
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// sanitizeCodexPreset 掩码 api_key,其余字段原样。
func sanitizeCodexPreset(cfg codex.Config) codex.Config {
	cfg.APIKey = maskKey(cfg.APIKey)
	return cfg
}
