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
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"BSRouter/internal/aggregate"
	"BSRouter/internal/apikey"
	"BSRouter/internal/claude"
	"BSRouter/internal/gateway"
	"BSRouter/internal/group"
	"BSRouter/internal/logger"
	"BSRouter/internal/network"
	"BSRouter/internal/provider"
)

// maxBodyBytes 限制入站请求体大小,防止未鉴权的内存耗尽。
const maxBodyBytes = 10 << 20 // 10 MB

// Server 是网关 HTTP 服务。
type Server struct {
	mgr        *provider.Manager
	groups     *group.Manager
	keys       *apikey.Manager
	presets    *claude.Manager
	aggregates *aggregate.Manager
	apiKey     string
	log        *logger.Logger
	webUI      http.Handler
	// claudeSettingsPath 覆盖本地 Claude Code 配置的目标 settings.json 路径;
	// 留空时用 ~/.claude/settings.json(仅测试/自定义使用)。
	claudeSettingsPath string
	// deployment 部署形态(由 cmd/gateway 启动时判定);netm 是出口地址配置。
	deployment *Deployment
	netm       *network.Manager
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

// WithAggregates 启用聚合模型(自动聚合同名模型,裸名调用时轮询负载均衡)。
// 需在 Handler() 之前调用。
func (s *Server) WithAggregates(am *aggregate.Manager) *Server {
	s.aggregates = am
	return s
}

// WithClaudeSettingsPath 指定"覆盖本地 Claude Code 配置"的目标 settings.json 路径。
// 留空时默认 ~/.claude/settings.json。仅测试与自定义场景使用。
func (s *Server) WithClaudeSettingsPath(path string) *Server {
	s.claudeSettingsPath = path
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
	// 日志查看端点
	mux.HandleFunc("GET /manage/v1/logs", s.handleListLogs)
	// 本地模式检测(前端据此启用本地配置覆盖功能)
	mux.HandleFunc("GET /manage/v1/local", s.handleLocalStatus)
	// 部署形态与出口地址(远程/NAT 部署下前端据此提醒填写出口 IP 与映射端口)
	if s.netm != nil {
		mux.HandleFunc("GET /manage/v1/network", s.handleGetNetwork)
		mux.HandleFunc("PUT /manage/v1/network", s.handleSetNetwork)
	}
	// 模型列表拉取(供前端表单自动填充,供应商尚未注册)
	mux.HandleFunc("POST /manage/v1/fetch-models", s.handleFetchModels)
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
	// 聚合模型端点
	if s.aggregates != nil {
		mux.HandleFunc("GET /manage/v1/aggregates", s.handleListAggregates)
		mux.HandleFunc("PUT /manage/v1/aggregates/{name}", s.handleUpdateAggregate)
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
	if s.log != nil {
		// 日志在最外层:被鉴权拒绝的请求也要记录。
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
	requestID       string
	model           string
	provider        string
	kind            string
	error           string
	upstreamStatus  int
	forwardURL      string
	forwardRequest  string
	forwardResponse string
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
			s.log.Log(logger.Entry{
				Timestamp:       time.Now().Format(time.RFC3339Nano),
				RequestID:       rec.requestID,
				Method:          r.Method,
				Path:            r.URL.Path,
				Status:          rw.status(),
				DurationMS:      time.Since(start).Milliseconds(),
				RemoteAddr:      r.RemoteAddr,
				UserAgent:       r.UserAgent(),
				RequestBytes:    maxInt64(r.ContentLength, 0),
				ResponseBytes:   rw.bytes,
				Model:           rec.model,
				Provider:        rec.provider,
				Kind:            rec.kind,
				UpstreamStatus:  rec.upstreamStatus,
				Error:           rec.error,
				ForwardURL:      rec.forwardURL,
				ForwardRequest:  rec.forwardRequest,
				ForwardResponse: rec.forwardResponse,
			})
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

// ---- 转发端点 ----

// handleAnthropic 接收 Anthropic Messages API 格式的请求。
func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	var req gateway.AnthropicRequest
	if !decodeJSON(w, r, "anthropic request", &req) {
		return
	}
	if req.Stream {
		s.serveStream(w, r, req.ToInternal(), gateway.FormatAnthropic)
		return
	}
	resp, err := s.route(r.Context(), req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp.ToAnthropic())
}

// handleCompletion 接收 OpenAI chat.completions 格式的请求。
func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request) {
	var req gateway.CompletionRequest
	if !decodeJSON(w, r, "completion request", &req) {
		return
	}
	if req.Stream {
		s.serveStream(w, r, req.ToInternal(), gateway.FormatCompletion)
		return
	}
	resp, err := s.route(r.Context(), req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp.ToCompletion())
}

// handleResponses 接收 OpenAI responses 格式的请求。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req gateway.ResponsesRequest
	if !decodeJSON(w, r, "responses request", &req) {
		return
	}
	if req.Stream {
		s.serveStream(w, r, req.ToInternal(), gateway.FormatResponses)
		return
	}
	resp, err := s.route(r.Context(), req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp.ToResponses())
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
	if err := s.writeSSE(w, clientFormat, upFormat, fullModel, body); err != nil {
		// 客户端断开或上游流中断:响应已发出,无法改写状态码。
		if rec, ok := r.Context().Value(logCtxKey).(*logRecord); ok {
			if r.Context().Err() == nil {
				rec.error = captureBody(err.Error(), "")
			}
		}
	}
}

// streamComplete 向单个供应商发起一次流式转发(日志/捕获/抹除逻辑同 complete),
// 返回上游 SSE 响应体(调用方负责 Close)与其接口格式。失败时返回 nil 体,不泄漏。
func (s *Server) streamComplete(ctx context.Context, req *gateway.Request, fullModel string, p provider.Provider, model, label, kind string) (io.ReadCloser, string, error) {
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.provider = label
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
	upFormat := string(p.ModelKind(model))
	req.Model = model // 去掉供应商前缀,以上游真实模型名请求
	body, err := p.Stream(ctx, req)
	if err != nil {
		redacted := redactKey(err, p.Config().APIKey)
		if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
			rec.error = captureBody(redacted.Error(), "")
			var sg interface{ HTTPStatus() int }
			if errors.As(err, &sg) {
				rec.upstreamStatus = sg.HTTPStatus()
			}
		}
		return nil, "", redacted
	}
	return body, upFormat, nil
}

