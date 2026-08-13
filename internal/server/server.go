// Package server 提供网关 HTTP 服务:三个转发端点分别接收 Anthropic /
// chat.completions / responses 三种接口格式的请求,按 "{供应商名}-{模型名}"
// 路由到对应供应商,并以请求的格式返回响应;同时提供供应商增删改查管理端点
// 与聚合所有供应商模型的模型列表端点。
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"BSRouter/internal/aggregate"
	"BSRouter/internal/apikey"
	"BSRouter/internal/claude"
	"BSRouter/internal/codex"
	"BSRouter/internal/fault"
	"BSRouter/internal/gateway"
	"BSRouter/internal/group"
	"BSRouter/internal/logger"
	"BSRouter/internal/network"
	"BSRouter/internal/provider"
	"BSRouter/internal/providertemplates"
	"BSRouter/internal/zcode"
)

// maxBodyBytes 限制入站请求体大小,防止未鉴权的内存耗尽。
const maxBodyBytes = 10 << 20 // 10 MB

// Server 是网关 HTTP 服务。
type Server struct {
	mgr        *provider.Manager
	groups     *group.Manager
	keys       *apikey.Manager
	presets    *claude.Manager
	codex      *codex.Manager
	zcode      *zcode.Manager
	aggregates *aggregate.Manager
	faults     *fault.Manager
	apiKey     string
	log        *logger.Logger
	webUI      http.Handler
	// logDetail 日志完整度:"default" 仅出错时记录完整转发详情;"full" 全部记录。
	// logDetailMu 保护运行时经管理端点修改;logDetailPath 持久化文件(可选)。
	logDetail     string
	logDetailMu   sync.RWMutex
	logDetailPath string
	// claudeSettingsPath 覆盖本地 Claude Code 配置的目标 settings.json 路径;
	// 留空时用 ~/.claude/settings.json(仅测试/自定义使用)。
	claudeSettingsPath string
	// codexConfigPath 覆盖本地 Codex 配置的目标 config.toml 路径;
	// 留空时用 ~/.codex/config.toml(仅测试/自定义使用)。
	codexConfigPath string
	// codexAuthPath 覆盖本地 Codex 鉴权文件的目标 auth.json 路径;
	// 留空时用 ~/.codex/auth.json(仅测试/自定义使用)。
	codexAuthPath string
	// codexModelCatalogPath 覆盖本地 Codex 模型目录文件的目标路径;
	// 留空时用 ~/.codex/bsrouter-models.json(仅测试/自定义使用)。
	codexModelCatalogPath string
	// codexModelsCachePath 覆盖本地 Codex 模型缓存文件的目标路径;
	// 留空时用 ~/.codex/models_cache.json(桌面 app 的模型列表来源)。
	codexModelsCachePath string
	// zcodeConfigPath 覆盖本地 zcode 配置的目标 config.json 路径;
	// 留空时用 ~/.zcode/v2/config.json(仅测试/自定义使用)。
	zcodeConfigPath string
	// deployment 部署形态(由 cmd/gateway 启动时判定);netm 是出口地址配置。
	deployment *Deployment
	netm       *network.Manager
	// streamIdleTimeout 上游流式响应体 idle 超时(两字节到达间隔超过即中止,0=禁用);
	// 经 WithStreamIdleTimeout 注入,直通与转换路径生效。
	streamIdleTimeout time.Duration
	// streamRetries 流开始前失败(请求发送错误 / 上游非 2xx)的每成员重试次数,0=禁用。
	// 由 cmd/gateway 经 WithStreamRetries 显式启用(默认 2),测试用 New(m) 构造时保持 0 不重试。
	streamRetries int
}

// New 基于供应商注册表构造网关服务。
func New(mgr *provider.Manager) *Server {
	return &Server{mgr: mgr}
}

// WithGroups 启用模型分组:每个分组作为虚拟供应商挂载到自己的 URL。
// 需在 Handler() 之前调用。
func (s *Server) WithGroups(gm *group.Manager) *Server {
	s.groups = gm
	return s
}

// WithAPIKey 启用 API Key 鉴权:所有端点都需携带匹配的 key 才能访问。
// key 会去掉首尾空白(与请求侧提取逻辑保持一致)。key 为空时等效不鉴权
// (仅用于测试/嵌入场景;cmd/gateway 会在启动时强制非空)。需在 Handler() 之前调用。
func (s *Server) WithAPIKey(key string) *Server {
	s.apiKey = strings.TrimSpace(key)
	return s
}

// WithLogger 启用请求日志:所有请求(含被鉴权拒绝的)都会以 JSONL 追加写入。
// 需在 Handler() 之前调用。
func (s *Server) WithLogger(l *logger.Logger) *Server {
	s.log = l
	return s
}

// WithLogDetail 设置日志完整度(启动默认值):"default" 仅出错记录完整转发详情,
// "full" 全部记录。管理端点可运行时修改并持久化。
func (s *Server) WithLogDetail(detail string) *Server {
	if detail == LogDetailFull {
		s.logDetail = LogDetailFull
	} else {
		s.logDetail = LogDetailDefault
	}
	return s
}

// WithLogDetailPath 设置日志完整度持久化文件路径(管理端点 PUT 时写回)。
func (s *Server) WithLogDetailPath(path string) *Server {
	s.logDetailPath = path
	return s
}

// WithStreamIdleTimeout 设置上游流式响应体的 idle 超时(两字节到达间隔超过该值即中止,
// 0=禁用)。经请求上下文注入,转换与直通、统一 API 与分组路径同时生效。
func (s *Server) WithStreamIdleTimeout(d time.Duration) *Server {
	s.streamIdleTimeout = d
	return s
}

// WithStreamRetries 设置流开始前失败的每成员重试次数(0=禁用;仅对流开始前的
// 请求发送错误 / 上游非 2xx 生效,流中途失败不重试)。
func (s *Server) WithStreamRetries(n int) *Server {
	s.streamRetries = n
	return s
}

// WithAPIKeys 启用下游模型请求的受管 API Key 鉴权(/api 端点额外接受受管 Key)。
// 需在 Handler() 之前调用。
func (s *Server) WithAPIKeys(km *apikey.Manager) *Server {
	s.keys = km
	return s
}

// WithClaudePresets 启用 Claude Code 配置预设管理(/manage/v1/claude-presets)。
// 需在 Handler() 之前调用。
func (s *Server) WithClaudePresets(cm *claude.Manager) *Server {
	s.presets = cm
	return s
}

// WithCodexPresets 启用 OpenAI Codex 配置预设管理(/manage/v1/codex-presets)。
// 需在 Handler() 之前调用。
func (s *Server) WithCodexPresets(cm *codex.Manager) *Server {
	s.codex = cm
	return s
}

// WithZcodePresets 启用 Z.ai zcode 配置预设管理(/manage/v1/zcode-presets)。
// 需在 Handler() 之前调用。
func (s *Server) WithZcodePresets(cm *zcode.Manager) *Server {
	s.zcode = cm
	return s
}

// WithAggregates 启用聚合模型(自动聚合同名模型,裸名调用时轮询负载均衡)。
// 需在 Handler() 之前调用。
func (s *Server) WithAggregates(am *aggregate.Manager) *Server {
	s.aggregates = am
	return s
}

// WithFaults 启用故障提示模块:记录上游特定错误(用户模式)或所有错误(开发模式),
// 经 /manage/v1/faults 陈列展示并逐条删除。需在 Handler() 之前调用。
func (s *Server) WithFaults(fm *fault.Manager) *Server {
	s.faults = fm
	return s
}

// WithClaudeSettingsPath 指定"覆盖本地 Claude Code 配置"的目标 settings.json 路径。
// 留空时默认 ~/.claude/settings.json。仅测试与自定义场景使用。
func (s *Server) WithClaudeSettingsPath(path string) *Server {
	s.claudeSettingsPath = path
	return s
}

// WithCodexConfigPath 指定"覆盖本地 Codex 配置"的目标 config.toml 路径。
// 留空时默认 ~/.codex/config.toml。仅测试与自定义场景使用。
func (s *Server) WithCodexConfigPath(path string) *Server {
	s.codexConfigPath = path
	return s
}

// WithCodexAuthPath 指定"覆盖本地 Codex 鉴权"的目标 auth.json 路径。
// 留空时默认 ~/.codex/auth.json。仅测试与自定义场景使用。
func (s *Server) WithCodexAuthPath(path string) *Server {
	s.codexAuthPath = path
	return s
}

// WithCodexModelCatalogPath 指定"覆盖本地 Codex 模型目录"的目标文件路径。
// 留空时默认 ~/.codex/bsrouter-models.json。仅测试与自定义场景使用。
func (s *Server) WithCodexModelCatalogPath(path string) *Server {
	s.codexModelCatalogPath = path
	return s
}

// WithCodexModelsCachePath 指定"覆盖本地 Codex 模型缓存"的目标文件路径。
// 留空时默认 ~/.codex/models_cache.json(桌面 app 读此文件)。仅测试与自定义场景使用。
func (s *Server) WithCodexModelsCachePath(path string) *Server {
	s.codexModelsCachePath = path
	return s
}

// WithZcodeConfigPath 指定"覆盖本地 zcode 配置"的目标 config.json 路径。
// 留空时默认 ~/.zcode/v2/config.json。仅测试与自定义场景使用。
func (s *Server) WithZcodeConfigPath(path string) *Server {
	s.zcodeConfigPath = path
	return s
}

// WithDeployment 设置网关部署形态(remote 地址派生/命令替换的依据)。
func (s *Server) WithDeployment(d *Deployment) *Server {
	s.deployment = d
	return s
}

// WithNetworkManager 启用出口地址配置(管理经 /manage/v1/network)。
func (s *Server) WithNetworkManager(nm *network.Manager) *Server {
	s.netm = nm
	return s
}

