package server

import (
	"errors"
	"net/http"
	"strings"

	"BSRouter/internal/provider"
	"BSRouter/internal/zcode"
)

// ---- Z.ai zcode 配置预设端点 ----

// handleAddZcodePreset 新增预设(body 提供配置),返回掩码后的配置。
func (s *Server) handleAddZcodePreset(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg zcode.Config
	if !decodeJSON(w, r, "zcode preset config", &cfg) {
		return
	}
	// 新增时没有可保留的原密钥,提交掩码值属于复制粘贴错误,直接拒绝。
	if strings.Contains(cfg.APIKey, "****") {
		writeError(w, http.StatusBadRequest, errors.New("api_key must not be a masked value"))
		return
	}
	if err := s.zcode.Add(cfg); err != nil {
		writeZcodePresetError(w, err)
		return
	}
	stored, err := s.zcode.Get(cfg.Name)
	if err != nil {
		writeZcodePresetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sanitizeZcodePreset(stored))
}

// handleListZcodePresets 返回全部预设(按名称排序,api_key 掩码)。
func (s *Server) handleListZcodePresets(w http.ResponseWriter, r *http.Request) {
	cfgs := s.zcode.List()
	out := make([]zcode.Config, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, sanitizeZcodePreset(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetZcodePreset 返回单个预设(api_key 掩码);不存在时返回 404。
func (s *Server) handleGetZcodePreset(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.zcode.Get(r.PathValue("name"))
	if err != nil {
		writeZcodePresetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeZcodePreset(cfg))
}

// handleUpdateZcodePreset 修改预设;名称以路径为准,不存在时返回 404。
// 未提供新密钥或提交的是掩码值时,保留原密钥。
func (s *Server) handleUpdateZcodePreset(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg zcode.Config
	if !decodeJSON(w, r, "zcode preset config", &cfg) {
		return
	}
	cfg.Name = r.PathValue("name")
	if existing, err := s.zcode.Get(cfg.Name); err == nil {
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
	if err := s.zcode.Update(cfg); err != nil {
		writeZcodePresetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeZcodePreset(cfg))
}

// handleDeleteZcodePreset 删除预设;不存在时返回 404。
func (s *Server) handleDeleteZcodePreset(w http.ResponseWriter, r *http.Request) {
	if err := s.zcode.Delete(r.PathValue("name")); err != nil {
		writeZcodePresetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleApplyZcodePresetLocal 将预设覆盖到本地 ~/.zcode/v2/config.json 的
// provider map(bsrouter* 系列供应商,保留其余内置/自定义供应商与顶层字段)。
// 仅当请求来自本机(回环地址)时允许;真实文件写入由后端完成。未配置密钥的预设
// 注入系统默认 key(与 Claude/Codex 预设一致)。所有请求走网关统一 API 入口,
// apply-local 按模型原生接口格式自动分割为多个供应商(openai-compatible /
// anthropic / responses,见 zcodeProviderSpecs)。zcode 的模型列表手动配置在
// config.json,apply-local 必须写入:生效模型列表 = 预设配置的 models,留空回退
// 网关全部可路由模型;仍为空(网关无模型)时 400 拒绝覆盖,避免写坏已有模型列表。
// zcode 无环境变量注入的启动命令(鉴权在 config.json 里),故不提供一键命令端点,
// apply-local 即覆盖机制。
func (s *Server) handleApplyZcodePresetLocal(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeError(w, http.StatusForbidden, errors.New("apply-local requires accessing the gateway from the local machine (127.0.0.1 / localhost)"))
		return
	}
	cfg, err := s.zcode.Get(r.PathValue("name"))
	if err != nil {
		writeZcodePresetError(w, err)
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
		// 拒绝用空列表覆盖已有供应商条目:预设未配置模型且网关供应商无模型时,
		// 写空 models 会破坏 zcode 已有模型列表,且无法回滚。
		writeError(w, http.StatusBadRequest, errors.New("no models configured; add models to the zcode preset or to providers.json before applying local zcode config"))
		return
	}
	windows := s.modelContextWindows(models)
	specs := s.zcodeProviderSpecs(models, windows)
	path := s.zcodeConfigPath
	if path == "" {
		path, err = zcode.DefaultConfigPath()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := zcode.ApplyToLocalConfig(path, cfg.APIKey, specs); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": true, "path": path, "models": len(models), "providers": len(specs)})
}

// defaultZcodeOrigin 返回网关本机监听入口的主机头(本地 http://127.0.0.1:<端口>),
// 供统一 API 入口派生 base_url(zcode 写本机配置,保持回环)。
func (s *Server) defaultZcodeOrigin() string {
	port := "18154"
	if s.deployment != nil && s.deployment.ListenPort != "" {
		port = s.deployment.ListenPort
	}
	return "http://127.0.0.1:" + port
}

// zcodeProviderSpecs 由生效模型列表生成要写入的 zcode 供应商集合:全部走网关统一 API
// 入口(根 /api),按模型原生接口格式分割为多个供应商(见 zcodeProvidersForEntry)。
func (s *Server) zcodeProviderSpecs(models []string, windows map[string]int) []zcode.ProviderSpec {
	return s.zcodeProvidersForEntry(s.defaultZcodeOrigin()+"/api", models, windows)
}

// zcodeProvidersForEntry 在统一 API 入口根 root 下把模型按原生接口格式分割为多个
// zcode 供应商,让 zcode 里每种格式的模型都走原生 wire 连接:
// completion → ProviderNameOpenAI(openai-compatible,root+/v1);
// anthropic → ProviderNameAnthropic(anthropic,root 不带 /v1);
// responses → ProviderNameResponses(openai-compatible + wire_api=responses,root+/v1)。
// 某格式无模型时不建该供应商。
func (s *Server) zcodeProvidersForEntry(root string, models []string, windows map[string]int) []zcode.ProviderSpec {
	var openai, anthropic, responses []string
	for _, m := range models {
		switch s.modelFormat(m) {
		case provider.KindAnthropic:
			anthropic = append(anthropic, m)
		case provider.KindResponses:
			responses = append(responses, m)
		default:
			openai = append(openai, m)
		}
	}
	apiV1 := root + "/v1"
	specs := make([]zcode.ProviderSpec, 0, 3)
	if len(openai) > 0 {
		specs = append(specs, zcode.ProviderSpec{Name: zcode.ProviderNameOpenAI, Kind: zcode.DefaultKind, BaseURL: apiV1, Models: openai, Windows: windows})
	}
	if len(anthropic) > 0 {
		specs = append(specs, zcode.ProviderSpec{Name: zcode.ProviderNameAnthropic, Kind: zcode.KindAnthropic, BaseURL: root, Models: anthropic, Windows: windows})
	}
	if len(responses) > 0 {
		specs = append(specs, zcode.ProviderSpec{Name: zcode.ProviderNameResponses, Kind: zcode.DefaultKind, WireAPI: zcode.WireAPIResponses, BaseURL: apiV1, Models: responses, Windows: windows})
	}
	return specs
}

// modelFormat 返回模型 id 的原生接口格式:合成 "{供应商}@{模型}" → 供应商模型的
// 主格式(ModelKind);聚合裸名 → 首个有效成员的同名模型主格式;无法解析回退 completion。
func (s *Server) modelFormat(id string) provider.Kind {
	name, rest, ok := strings.Cut(provider.StripContextMarker(id), "@")
	if !ok || rest == "" {
		// 聚合裸名:取首个有效成员供应商的同名模型主格式。
		if s.aggregates != nil {
			if members, ok := s.aggregates.Members(name); ok {
				for _, p := range members {
					if prov, _, err := s.mgr.Resolve(p + "@" + name); err == nil {
						return prov.ModelKind(name)
					}
				}
			}
		}
		return provider.KindCompletion
	}
	prov, resolved, err := s.mgr.Resolve(id)
	if err != nil {
		return provider.KindCompletion
	}
	return prov.ModelKind(resolved)
}

// writeZcodePresetError 映射管理端点错误状态:不存在 -> 404,已存在 -> 409,
// 持久化失败 -> 500,其余(配置非法)-> 400。
func writeZcodePresetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, zcode.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, zcode.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, zcode.ErrPersist):
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// sanitizeZcodePreset 掩码 api_key,其余字段原样。
func sanitizeZcodePreset(cfg zcode.Config) zcode.Config {
	cfg.APIKey = maskKey(cfg.APIKey)
	return cfg
}