// streamRoute 转发一个流式请求:聚合模型做故障转移(流开始前可切换到其余成员),否则
// 单供应商转发。返回上游 SSE 响应体(调用方负责 Close)与其接口格式(供流式解码器使用)。
func (s *Server) streamRoute(ctx context.Context, req *gateway.Request) (io.ReadCloser, string, error) {
	fullModel := req.Model
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.model = fullModel
	}
	base := provider.StripContextMarker(fullModel)
	if members, ok := s.aggregateMembers(base); ok {
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
	p, model, err := s.resolveModel(fullModel)
	if err != nil {
		if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
			rec.error = captureBody(err.Error(), "")
		}
		return nil, "", err
	}
	return s.streamComplete(ctx, req, fullModel, p, model, p.Name(), string(p.ModelKind(model)))
}

// writeSSE 把上游 SSE 流经规范化中间类型转换后写回客户端。
// 上游格式 SSE →(解码器)→ 规范化事件 →(模型回填等格式无关变换)→(编码器)→ 客户端格式 SSE。
func (s *Server) writeSSE(w http.ResponseWriter, clientFormat, upFormat, fullModel string, body io.Reader) error {
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
	return s.mgr.Resolve(provider.StripContextMarker(model))
}

// aggregateMembers 若模型是聚合模型,返回其故障转移尝试顺序(轮询起点、跳过冷却中
// 的成员;全部冷却时返回全部成员)。未启用聚合或模型非聚合返回 false。
func (s *Server) aggregateMembers(model string) ([]string, bool) {
	if s.aggregates == nil {
		return nil, false
	}
	return s.aggregates.TryOrder(model)
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
		if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
			// 日志只记录已抹除 api_key 的错误,并限制长度(避免超大上游错误体撑爆日志)。
			rec.error = captureBody(redacted.Error(), "")
			var sg interface{ HTTPStatus() int }
			if errors.As(err, &sg) {
				rec.upstreamStatus = sg.HTTPStatus()
			}
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
	base := provider.StripContextMarker(fullModel)
	if members, ok := s.aggregateMembers(base); ok {
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
	p, model, err := s.resolveModel(fullModel)
	if err != nil {
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
	// 同步模型:已在列表中的模型保留其模型级接口格式,新增模型使用供应商默认(Kind 留空)。
	existingKind := make(map[string]provider.Kind)
	for _, m := range p.Models() {
		if m.Kind != "" {
			existingKind[m.Name] = m.Kind
		}
	}
	modelCfgs := make([]provider.ModelConfig, 0, len(models))
	for _, id := range models {
		modelCfgs = append(modelCfgs, provider.ModelConfig{Name: id, Kind: existingKind[id]})
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
	entries, err := s.log.Recent(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
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

// modelEntry 是模型列表中的一项,id 为 "{供应商名}-{模型名}"。
// 遵循 OpenAI 兼容 /models 规范,元素仅含 id/object/owned_by。
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// handleListModels 聚合所有供应商的模型与聚合模型,按 id 排序。
// 合成 id 为 "{供应商}@{模型}";聚合模型为不含 @ 的裸模型名(owned_by=unified)。
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
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
				ID:      id,
				Object:  "model",
				OwnedBy: cfg.Name,
			})
		}
	}
	// 聚合模型(裸名,挂在统一供应商下)。
	if s.aggregates != nil {
		for _, a := range s.aggregates.Models() {
			if seen[a.Name] {
				continue
			}
			seen[a.Name] = true
			entries = append(entries, modelEntry{ID: a.Name, Object: "model", OwnedBy: "unified"})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: entries})
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

// writeRouteError 按错误类型映射转发端点状态:未知供应商 -> 404,上游错误 -> 502。
func writeRouteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
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