// WithWebUI 挂载内嵌前端页面(静态资源,无需鉴权)。需在 Handler() 之前调用。
func (s *Server) WithWebUI(h http.Handler) *Server {
	s.webUI = h
	return s
}

// Handler 返回路由。URL 结构:/manage 下为管理接口,/api 下为统一 API
// (下游客户端调用)与分组虚拟供应商。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// 统一 API(下游客户端调用)
	mux.HandleFunc("POST /api/v1/messages", s.handleAnthropic)
	mux.HandleFunc("POST /api/v1/chat/completions", s.handleCompletion)
	mux.HandleFunc("POST /api/v1/responses", s.handleResponses)
	mux.HandleFunc("GET /api/v1/models", s.handleListModels)
	// 管理端模型列表(与 /api/v1/models 同源,供管理界面用 /manage 鉴权访问,
	// 避免管理端依赖下游凭据导致 401)。
	mux.HandleFunc("GET /manage/v1/models", s.handleListModels)
	// 供应商管理端点
	mux.HandleFunc("POST /manage/v1/providers", s.handleAddProvider)
	mux.HandleFunc("GET /manage/v1/providers", s.handleListProviders)
	mux.HandleFunc("GET /manage/v1/providers/{name}", s.handleGetProvider)
	mux.HandleFunc("PUT /manage/v1/providers/{name}", s.handleUpdateProvider)
	mux.HandleFunc("DELETE /manage/v1/providers/{name}", s.handleDeleteProvider)
	// 单模型上下文窗口更新(供模型管理页行内编辑)。
	mux.HandleFunc("PUT /manage/v1/providers/{name}/models/{model}", s.handleUpdateModelContextWindow)
	// 供应商探测端点
	mux.HandleFunc("POST /manage/v1/providers/{name}/ping", s.handlePingProvider)
	mux.HandleFunc("POST /manage/v1/providers/{name}/sync-models", s.handleSyncModels)
	mux.HandleFunc("GET /manage/v1/providers/{name}/usage", s.handleUsageProvider)
	// 分组管理端点与分组虚拟供应商派发
	if s.groups != nil {
		mux.HandleFunc("POST /manage/v1/groups", s.handleAddGroup)
		mux.HandleFunc("GET /manage/v1/groups", s.handleListGroups)
		mux.HandleFunc("GET /manage/v1/groups/{name}", s.handleGetGroup)
		mux.HandleFunc("PUT /manage/v1/groups/{name}", s.handleUpdateGroup)
		mux.HandleFunc("DELETE /manage/v1/groups/{name}", s.handleDeleteGroup)
		// 分组虚拟供应商:/api/{分组URL}/v1/...(具体路由优先)。
		mux.Handle("/api/", http.HandlerFunc(s.handleGroupURL))
	}
	// 日志查看端点(列表 + 当前日志文件路径 + 完整度分级)
	mux.HandleFunc("GET /manage/v1/logs", s.handleListLogs)
	mux.HandleFunc("GET /manage/v1/logs/file", s.handleLogFile)
	if s.log != nil {
		mux.HandleFunc("GET /manage/v1/logs/detail", s.handleGetLogDetail)
		mux.HandleFunc("PUT /manage/v1/logs/detail", s.handleSetLogDetail)
	}
	// 本地模式检测(前端据此启用本地配置覆盖功能)
	mux.HandleFunc("GET /manage/v1/local", s.handleLocalStatus)
	// 部署形态与出口地址(远程/NAT 部署下前端据此提醒填写出口 IP 与映射端口)
	if s.netm != nil {
		mux.HandleFunc("GET /manage/v1/network", s.handleGetNetwork)
		mux.HandleFunc("PUT /manage/v1/network", s.handleSetNetwork)
	}
	// 模型列表拉取(供前端表单自动填充,供应商尚未注册)
	mux.HandleFunc("POST /manage/v1/fetch-models", s.handleFetchModels)
	// 内置供应商接入模板(用户只需填入 api_key)
	mux.HandleFunc("GET /manage/v1/provider-templates", s.handleListProviderTemplates)
	// 受管 API Key 端点
	if s.keys != nil {
		mux.HandleFunc("POST /manage/v1/keys", s.handleAddKey)
		mux.HandleFunc("GET /manage/v1/keys", s.handleListKeys)
		mux.HandleFunc("DELETE /manage/v1/keys/{name}", s.handleDeleteKey)
	}
	// Claude Code 配置预设端点
	if s.presets != nil {
		mux.HandleFunc("POST /manage/v1/claude-presets", s.handleAddClaudePreset)
		mux.HandleFunc("GET /manage/v1/claude-presets", s.handleListClaudePresets)
		mux.HandleFunc("GET /manage/v1/claude-presets/{name}", s.handleGetClaudePreset)
		mux.HandleFunc("PUT /manage/v1/claude-presets/{name}", s.handleUpdateClaudePreset)
		mux.HandleFunc("DELETE /manage/v1/claude-presets/{name}", s.handleDeleteClaudePreset)
		mux.HandleFunc("GET /manage/v1/claude-presets/{name}/command", s.handleClaudePresetCommand)
		mux.HandleFunc("POST /manage/v1/claude-presets/{name}/apply-local", s.handleApplyClaudePresetLocal)
	}
	// OpenAI Codex 配置预设端点
	if s.codex != nil {
		mux.HandleFunc("POST /manage/v1/codex-presets", s.handleAddCodexPreset)
		mux.HandleFunc("GET /manage/v1/codex-presets", s.handleListCodexPresets)
		mux.HandleFunc("GET /manage/v1/codex-presets/{name}", s.handleGetCodexPreset)
		mux.HandleFunc("PUT /manage/v1/codex-presets/{name}", s.handleUpdateCodexPreset)
		mux.HandleFunc("DELETE /manage/v1/codex-presets/{name}", s.handleDeleteCodexPreset)
		mux.HandleFunc("GET /manage/v1/codex-presets/{name}/command", s.handleCodexPresetCommand)
		mux.HandleFunc("POST /manage/v1/codex-presets/{name}/apply-local", s.handleApplyCodexPresetLocal)
		mux.HandleFunc("GET /manage/v1/codex-native-slugs", s.handleListCodexNativeSlugs)
	}
	// Z.ai zcode 配置预设端点
	if s.zcode != nil {
		mux.HandleFunc("POST /manage/v1/zcode-presets", s.handleAddZcodePreset)
		mux.HandleFunc("GET /manage/v1/zcode-presets", s.handleListZcodePresets)
		mux.HandleFunc("GET /manage/v1/zcode-presets/{name}", s.handleGetZcodePreset)
		mux.HandleFunc("PUT /manage/v1/zcode-presets/{name}", s.handleUpdateZcodePreset)
		mux.HandleFunc("DELETE /manage/v1/zcode-presets/{name}", s.handleDeleteZcodePreset)
		mux.HandleFunc("POST /manage/v1/zcode-presets/{name}/apply-local", s.handleApplyZcodePresetLocal)
	}
	// 聚合模型端点
	if s.aggregates != nil {
		mux.HandleFunc("GET /manage/v1/aggregates", s.handleListAggregates)
		mux.HandleFunc("PUT /manage/v1/aggregates/{name}", s.handleUpdateAggregate)
	}
	// 故障提示端点(记录上游特定错误/内部错误,供前端陈列展示并逐条删除)
	if s.faults != nil {
		mux.HandleFunc("GET /manage/v1/faults", s.handleListFaults)
		mux.HandleFunc("DELETE /manage/v1/faults/{id}", s.handleDeleteFault)
	}
	// 内嵌前端页面(静态资源)
	if s.webUI != nil {
		mux.Handle("/", s.webUI)
	}
	var h http.Handler = mux
	if s.apiKey != "" || (s.keys != nil && s.keys.Count() > 0) {
		// key 在构建时快照进闭包:鉴权开关与逐请求比较不会因后续修改而分叉,
		// 也避免逐请求读字段带来的数据竞争。
		h = s.requireAPIKey(s.apiKey, h)
	}
	if s.log != nil || s.faults != nil {
		// 日志/故障记录在最外层:被鉴权拒绝的请求也要记录。
		h = s.logMiddleware(h)
	}
	return h
}

// requireAPIKey 是 API Key 鉴权中间件,校验通过后才进入路由。
// 仅对 API 路径(/api、/manage)强制鉴权;前端页面与静态资源无需鉴权。
// 鉴权规则:
//   - 网关 key 可访问 /api 与 /manage;
//   - 受管 apikey 仅可访问 /api(模型请求);
//   - /manage 未配置网关 key 时保持"无鉴权"模式(纯本地)。
func (s *Server) requireAPIKey(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleaned := path.Clean(r.URL.Path)
		if !isAPIPath(cleaned) {
			next.ServeHTTP(w, r)
			return
		}
		// 模型列表端点公开(仅含模型 ID,无密钥),下游客户端免 key 即可发现可用模型。
		if s.isPublicModelList(r, cleaned) {
			next.ServeHTTP(w, r)
			return
		}
		got := apiKeyFromRequest(r)
		valid := key != "" && constantTimeEqual(got, key)
		if !valid && s.keys != nil && strings.HasPrefix(cleaned, "/api/") {
			valid = s.keys.Valid(got)
		}
		if !valid && !(strings.HasPrefix(cleaned, "/manage/") && key == "") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gateway"`)
			writeError(w, http.StatusUnauthorized, errors.New("unauthorized: missing or invalid api key"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAPIPath 判断请求是否应视为受保护的网关 API(/api 统一 API 与分组、/manage 管理接口)。
// 对解码后的路径先做 path.Clean 归一化,与 ServeMux 的路由/重定向语义保持一致,
// 避免 "/api" 精确路径、"/../manage/..."、"/api%2f..." 等变体绕过鉴权与日志。
func isAPIPath(p string) bool {
	p = path.Clean(p)
	return p == "/api" || p == "/manage" || strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/manage/")
}

// isPublicModelList 判断 GET 请求是否为公开的模型列表端点:统一 API 的
// /api/v1/models、管理端同源 /manage/v1/models,以及各分组虚拟供应商的
// {分组URL}/v1/models。模型列表只返回模型 ID(无密钥),公开以便下游客户端
// 免 key 发现可用模型;鉴权仍覆盖其余全部 /api 与 /manage 端点。
func (s *Server) isPublicModelList(r *http.Request, cleaned string) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch cleaned {
	case "/api/v1/models", "/manage/v1/models":
		return true
	}
	// 分组模型列表:URL 形如 {分组URL}/v1/models,经 ResolveURL 判定归属某个分组。
	// 分组 URL 必然位于 /api 下且不占用 /api/v1 保留段,故 /api/v1/models 不会误配。
	if s.groups != nil {
		if _, rest, ok := s.groups.ResolveURL(cleaned); ok && rest == "/v1/models" {
			return true
		}
	}
	return false
}

// constantTimeEqual 以 SHA-256 摘要做常数时间比较:无论输入长度如何,比较的都是
// 固定长度的摘要,运行时长与密钥长度/是否存在无关,不泄露网关密钥的时序信息。
func constantTimeEqual(a, b string) bool {
	da := sha256.Sum256([]byte(a))
	db := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(da[:], db[:]) == 1
}

// apiKeyFromRequest 从 Authorization: Bearer 或 x-api-key 头中提取 API Key,
// 兼容 OpenAI 风格客户端与 Anthropic 风格客户端。
func apiKeyFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
			return strings.TrimSpace(auth[len(prefix):])
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// ---- 请求日志 ----

// ctxKey 用于在请求上下文中传递日志富化数据。
type ctxKey int

const logCtxKey ctxKey = iota

// logRecord 记录一次请求在转发过程中解析出的富化信息。
type logRecord struct {
	requestID             string
	model                 string
	provider              string // 日志展示用(分组请求带 "组名→" 前缀)
	providerName          string // 真实供应商名(不含分组前缀,供故障阻塞匹配)
	kind                  string
	error                 string
	upstreamStatus        int
	blocked               bool   // 该请求被故障阻塞(供故障模块跳过重复记录)
	faultRecorded         bool   // 转发失败已在成员级即时记录(供故障模块跳过最终重复记录)
	requestBody           string // 客户端原始请求体(仅转换路径)
	forwardURL            string
	forwardRequest        string // 发给上游(转换后的请求体)
	forwardResponse       string // 上游返回(转换前/上游格式)
	convertedResponseBody string // 转换后回客户端(仅转换路径)
}

// 日志完整度取值。
const (
	LogDetailDefault = "default"
	LogDetailFull    = "full"
)

// logDetailOf 返回当前日志完整度(并发安全)。
func (s *Server) logDetailOf() string {
	s.logDetailMu.RLock()
	defer s.logDetailMu.RUnlock()
	if s.logDetail == LogDetailFull {
		return LogDetailFull
	}
	return LogDetailDefault
}

// keepForwardDetail 判断该请求是否记录完整转发详情:
// full 完整度总是记录;default 仅当出错(网关内部错误 / 供应商返回错误)时记录。
func keepForwardDetail(s *Server, rec *logRecord, status int) bool {
	if s.logDetailOf() == LogDetailFull {
		return true
	}
	return rec.error != "" || status >= 400
}

// recordStreamError 记录流式错误:客户端未断开记一切错误;客户端已断开仅记源于上游的
// 错误(截断/读体失败/上游 error 事件),避免把用户手动中断(Esc/新消息取代)误报为上游故障。
func recordStreamError(rec *logRecord, ctx context.Context, err error) {
	if rec == nil || rec.error != "" {
		return
	}
	if ctx.Err() == nil || gateway.IsUpstreamStreamError(err) {
		rec.error = captureBody(err.Error(), "")
	}
}

// markBlocked 若 err 是阻塞错误,在日志记录上记入错误信息(供日志排查)并打上 blocked
// 标记,使故障模块不把它重复记为一条新故障(该故障已在故障列表中,正是它触发了阻塞)。
func markBlocked(ctx context.Context, err error) {
	var be *fault.BlockedError
	if !errors.As(err, &be) {
		return
	}
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.error = captureBody(err.Error(), "")
		rec.blocked = true
	}
}

// recordMemberFault 在转发某个成员失败时即时记录故障(供故障转移中途触发阻塞——即使
// 后续成员成功,该失败成员也已被记录)。fault.Record 按模式与分类过滤(用户模式仅记
// 余额不足等可阻塞故障,开发模式记全部)。err 应已抹除密钥。
func (s *Server) recordMemberFault(rec *logRecord, upstreamStatus int, providerName, model string, err error) {
	if s.faults == nil || rec == nil || err == nil {
		return
	}
	rec.faultRecorded = true
	rl, ins, rlEnabled, rlDur := s.providerBlockInput(providerName)
	s.faults.Record(fault.Input{
		Error:                    captureBody(err.Error(), ""),
		Status:                   http.StatusBadGateway,
		UpstreamStatus:           upstreamStatus,
		Upstream:                 upstreamStatus != 0 || providerName != "",
		Model:                    model,
		Provider:                 providerName,
		RateLimitStatus:          rl,
		RateLimitEnabled:         rlEnabled,
		RateLimitDurationMinutes: rlDur,
		InsufficientBalanceStatus: ins,
	})
}

// providerBlockInput 返回供应商自定义的阻塞配置:限流/余额不足错误码(nil = 用默认)、
// 限流阻塞开关(nil = 启用)、限流阻塞时长(分钟,0 = 默认 120)。供应商不存在时全 nil/0。
func (s *Server) providerBlockInput(name string) (rateLimit, insufficient *int, rateLimitEnabled *bool, rateLimitDurationMinutes int) {
	if s.mgr != nil {
		if p, err := s.mgr.Get(name); err == nil {
			cfg := p.Config()
			return cfg.RateLimitStatus, cfg.InsufficientBalanceStatus, cfg.RateLimitEnabled, cfg.RateLimitDurationMinutes
		}
	}
	return nil, nil, nil, 0
}

// recordFault 把一次出错请求记录为故障(供故障提示模块)。判定规则:
//   - 转发失败已在成员级即时记录(rec.faultRecorded)或阻塞错误(rec.blocked)时跳过;
//   - 有错误信息(rec.error,如流式中途失败)或网关返回非鉴权类 4xx/5xx 时视为故障;
//   - 鉴权失败(401/403)不属于网关/上游故障,不记录;
//   - 无错误信息时按状态码合成故障内容。
// 具体分类与模式过滤由 fault.Manager 决定(用户模式仅记硬编码特定故障,开发模式记全部)。
func (s *Server) recordFault(rec *logRecord, status int) {
	if rec == nil {
		return
	}
	// 阻塞错误(供应商被故障禁用)不重复记录:对应故障已在故障列表中,正是它触发了阻塞。
	if rec.blocked {
		return
	}
	// 转发失败已在成员级即时记录(complete/streamComplete/直通闭包),此处不再重复记录。
	if rec.faultRecorded {
		return
	}
	if rec.error == "" && (status < 400 || status == http.StatusUnauthorized || status == http.StatusForbidden) {
		return
	}
	msg := rec.error
	if msg == "" {
		msg = fmt.Sprintf("gateway returned %d %s", status, http.StatusText(status))
	}
	providerName := rec.providerName
	if providerName == "" {
		providerName = rec.provider // 无真实供应商名时回退(分组前缀形态)
	}
	rl, ins, rlEnabled, rlDur := s.providerBlockInput(providerName)
	s.faults.Record(fault.Input{
		Error:                    msg,
		Status:                   status,
		UpstreamStatus:           rec.upstreamStatus,
		Upstream:                 rec.upstreamStatus != 0 || rec.provider != "",
		Model:                    rec.model,
		Provider:                 providerName,
		RateLimitStatus:          rl,
		RateLimitEnabled:         rlEnabled,
		RateLimitDurationMinutes: rlDur,
		InsufficientBalanceStatus: ins,
	})
}

// handleListFaults 返回当前故障捕捉模式与全部故障(最新在前)。
func (s *Server) handleListFaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":   s.faults.Mode(),
		"faults": s.faults.List(),
	})
}

// handleDeleteFault 删除一条故障;不存在返回 404,持久化失败返回 500。
func (s *Server) handleDeleteFault(w http.ResponseWriter, r *http.Request) {
	if err := s.faults.Delete(r.PathValue("id")); err != nil {
		if errors.Is(err, fault.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logMiddleware 记录所有 API 请求的访问日志(JSONL)。处于最外层,鉴权拒绝的请求也记录;
// 使用 defer 保证 handler panic 时也写入日志(补齐 500)。前端静态资源请求不写入日志。
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &logRecord{requestID: newRequestID()}
		ctx := context.WithValue(r.Context(), logCtxKey, rec)
		rw := &statusRecorder{ResponseWriter: w}
		panicked := true
		defer func() {
			if panicked {
				// handler panic 时 net/http 会回写 500,这里补齐对应日志。
				rw.code = http.StatusInternalServerError
			}
			status := rw.status()
			entry := logger.Entry{
				Timestamp:      time.Now().Format(time.RFC3339Nano),
				RequestID:      rec.requestID,
				Method:         r.Method,
				Path:           r.URL.Path,
				Status:         status,
				DurationMS:     time.Since(start).Milliseconds(),
				RemoteAddr:     r.RemoteAddr,
				UserAgent:      r.UserAgent(),
				RequestBytes:   maxInt64(r.ContentLength, 0),
				ResponseBytes:  rw.bytes,
				Model:          rec.model,
				Provider:       rec.provider,
				Kind:           rec.kind,
				UpstreamStatus: rec.upstreamStatus,
				Error:          rec.error,
			}
			// 完整度分级:default 模式下正常请求只记简单信息,出错才带完整转发详情。
			if keepForwardDetail(s, rec, status) {
				entry.RequestBody = rec.requestBody
				entry.ForwardURL = rec.forwardURL
				entry.ForwardRequest = rec.forwardRequest
				entry.ForwardResponse = rec.forwardResponse
				entry.ConvertedResponseBody = rec.convertedResponseBody
			}
			if s.log != nil {
				s.log.Log(entry)
			}
			if s.faults != nil {
				s.recordFault(rec, status)
			}
		}()
		next.ServeHTTP(rw, r.WithContext(ctx))
		panicked = false
	})
}

// statusRecorder 包装 ResponseWriter 以捕获响应状态码与字节数。
type statusRecorder struct {
	http.ResponseWriter
	code  int
	bytes int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// captureWriter 包装 ResponseWriter,累计写入的前 maxForwardBody 字节
// (供记录流式转换后回客户端的 SSE 前段;截断规则与 captureBody 对齐)。
type captureWriter struct {
	http.ResponseWriter
	flusher http.Flusher
	buf     []byte
}

func newCaptureWriter(w http.ResponseWriter) *captureWriter {
	cw := &captureWriter{ResponseWriter: w}
	if fl, ok := w.(http.Flusher); ok {
		cw.flusher = fl
	}
	return cw
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if remain := maxForwardBody - len(c.buf); remain > 0 {
		if len(p) < remain {
			remain = len(p)
		}
		c.buf = append(c.buf, p[:remain]...)
	}
	return c.ResponseWriter.Write(p)
}

func (c *captureWriter) Flush() {
	if c.flusher != nil {
		c.flusher.Flush()
	}
}

// writeConvertedJSON 编码响应体并写回客户端,同时把转换后的响应体记录到日志
// (用于非流式转换路径的 converted_response_body)。
func writeConvertedJSON(w http.ResponseWriter, status int, v any, rec *logRecord) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rec != nil {
		rec.convertedResponseBody = captureBody(string(data), "")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// maxForwardBody 限制写入日志的转发请求/响应体大小。
const maxForwardBody = 256 << 10 // 256 KB

// minRedactKeyLen 只有足够长的 key 才做内容替换,避免短测试 key(如 "k")把
// prompt/响应中的自然文本误替换成乱码。真实供应商 api_key 远长于此阈值。
const minRedactKeyLen = 8

// captureBody 将转发内容转为日志字符串:抹除 api_key(含 URL 编码形态)并
// 按 rune 边界截断超长内容,避免切断多字节字符。
func captureBody(s, key string) string {
	if key != "" && len(key) >= minRedactKeyLen {
		s = strings.ReplaceAll(s, key, "***")
		// 也抹除 URL 编码形态(如 base_url 内嵌 ?key=sk%2F...)。
		if qk := url.QueryEscape(key); qk != key {
			s = strings.ReplaceAll(s, qk, "***")
		}
		if pk := url.PathEscape(key); pk != key && pk != url.QueryEscape(key) {
			s = strings.ReplaceAll(s, pk, "***")
		}
	}
	if len(s) > maxForwardBody {
		s = s[:maxForwardBody]
		// 去掉末尾不完整的 UTF-8 序列,避免日志中出现 U+FFFD 替换字符。
		for len(s) > 0 {
			r, size := utf8.DecodeLastRuneInString(s)
			if r != utf8.RuneError || size != 1 {
				break
			}
			s = s[:len(s)-size]
		}
		return s + "...(truncated)"
	}
	return s
}

// ---- 直通转发 ----
//
// 直通是转发端点的前置分支:模型支持客户端请求的接口格式时,请求体不经中间层
// (canonical)转换,原样转发到上游;响应同样原样回传,仅把 model 字段改写为客户端
// 请求的完整模型名(用户要求所有情况下 model 都处理为正确内容)。不命中直通时,
// handler 恢复原始 body 走现有转换路径(route/streamRoute,行为完全不变)。

// maxErrBody 限制直通路径读取非 2xx 错误体的大小。
const maxErrBody = 1 << 20 // 1 MB

// readRawBody 读取原始请求体并限制大小(语义与 decodeJSON 一致:超大 413、其余 400)。
// 同时把客户端原始请求体记入日志记录(直通路径随后清空;转换路径保留为 request_body)。
func (s *Server) readRawBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, err)
		} else {
			writeError(w, http.StatusBadRequest, err)
		}
		return nil, false
	}
	if rec, ok := r.Context().Value(logCtxKey).(*logRecord); ok {
		rec.requestBody = captureBody(string(data), "")
	}
	return data, true
}

// metaModelStream 从原始请求体解析直通所需的顶层字段 {model, stream}(三格式同字段)。
// 解析失败时写 400 并返回 nil。
func (s *Server) metaModelStream(w http.ResponseWriter, raw []byte, label string) *struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
} {
	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode %s: %w", label, err))
		return nil
	}
	return &meta
}

// directTarget 是直通目标:单供应商(显式 @ 路由)或聚合(全部成员同格式)。
type directTarget struct {
	single  bool
	p       provider.Provider
	model   string   // 上游真实模型名:单供应商去 @ 前缀;聚合为剥上下文标记后的裸名
	members []string // 聚合:按优先级顺序的成员
}

// resolveDirectTarget 判定请求可否直通:
//   - 聚合:所有成员都支持客户端格式才整体直通(裸名路由主体不变);
//   - 单供应商:显式 @ 路由且模型支持客户端格式。
// 否则返回 false,交由转换路径处理(转换路径自会 404/正常转换)。
func (s *Server) resolveDirectTarget(fullModel, clientFormat string) (*directTarget, bool) {
	// native alias 把裸原生 slug 映射到绑定路由模型;fullModel 仅用于响应回填。
	routed := s.resolveAliasedModel(fullModel)
	base := provider.StripContextMarker(routed)
	if s.aggregates != nil {
		if members, ok := s.aggregates.Members(base); ok {
			allSupport := true
			for _, member := range members {
				p, _, err := s.mgr.Resolve(member + "@" + base)
				if err != nil || !p.Supports(base, provider.Kind(clientFormat)) {
					allSupport = false
					break
				}
			}
			if allSupport {
				return &directTarget{model: base, members: members}, true
			}
		}
	}
	p, model, err := s.resolveModel(routed)
	if err != nil {
		return nil, false
	}
	if !p.Supports(model, provider.Kind(clientFormat)) {
		return nil, false
	}
	return &directTarget{single: true, p: p, model: model}, true
}

// directCapture 注入转发详情收集器(抹除该供应商 api_key)并设置日志供应商前缀。
// 同时注入流式 idle 超时(覆盖直通 + 分组直通;directComplete 调用它但 doRaw 不读该值,无副作用)。
func (s *Server) directCapture(ctx context.Context, p provider.Provider, label string) context.Context {
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.provider = label
		rec.providerName = p.Name()
		rec.error = "" // 故障转移:清空上一成员遗留的错误
		key := p.Config().APIKey
		ctx = gateway.WithCapture(ctx, func(url string, reqBody, respBody []byte, status int) {
			rec.forwardURL = captureBody(url, key)
			rec.forwardRequest = captureBody(string(reqBody), key)
			rec.forwardResponse = captureBody(string(respBody), key)
			rec.upstreamStatus = status
		})
	}
	return gateway.WithStreamIdleTimeout(ctx, s.streamIdleTimeout)
}

// directComplete 执行一次非流式直通转发:命中返回 true(响应已写出)。
// labelPrefix 用于分组(组名+"→"),统一 API 为空串;最终 provider 日志为 labelPrefix+供应商名。
func (s *Server) directComplete(w http.ResponseWriter, r *http.Request, fullModel string, raw []byte, clientFormat, labelPrefix string) bool {
	target, ok := s.resolveDirectTarget(fullModel, clientFormat)
	if !ok {
		return false
	}
	reqRaw := rewriteModel(raw, target.model) // 改请求 model 为上游真实模型名(去 @ 前缀/上下文标记)
	if reqRaw == nil {
		reqRaw = raw
	}
	if rec, ok := r.Context().Value(logCtxKey).(*logRecord); ok {
		rec.model = fullModel
		rec.kind = clientFormat
		// 直通无格式转换:不记录客户端原始请求体(request_body),只记 forward_*(发给上游的)。
		rec.requestBody = ""
		rec.convertedResponseBody = ""
	}
	var status int
	var body []byte
	var err error
	rec, _ := r.Context().Value(logCtxKey).(*logRecord)
	if target.single {
		ctx := s.directCapture(r.Context(), target.p, labelPrefix+target.p.Name())
		status, body, err = target.p.CompleteRaw(ctx, provider.Kind(clientFormat), reqRaw)
		if err != nil {
			err = redactKey(err, target.p.Config().APIKey)
		} else if status < 200 || status >= 300 {
			err = redactKey(fmt.Errorf("upstream returned %d: %s", status, truncateBody(body)), target.p.Config().APIKey)
		}
		if err != nil {
			// 单供应商失败即时记录(无聚合时同样触发阻塞)。
			s.recordMemberFault(rec, status, target.p.Name(), fullModel, err)
		}
	} else {
		// 转发顺序用 faultFilteredOrder(跳过冷却中成员 + 剔除被故障禁用的成员)。
		order, _ := s.faultFilteredOrder(target.model)
		if len(order) == 0 {
			err = &fault.BlockedError{Reason: "all providers are currently blocked"}
		} else {
			status, body, err = s.failoverRaw(target.model, order, func(member string) (int, []byte, error) {
				p, _, rerr := s.mgr.Resolve(member + "@" + target.model)
				if rerr != nil {
					return 0, nil, rerr
				}
				ctx := s.directCapture(r.Context(), p, labelPrefix+p.Name())
				st, bd, cerr := p.CompleteRaw(ctx, provider.Kind(clientFormat), reqRaw)
				if cerr != nil {
					e := redactKey(cerr, p.Config().APIKey)
					s.recordMemberFault(rec, 0, p.Name(), fullModel, e)
					return 0, nil, e
				}
				if st < 200 || st >= 300 {
					e := redactKey(fmt.Errorf("upstream returned %d: %s", st, truncateBody(bd)), p.Config().APIKey)
					s.recordMemberFault(rec, st, p.Name(), fullModel, e)
					return st, bd, e
				}
				return st, bd, nil
			})
		}
	}
	if err != nil {
		markBlocked(r.Context(), err)
		if rec, ok := r.Context().Value(logCtxKey).(*logRecord); ok && rec.error == "" {
			rec.error = captureBody(err.Error(), "")
		}
		writeRouteError(w, err)
		return true
	}
	// 2xx:回填响应 model 为客户端请求的完整模型名,状态码原样透传。
	out := rewriteModel(body, fullModel)
	if out == nil {
		out = body
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
	return true
}

// directStream 执行一次流式直通转发:命中返回 true(响应已开始写出)。
// 故障转移在流开始前完成(非 2xx/传输失败切换成员);拿到 2xx 上游 SSE 体后
// 逐事件透传并仅改写各格式事件中的 model 字段。
func (s *Server) directStream(w http.ResponseWriter, r *http.Request, fullModel string, raw []byte, clientFormat, labelPrefix string) bool {
	target, ok := s.resolveDirectTarget(fullModel, clientFormat)
	if !ok {
		return false
	}
	reqRaw := rewriteModel(raw, target.model)
	if reqRaw == nil {
		reqRaw = raw
	}
	rec, _ := r.Context().Value(logCtxKey).(*logRecord)
	if rec != nil {
		rec.model = fullModel
		rec.kind = clientFormat
		// 直通无格式转换:不记录客户端原始请求体(request_body),只记 forward_*。
		rec.requestBody = ""
		rec.convertedResponseBody = ""
	}
	routeError := func(err error) {
		markBlocked(r.Context(), err)
		if rec != nil && rec.error == "" {
			rec.error = captureBody(err.Error(), "")
		}
		writeRouteError(w, err)
	}
	var body io.ReadCloser
	if target.single {
		ctx := s.directCapture(r.Context(), target.p, labelPrefix+target.p.Name())
		resp, err := target.p.StreamRaw(ctx, provider.Kind(clientFormat), reqRaw)
		if err != nil {
			e := redactKey(err, target.p.Config().APIKey)
			s.recordMemberFault(rec, 0, target.p.Name(), fullModel, e)
			routeError(e)
			return true
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
			resp.Body.Close()
			e := redactKey(fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncateBody(data)), target.p.Config().APIKey)
			s.recordMemberFault(rec, resp.StatusCode, target.p.Name(), fullModel, e)
			routeError(e)
			return true
		}
		body = resp.Body
	} else {
		// 聚合:转发顺序用 faultFilteredOrder(跳过冷却中成员 + 剔除被故障禁用的成员)。
		order, _ := s.faultFilteredOrder(target.model)
		if len(order) == 0 {
			routeError(&fault.BlockedError{Reason: "all providers are currently blocked"})
			return true
		}
		err := s.failoverForward(target.model, order, func(member string) error {
			p, _, rerr := s.mgr.Resolve(member + "@" + target.model)
			if rerr != nil {
				return rerr
			}
			ctx := s.directCapture(r.Context(), p, labelPrefix+p.Name())
			resp, serr := p.StreamRaw(ctx, provider.Kind(clientFormat), reqRaw)
			if serr != nil {
				e := redactKey(serr, p.Config().APIKey)
				s.recordMemberFault(rec, 0, p.Name(), fullModel, e)
				return e
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				defer resp.Body.Close()
				data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
				e := redactKey(fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncateBody(data)), p.Config().APIKey)
				s.recordMemberFault(rec, resp.StatusCode, p.Name(), fullModel, e)
				return e
			}
			body = resp.Body
			return nil
		})
		if err != nil {
			routeError(err)
			return true
		}
	}
	defer body.Close()
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := gateway.RewriteSSEModel(w, body, clientFormat, fullModel); err != nil {
		// 客户端断开或上游流中断:响应已发出,无法改写状态码。
		// 客户端未断开记一切错误;客户端已断开仅记源于上游的错误(截断/读体失败)。
		if rec, ok := r.Context().Value(logCtxKey).(*logRecord); ok {
			recordStreamError(rec, r.Context(), err)
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true
}

// failoverRaw 对聚合成员做原始转发故障转移:依序调用 send(member),闭包负责把非 2xx
// 转为 error(带抹除密钥),任何 error 均视为成员失败(切换到下一成员);首个成功
// (err==nil)返回并冷却失败成员;全败返回最后一次错误。
func (s *Server) failoverRaw(base string, members []string, send func(member string) (int, []byte, error)) (int, []byte, error) {
	var lastErr error
	var lastStatus int
	var lastBody []byte
	failed := make([]string, 0, len(members))
	for _, member := range members {
		status, body, err := send(member)
		if err != nil {
			lastErr = err
			lastStatus, lastBody = status, body
			failed = append(failed, member)
			continue
		}
		if len(failed) > 0 {
			s.aggregates.Ban(base, failed...)
		}
		return status, body, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all members failed")
	}
	return lastStatus, lastBody, lastErr
}

// rewriteModel 改写 JSON 的顶层 model 字段。返回改写后的 JSON;无需改写
// (非 JSON / 无 model 键 / 值未变化)时返回 nil。用 UseNumber 防大整数精度丢失。
func rewriteModel(data []byte, model string) []byte {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil
	}
	cur, ok := obj["model"].(string)
	if !ok || cur == model {
		return nil
	}
	obj["model"] = model
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}

// truncateBody 截断上游响应体,用于错误信息。
func truncateBody(b []byte) string {
	const max = 500
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// ---- 转发端点 ----

// handleAnthropic 接收 Anthropic Messages API 格式的请求。
// 模型支持该格式时走直通(原样转发,不经中间层转换),否则走现有转换路径。
func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readRawBody(w, r)
	if !ok {
		return
	}
	meta := s.metaModelStream(w, raw, "anthropic request")
	if meta == nil {
		return
	}
	if meta.Stream {
		if s.directStream(w, r, meta.Model, raw, gateway.FormatAnthropic, "") {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var req gateway.AnthropicRequest
		if !decodeJSON(w, r, "anthropic request", &req) {
			return
		}
		s.serveStream(w, r, req.ToInternal(), gateway.FormatAnthropic)
		return
	}
	if s.directComplete(w, r, meta.Model, raw, gateway.FormatAnthropic, "") {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req gateway.AnthropicRequest
	if !decodeJSON(w, r, "anthropic request", &req) {
		return
	}
	resp, err := s.route(r.Context(), req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	rec, _ := r.Context().Value(logCtxKey).(*logRecord)
	writeConvertedJSON(w, http.StatusOK, resp.ToAnthropic(), rec)
}

// handleCompletion 接收 OpenAI chat.completions 格式的请求。
// 模型支持该格式时走直通(原样转发,不经中间层转换),否则走现有转换路径。
func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readRawBody(w, r)
	if !ok {
		return
	}
	meta := s.metaModelStream(w, raw, "completion request")
	if meta == nil {
		return
	}
	if meta.Stream {
		if s.directStream(w, r, meta.Model, raw, gateway.FormatCompletion, "") {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var req gateway.CompletionRequest
		if !decodeJSON(w, r, "completion request", &req) {
			return
		}
		s.serveStream(w, r, req.ToInternal(), gateway.FormatCompletion)
		return
	}
	if s.directComplete(w, r, meta.Model, raw, gateway.FormatCompletion, "") {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req gateway.CompletionRequest
	if !decodeJSON(w, r, "completion request", &req) {
		return
	}
	resp, err := s.route(r.Context(), req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	rec, _ := r.Context().Value(logCtxKey).(*logRecord)
	writeConvertedJSON(w, http.StatusOK, resp.ToCompletion(), rec)
}

// handleResponses 接收 OpenAI responses 格式的请求。
// 模型支持该格式时走直通(原样转发,不经中间层转换),否则走现有转换路径。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readRawBody(w, r)
	if !ok {
		return
	}
	meta := s.metaModelStream(w, raw, "responses request")
	if meta == nil {
		return
	}
	if meta.Stream {
		if s.directStream(w, r, meta.Model, raw, gateway.FormatResponses, "") {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var req gateway.ResponsesRequest
		if !decodeJSON(w, r, "responses request", &req) {
			return
		}
		s.serveStream(w, r, req.ToInternal(), gateway.FormatResponses)
		return
	}
	if s.directComplete(w, r, meta.Model, raw, gateway.FormatResponses, "") {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req gateway.ResponsesRequest
	if !decodeJSON(w, r, "responses request", &req) {
		return
	}
	resp, err := s.route(r.Context(), req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	rec, _ := r.Context().Value(logCtxKey).(*logRecord)
	writeConvertedJSON(w, http.StatusOK, resp.ToResponses(), rec)
}

// serveStream 是统一 API 流式转发公共路径:解析供应商、启动上游流、经规范化事件回写 SSE。
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, req *gateway.Request, clientFormat string) {
	fullModel := req.Model
	body, upFormat, err := s.streamRoute(r.Context(), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	defer body.Close()
	out := w
	var rec *logRecord
	if rv, ok := r.Context().Value(logCtxKey).(*logRecord); ok {
		rec = rv
		out = newCaptureWriter(w) // 记录转换后回客户端的 SSE 前段
	}
	if err := s.writeSSE(out, rec, clientFormat, upFormat, fullModel, body); err != nil {
		// 客户端断开或上游流中断:响应已发出,无法改写状态码。
		// 客户端未断开记一切错误;客户端已断开仅记源于上游的错误(截断/读体失败)。
		recordStreamError(rec, r.Context(), err)
	}
	if rec != nil {
		rec.convertedResponseBody = captureBody(string(out.(*captureWriter).buf), "")
	}
}

// streamComplete 向单个供应商发起一次流式转发(日志/捕获/抹除逻辑同 complete),
// 返回上游 SSE 响应体(调用方负责 Close)与其接口格式。失败时返回 nil 体,不泄漏。
func (s *Server) streamComplete(ctx context.Context, req *gateway.Request, fullModel string, p provider.Provider, model, label, kind string) (io.ReadCloser, string, error) {
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.provider = label
		rec.providerName = p.Name()
		rec.kind = kind
		rec.error = "" // 故障转移:清空上一成员遗留的错误
		// 注入转发详情收集器:记录转发到哪、发了什么、回了什么(抹除 api_key)。
		ctx = gateway.WithCapture(ctx, func(url string, reqBody, respBody []byte, status int) {
			key := p.Config().APIKey
			rec.forwardURL = captureBody(url, key)
			rec.forwardRequest = captureBody(string(reqBody), key)
			rec.forwardResponse = captureBody(string(respBody), key)
			rec.upstreamStatus = status
		})
	}
	// 注入流式 idle 超时(覆盖转换 + 分组转换;直通经 directCapture)。
	ctx = gateway.WithStreamIdleTimeout(ctx, s.streamIdleTimeout)
	upFormat := string(p.ModelKind(model))
	req.Model = model // 去掉供应商前缀,以上游真实模型名请求

	// 流开始前失败(请求发送错误 / 上游非 2xx)做有限重试:客户端断开(ctx 取消)即停,
	// 4xx 不重试。重试在单个成员内部,与聚合故障转移(成员之间)共存——成员重试耗尽
	// 仍算一次 send(member) 失败,failoverForward 再切换到下一成员。
	var body io.ReadCloser
	var err error
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil, "", redactKey(ctx.Err(), p.Config().APIKey)
		}
		body, err = p.Stream(ctx, req)
		if err == nil {
			return body, upFormat, nil
		}
		if !s.retryableStreamError(ctx, err) || attempt >= s.streamRetries {
			break
		}
		if serr := sleepStreamRetry(ctx, attempt); serr != nil {
			return nil, "", redactKey(serr, p.Config().APIKey)
		}
	}
	redacted := redactKey(err, p.Config().APIKey)
	upstream := 0
	var sg interface{ HTTPStatus() int }
	if errors.As(err, &sg) {
		upstream = sg.HTTPStatus()
	}
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.error = captureBody(redacted.Error(), "")
		rec.upstreamStatus = upstream
		// 故障转移中途即时记录:即使后续成员成功,该失败成员也已被记录(可触发阻塞)。
		s.recordMemberFault(rec, upstream, p.Name(), fullModel, redacted)
	}
	return nil, "", redacted
}

// retryableStreamError 判断一次流开始前失败是否值得重试:
// 客户端已断开不重试;上游 5xx(503/502/504 等)与连接/超时错误重试;4xx(鉴权/参数)不重试。
func (s *Server) retryableStreamError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var sg interface{ HTTPStatus() int }
	if errors.As(err, &sg) {
		return sg.HTTPStatus() >= 500
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// 连接/传输错误经 net 包的 *url.Error 暴露。
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	return false
}

// sleepStreamRetry 重试前的退避等待:2^attempt * 500ms,封顶 3s;ctx 取消则立即返回。
func sleepStreamRetry(ctx context.Context, attempt int) error {
	d := time.Duration(1<<attempt) * 500 * time.Millisecond
	if d > 3*time.Second {
		d = 3 * time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// streamRoute 转发一个流式请求:聚合模型做故障转移(流开始前可切换到其余成员),否则
// 单供应商转发。返回上游 SSE 响应体(调用方负责 Close)与其接口格式(供流式解码器使用)。
func (s *Server) streamRoute(ctx context.Context, req *gateway.Request) (io.ReadCloser, string, error) {
	fullModel := req.Model
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.model = fullModel
	}
	// native alias 把裸原生 slug 映射到绑定路由模型;fullModel 保持原始值用于响应回填。
	routed := s.resolveAliasedModel(fullModel)
	base := provider.StripContextMarker(routed)
	if members, ok := s.aggregateMembers(base); ok {
		if len(members) == 0 {
			err := &fault.BlockedError{Reason: "all providers are currently blocked"}
			markBlocked(ctx, err)
			return nil, "", err
		}
		var body io.ReadCloser
		var upFormat string
		err := s.failoverForward(base, members, func(member string) error {
			p, model, rerr := s.mgr.Resolve(member + "@" + base)
			if rerr != nil {
				return rerr
			}
			var serr error
			body, upFormat, serr = s.streamComplete(ctx, req, fullModel, p, model, p.Name(), string(p.ModelKind(model)))
			return serr
		})
		return body, upFormat, err
	}
	p, model, err := s.resolveModel(routed)
	if err != nil {
		if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
			rec.error = captureBody(err.Error(), "")
		}
		markBlocked(ctx, err)
		return nil, "", err
	}
	return s.streamComplete(ctx, req, fullModel, p, model, p.Name(), string(p.ModelKind(model)))
}

// writeSSE 把上游 SSE 流经规范化中间类型转换后写回客户端。
// 上游格式 SSE →(解码器)→ 规范化事件 →(模型回填等格式无关变换)→(编码器)→ 客户端格式 SSE。
// rec 非空时,在写出 error 事件前先记录 rec.error——客户端若随后断开,写会失败但日志不丢;
// 这同时修复"上游 anthropic/responses error 事件不记日志"的既有盲区。
func (s *Server) writeSSE(w http.ResponseWriter, rec *logRecord, clientFormat, upFormat, fullModel string, body io.Reader) error {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	decoder, ok := gateway.DecoderFor(upFormat)
	if !ok {
		return fmt.Errorf("no stream decoder for upstream format %q", upFormat)
	}
	encoder, ok := gateway.EncoderFor(clientFormat)
	if !ok {
		return fmt.Errorf("no stream encoder for client format %q", clientFormat)
	}
	return decoder(body, func(ev gateway.StreamEvent) error {
		// 规范化 → 规范化:回填响应模型为客户端请求的完整模型名。
		if ev.Type == gateway.StreamMessageStart {
			ev.Model = fullModel
		}
		if ev.Type == gateway.StreamError && rec != nil && ev.Error != "" {
			rec.error = captureBody(ev.Error, "")
		}
		if err := encoder(w, ev); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
}

// resolveModel 解析非聚合模型:剥离上下文标记后按供应商前缀解析。
// 聚合模型的解析与故障转移由 route/streamRoute/routeGroup/streamGroupRoute
// 在调用本函数前先走 aggregateMembers 分支。
func (s *Server) resolveModel(model string) (provider.Provider, string, error) {
	p, m, err := s.mgr.Resolve(provider.StripContextMarker(model))
	if err != nil {
		return nil, "", err
	}
	// 供应商被故障(余额不足等)禁用时,单独请求直接返回阻塞原因,不转发上游。
	if s.faults != nil {
		if reason, blocked := s.faults.Block(p.Name()); blocked {
			return nil, "", &fault.BlockedError{Provider: p.Name(), Reason: reason}
		}
	}
	return p, m, nil
}

// splitContextMarker 把模型 id 拆为基础名与上下文标记(如 [1M]);无合法标记时
// 标记为空。复用 provider.StripContextMarker 的合法标记判定。
func splitContextMarker(model string) (base, marker string) {
	base = provider.StripContextMarker(model)
	if base == model {
		return model, ""
	}
	return base, model[len(base):]
}

// effectiveModels 返回预设生效的模型列表:显式 Models(≤7)优先;留空回退网关全部
// 可路由模型(自动分配前 7 个,向后兼容)。
func (s *Server) effectiveModels(p codex.Config) []string {
	if len(p.Models) > 0 {
		return p.Models
	}
	return s.allModelIDs()
}

// nativeAliases 返回生效的 native-alias 绑定(slug → NativeAlias):收集全部 codex
// 预设的生效模型列表并集,按模型 id 排序,依次占用原生 id 池(每 slug 一模型,
// 池 7 个用尽即停)。display_name 用模型 id(诚实标签:桌面显示真实模型名;原生
// slug 只是通过 Desktop allowlist 的"护照",对应关系无关紧要)。
// codex 预设未启用时为空。
func (s *Server) nativeAliases() map[string]codex.NativeAlias {
	if s.codex == nil {
		return nil
	}
	pool := codex.NativeOpenAISlugs()
	var models []string
	seen := make(map[string]bool)
	for _, p := range s.codex.List() {
		for _, m := range s.effectiveModels(p) {
			m = strings.TrimSpace(m)
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			models = append(models, m)
		}
	}
	sort.Strings(models)
	out := make(map[string]codex.NativeAlias, len(models))
	for i, m := range models {
		if i >= len(pool) {
			break
		}
		out[pool[i]] = codex.NativeAlias{Slug: pool[i], Model: m, DisplayName: m}
	}
	return out
}

// defaultCodexBaseURL 返回网关统一 API 入口(预设 base_url 留空时使用):
// 本地默认 http://127.0.0.1:<监听端口>/api/v1;远程部署时由调用方再按广告地址改写。
func (s *Server) defaultCodexBaseURL() string {
	port := "18154"
	if s.deployment != nil && s.deployment.ListenPort != "" {
		port = s.deployment.ListenPort
	}
	return "http://127.0.0.1:" + port + "/api/v1"
}

// resolveAliasedModel 若 model(去上下文标记后)是 native-alias slug,返回其绑定的
// 路由模型(保留原上下文标记;绑定模型自带标记时不叠加);否则原样返回。仅影响路由
// 目标,请求模型回填仍用调用方传入的原始 fullModel。
func (s *Server) resolveAliasedModel(model string) string {
	base, marker := splitContextMarker(model)
	// 原生 slug 是裸 id(无 @、无 /):合成 id/带前缀 id 不可能是别名,短路避免
	// 每次请求都重建绑定映射。
	if strings.ContainsAny(base, "@/") {
		return model
	}
	a, ok := s.nativeAliases()[base]
	if !ok {
		return model
	}
	if marker != "" {
		return provider.StripContextMarker(a.Model) + marker
	}
	return a.Model
}

// aggregateMembers 若模型是聚合模型,返回其故障转移尝试顺序(轮询起点、跳过冷却中
// 的成员;全部冷却时返回全部成员),并剔除被故障禁用的供应商。未启用聚合或模型非聚合
// 返回 false。聚合的全部成员都被禁用时返回 ok=true 且 members 为空,调用方据此返回
// 阻塞错误。
func (s *Server) aggregateMembers(model string) ([]string, bool) {
	if s.aggregates == nil {
		return nil, false
	}
	return s.faultFilteredOrder(model)
}

// faultFilteredOrder 返回聚合模型的故障转移顺序(轮询/冷却语义同 aggregates.TryOrder),
// 但剔除被故障禁用的供应商。模型非聚合返回 ok=false;聚合全部成员被禁用时返回 ok=true
// 且空切片。
func (s *Server) faultFilteredOrder(model string) ([]string, bool) {
	order, ok := s.aggregates.TryOrder(model)
	if !ok {
		return nil, false
	}
	if s.faults == nil {
		return order, true
	}
	out := make([]string, 0, len(order))
	for _, m := range order {
		if _, blocked := s.faults.Block(m); !blocked {
			out = append(out, m)
		}
	}
	return out, true
}

// failoverForward 对聚合成员做故障转移:依次调用 send(member),首个成功返回 nil;
// 全部失败返回最后一次错误。若期间有成员成功(证明前面的失败是供应商问题而非下游
// 请求问题),把失败成员加入该聚合模型的 10 分钟冷却,削减对异常供应商的请求压力。
// send 由调用方闭包实现单次转发;闭包需把成功结果写回其捕获的返回值。
func (s *Server) failoverForward(base string, members []string, send func(member string) error) error {
	var lastErr error
	failed := make([]string, 0, len(members))
	for _, member := range members {
		if err := send(member); err != nil {
			lastErr = err
			failed = append(failed, member)
			continue
		}
		if len(failed) > 0 {
			s.aggregates.Ban(base, failed...)
		}
		return nil
	}
	return lastErr
}

// complete 向单个供应商转发一次非流式请求:写日志(供应商/格式/清空上一成员遗留的
// 错误)、注入转发详情捕获(抹除该供应商 api_key)、以上游真实模型名请求、失败时抹除
// 错误中的 api_key;成功时把响应模型回填为请求时的完整模型名。
func (s *Server) complete(ctx context.Context, req *gateway.Request, fullModel string, p provider.Provider, model, label, kind string) (*gateway.Response, error) {
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.provider = label
		rec.providerName = p.Name()
		rec.kind = kind
		rec.error = "" // 故障转移:清空上一成员遗留的错误
		// 注入转发详情收集器:记录转发到哪、发了什么、回了什么(抹除 api_key)。
		ctx = gateway.WithCapture(ctx, func(url string, reqBody, respBody []byte, status int) {
			key := p.Config().APIKey
			rec.forwardURL = captureBody(url, key)
			rec.forwardRequest = captureBody(string(reqBody), key)
			rec.forwardResponse = captureBody(string(respBody), key)
			rec.upstreamStatus = status
		})
	}
	req.Model = model // 去掉供应商前缀,以上游真实模型名请求
	resp, err := p.Complete(ctx, req)
	if err != nil {
		redacted := redactKey(err, p.Config().APIKey)
		upstream := 0
		var sg interface{ HTTPStatus() int }
		if errors.As(err, &sg) {
			upstream = sg.HTTPStatus()
		}
		if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
			// 日志只记录已抹除 api_key 的错误,并限制长度(避免超大上游错误体撑爆日志)。
			rec.error = captureBody(redacted.Error(), "")
			rec.upstreamStatus = upstream
			// 故障转移中途即时记录:即使后续成员成功,该失败成员也已被记录(可触发阻塞)。
			s.recordMemberFault(rec, upstream, p.Name(), fullModel, redacted)
		}
		// 防止上游在错误体中回显 api_key 导致经 502 泄露。
		return nil, redacted
	}
	resp.Model = fullModel
	return resp, nil
}

// route 转发一个非流式请求:聚合模型做故障转移(失败切换到其余成员,成功后冷却失败
// 成员),否则单供应商转发;响应模型回填为请求时的完整 "{供应商名}@{模型名}"。
func (s *Server) route(ctx context.Context, req *gateway.Request) (*gateway.Response, error) {
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.model = req.Model
	}
	fullModel := req.Model
	// native alias 把裸原生 slug 映射到绑定路由模型;fullModel 保持原始值用于响应回填。
	routed := s.resolveAliasedModel(fullModel)
	base := provider.StripContextMarker(routed)
	if members, ok := s.aggregateMembers(base); ok {
		if len(members) == 0 {
			err := &fault.BlockedError{Reason: "all providers are currently blocked"}
			markBlocked(ctx, err)
			return nil, err
		}
		var resp *gateway.Response
		err := s.failoverForward(base, members, func(member string) error {
			p, model, rerr := s.mgr.Resolve(member + "@" + base)
			if rerr != nil {
				return rerr
			}
			var cerr error
			resp, cerr = s.complete(ctx, req, fullModel, p, model, p.Name(), string(p.ModelKind(model)))
			return cerr
		})
		return resp, err
	}
	p, model, err := s.resolveModel(routed)
	if err != nil {
		markBlocked(ctx, err)
		return nil, err
	}
	return s.complete(ctx, req, fullModel, p, model, p.Name(), string(p.ModelKind(model)))
}

// redactKey 从错误信息中抹除 api_key。
func redactKey(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "***"))
}

// ---- 供应商管理端点 ----

// handleAddProvider 新增供应商;已存在时返回 409,配置非法返回 400。
func (s *Server) handleAddProvider(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg provider.Config
	if !decodeJSON(w, r, "provider config", &cfg) {
		return
	}
	// 新增时没有可保留的原密钥,提交掩码值属于复制粘贴错误,直接拒绝。
	if strings.Contains(cfg.APIKey, "****") {
		writeError(w, http.StatusBadRequest, errors.New("api_key must not be a masked value"))
		return
	}
	if err := s.mgr.Add(cfg); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sanitizeConfig(cfg))
}

// handleListProviders 返回全部供应商配置(按名称排序,api_key 掩码)。
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	cfgs := s.mgr.List()
	out := make([]provider.Config, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, sanitizeConfig(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListProviderTemplates 返回内置的供应商接入模板目录(不含任何密钥),
// 供前端「模板库」让用户一键填入 base_url/格式/模型,仅需补 api_key 即可接入。
func (s *Server) handleListProviderTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, providertemplates.All())
}

// handleGetProvider 返回单个供应商配置(api_key 掩码);不存在时返回 404。
func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.mgr.Get(r.PathValue("name"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeConfig(p.Config()))
}

// handleUpdateProvider 修改供应商;名称以路径为准,不存在时返回 404。
func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg provider.Config
	if !decodeJSON(w, r, "provider config", &cfg) {
		return
	}
	cfg.Name = r.PathValue("name")
	// 未提供新密钥或提交的是掩码值时,保留原密钥:避免 GET->编辑->PUT 往返
	// 把掩码占位符当成真实密钥写入,导致凭据被永久破坏。
	if existing, err := s.mgr.Get(cfg.Name); err == nil {
		old := existing.Config().APIKey
		if cfg.APIKey == "" || cfg.APIKey == maskKey(old) {
			cfg.APIKey = old
		}
	}
	if err := s.mgr.Update(cfg); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeConfig(cfg))
}

// handleDeleteProvider 删除供应商;不存在时返回 404。
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.PathValue("name")); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePingProvider 测试供应商连通性(可达性 + 鉴权),结果恒以 200 返回。
func (s *Server) handlePingProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.mgr.Get(r.PathValue("name"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	result, _ := p.Ping(r.Context())
	// 防止上游错误/传输错误信息中回显 api_key 或包含带凭据的 URL。
	if key := p.Config().APIKey; key != "" {
		result.Error = strings.ReplaceAll(result.Error, key, "***")
	}
	writeJSON(w, http.StatusOK, result)
}

// handleSyncModels 从模型列表接口拉取模型并更新该供应商的 Models。
func (s *Server) handleSyncModels(w http.ResponseWriter, r *http.Request) {
	p, err := s.mgr.Get(r.PathValue("name"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	models, err := p.FetchModels(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, redactKey(err, p.Config().APIKey))
		return
	}
	if len(models) == 0 {
		// 拒绝用空列表覆盖已知模型,避免一次上游抖动清空配置。
		writeError(w, http.StatusBadGateway, errors.New("sync returned no models; refusing to overwrite existing models"))
		return
	}
	// 同步模型:已在列表中的模型保留其模型级接口格式与上下文窗口,新增模型使用
	// 供应商默认(Kind 留空、窗口 0 = 200k)。
	existingKind := make(map[string]provider.Kind)
	existingWindow := make(map[string]int)
	for _, m := range p.Models() {
		if m.Kind != "" {
			existingKind[m.Name] = m.Kind
		}
		if m.ContextWindow != 0 {
			existingWindow[m.Name] = m.ContextWindow
		}
	}
	modelCfgs := make([]provider.ModelConfig, 0, len(models))
	for _, id := range models {
		modelCfgs = append(modelCfgs, provider.ModelConfig{
			Name:          id,
			Kind:          existingKind[id],
			ContextWindow: existingWindow[id],
		})
	}
	// 只更新模型列表,保留其余字段,避免覆盖并发的配置修改。
	if err := s.mgr.SetModels(p.Name(), modelCfgs); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": p.Name(),
		"models":   models,
		"count":    len(models),
	})
}

// handleUpdateModelContextWindow 更新单个模型的上下文窗口(k,0 = 清空回默认 200k)。
// body {"context_window":N};N<0 返回 400,供应商/模型不存在返回 404。只改目标模型
// 的窗口,其余配置不变(供模型管理页行内编辑,无需整份提交供应商配置)。
func (s *Server) handleUpdateModelContextWindow(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var body struct {
		ContextWindow int `json:"context_window"`
	}
	if !decodeJSON(w, r, "model context window", &body) {
		return
	}
	if body.ContextWindow < 0 {
		writeError(w, http.StatusBadRequest, errors.New("context_window must be >= 0"))
		return
	}
	p, err := s.mgr.Get(r.PathValue("name"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	model := r.PathValue("model")
	cfg := p.Config()
	idx := -1
	for i, m := range cfg.Models {
		if m.Name == model {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, errors.New("model not found"))
		return
	}
	// 复制切片再改目标项,避免与管理器内部配置共享底层数组造成别名修改。
	newModels := make([]provider.ModelConfig, len(cfg.Models))
	copy(newModels, cfg.Models)
	newModels[idx].ContextWindow = body.ContextWindow
	cfg.Models = newModels
	if err := s.mgr.Update(cfg); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeConfig(cfg))
}

// handleUsageProvider 从配置的 UsageURL 查询用量并原样透传上游响应。
func (s *Server) handleUsageProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.mgr.Get(r.PathValue("name"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	data, err := p.QueryUsage(r.Context())
	if err != nil {
		if errors.Is(err, provider.ErrNotConfigured) {
			writeError(w, http.StatusBadRequest, err)
		} else {
			writeError(w, http.StatusBadGateway, redactKey(err, p.Config().APIKey))
		}
		return
	}
	// 防止上游在 2xx 响应中回显 api_key。
	if key := p.Config().APIKey; key != "" {
		data = bytes.ReplaceAll(data, []byte(key), []byte("***"))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleListLogs 返回最近若干条请求日志(最新的在前);limit 默认 100,上限 1000。
// 默认只返回 /api 转发日志(管理端点的日志不进列表,避免界面被管理操作刷屏);
// 传 scope=all 可查看全部(含 /manage)。
func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	if s.log == nil {
		writeError(w, http.StatusBadRequest, errors.New("request logging is disabled"))
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	var keep func(logger.Entry) bool
	if r.URL.Query().Get("scope") != "all" {
		// 仅转发日志:/api 路径(统一 API 与分组虚拟供应商)。
		keep = func(e logger.Entry) bool { return strings.HasPrefix(e.Path, "/api") }
	}
	entries, err := s.log.Recent(limit, keep)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleLogFile 返回当前请求日志文件路径(每次运行默认以启动时间戳命名,
// 供前端展示用户当前正在查看哪份日志)。
func (s *Server) handleLogFile(w http.ResponseWriter, r *http.Request) {
	if s.log == nil {
		writeError(w, http.StatusBadRequest, errors.New("request logging is disabled"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": s.log.Path()})
}

// handleGetLogDetail 返回当前日志完整度("default" / "full")。
func (s *Server) handleGetLogDetail(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"detail": s.logDetailOf()})
}

// handleSetLogDetail 设置日志完整度(body {detail:"default"|"full"}),运行时生效
// 并持久化到配置(若配置了持久化路径)。
func (s *Server) handleSetLogDetail(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var req struct {
		Detail string `json:"detail"`
	}
	if !decodeJSON(w, r, "log detail", &req) {
		return
	}
	if req.Detail != LogDetailDefault && req.Detail != LogDetailFull {
		writeError(w, http.StatusBadRequest, fmt.Errorf("detail must be %q or %q", LogDetailDefault, LogDetailFull))
		return
	}
	s.logDetailMu.Lock()
	s.logDetail = req.Detail
	path := s.logDetailPath
	s.logDetailMu.Unlock()
	if path != "" {
		data, err := json.MarshalIndent(map[string]string{"detail": req.Detail}, "", "  ")
		if err == nil {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("persist log detail: %w", err))
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"detail": req.Detail})
}

// handleFetchModels 从未注册供应商的模型列表地址拉取模型列表,供前端表单自动填充。
// 复用 provider.FetchModels;失败时抹除 api_key 再返回。
// 编辑场景:请求体可带 name;若 api_key 留空且 name 指向已注册供应商,则用服务端
// 存储的密钥发起拉取(真实密钥不暴露给前端),避免"编辑时保留密钥导致无法拉取模型"。
func (s *Server) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var req struct {
		Name      string        `json:"name,omitempty"`
		Kind      provider.Kind `json:"kind"`
		BaseURL   string        `json:"base_url"`
		APIKey    string        `json:"api_key"`
		ModelsURL string        `json:"models_url"`
	}
	if !decodeJSON(w, r, "fetch-models request", &req) {
		return
	}
	if req.APIKey == "" && req.Name != "" {
		if p, err := s.mgr.Get(req.Name); err == nil {
			req.APIKey = p.Config().APIKey
		}
	}
	cfg := provider.Config{
		Name:      "__fetch__",
		Kind:      req.Kind,
		BaseURL:   req.BaseURL,
		APIKey:    req.APIKey,
		ModelsURL: req.ModelsURL,
	}
	p, err := provider.New(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	models, err := p.FetchModels(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, redactKey(err, req.APIKey))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "count": len(models)})
}

// ---- 受管 API Key 端点 ----

// handleAddKey 生成一个新 API Key(body 提供 name),返回完整 Key 供分发给下游。
func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, "apikey request", &req) {
		return
	}
	cfg, err := s.keys.Generate(req.Name)
	if err != nil {
		writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, cfg)
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.keys.List())
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	if err := s.keys.Delete(r.PathValue("name")); err != nil {
		writeKeyError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apikey.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, apikey.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, apikey.ErrPersist):
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// modelEntry 是模型列表中的一项,id 为 "{供应商名}@{模型名}" 或聚合裸名。
// 遵循 OpenAI 兼容 /models 规范,元素仅含 id/object/owned_by;
// context_window 为上下文窗口(k 为单位):供应商模型取配置值,聚合模型取全部有效
// 成员的最小值(见 aggregateContextWindowK),未配置时省略。
type modelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	OwnedBy       string `json:"owned_by"`
	ContextWindow int    `json:"context_window,omitempty"`
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// allModelEntries 聚合网关全部可路由模型:各供应商模型合成 "{供应商}@{模型}"
// (经 Resolve 验证真实命中)+ 聚合模型(裸名)。返回按 id 排序的条目列表,
// 是 /api/v1/models、/manage/v1/models 与 codex 模型目录的共同数据源。
func (s *Server) allModelEntries() []modelEntry {
	entries := make([]modelEntry, 0)
	seen := make(map[string]bool)
	for _, cfg := range s.mgr.List() {
		for _, m := range cfg.Models {
			id := cfg.Name + "@" + m.Name
			if seen[id] {
				continue
			}
			p, resolved, err := s.mgr.Resolve(id)
			if err != nil || p.Name() != cfg.Name || resolved != m.Name {
				continue // 该 id 实际会路由到其他供应商,发布出去只会误导客户端
			}
			seen[id] = true
			entries = append(entries, modelEntry{
				ID:            id,
				Object:        "model",
				OwnedBy:       cfg.Name,
				ContextWindow: m.ContextWindow,
			})
		}
	}
	// 聚合模型(裸名,挂在统一供应商下)。context_window 取全部有效成员的最小值
	// (故障转移/负载均衡到任一成员都不超窗;成员未配置窗口时按默认 200k 参与取小)。
	if s.aggregates != nil {
		for _, a := range s.aggregates.Models() {
			if seen[a.Name] {
				continue
			}
			seen[a.Name] = true
			// 复用 aggregate.Model.Members(aggregate.Models 已算好),避免
			// 再次全量扫描供应商重推成员(该函数在模型列表与热路径上都被调用)。
			entries = append(entries, modelEntry{
				ID:            a.Name,
				Object:        "model",
				OwnedBy:       "unified",
				ContextWindow: s.membersContextWindowK(a.Name, a.Members),
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries
}

// handleListModels 聚合所有供应商的模型与聚合模型,按 id 排序。
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: s.allModelEntries()})
}

// ---- 响应与错误 ----

// maskKey 掩码处理 api_key,保证中间隐藏部分不少于 3 个字符。
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) <= 4 {
		return "****"
	}
	show := len(r) / 4
	if show < 2 {
		show = 2
	}
	if show > 4 {
		show = 4
	}
	if hidden := len(r) - 2*show; hidden < 3 {
		show = (len(r) - 3) / 2
	}
	return string(r[:show]) + "****" + string(r[len(r)-show:])
}

// sanitizeConfig 返回用于 HTTP 响应的配置,掩码 api_key。
func sanitizeConfig(cfg provider.Config) provider.Config {
	cfg.APIKey = maskKey(cfg.APIKey)
	return cfg
}

// requireJSONBody 拒绝非 application/json 的写请求,阻止浏览器以 CORS-safelisted
// 的 text/plain 等类型无预检直接提交,降低管理端点的 CSRF 风险。
func requireJSONBody(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return false
	}
	return true
}

// decodeJSON 解码 JSON 请求体并限制大小:请求体过大返回 413,格式非法返回 400。
func decodeJSON(w http.ResponseWriter, r *http.Request, label string, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, err)
		} else {
			writeError(w, http.StatusBadRequest, fmt.Errorf("decode %s: %w", label, err))
		}
		return false
	}
	return true
}

// writeRouteError 按错误类型映射转发端点状态:未知供应商 -> 404,供应商被故障禁用 -> 503,
// 其余上游错误 -> 502。
func writeRouteError(w http.ResponseWriter, err error) {
	var be *fault.BlockedError
	switch {
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.As(err, &be):
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		writeError(w, http.StatusBadGateway, err)
	}
}

// writeManagerError 映射管理端点错误状态:不存在 -> 404,已存在 -> 409,
// 持久化失败 -> 500,其余(配置非法)-> 400。
func writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, provider.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, provider.ErrPersist):
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
