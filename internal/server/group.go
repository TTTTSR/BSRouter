package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"BSRouter/internal/fault"
	"BSRouter/internal/gateway"
	"BSRouter/internal/group"
	"BSRouter/internal/provider"
)

// ---- 分组管理端点 ----

func (s *Server) handleAddGroup(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg group.Config
	if !decodeJSON(w, r, "group config", &cfg) {
		return
	}
	if err := s.groups.Add(cfg); err != nil {
		writeGroupError(w, err)
		return
	}
	g, _ := s.groups.Get(cfg.Name) // 返回归一化(含默认 URL)后的配置
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.groups.List())
}

func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	g, err := s.groups.Get(r.PathValue("name"))
	if err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var cfg group.Config
	if !decodeJSON(w, r, "group config", &cfg) {
		return
	}
	cfg.Name = r.PathValue("name")
	// 未提供 url 时保留原 url,避免部分更新把自定义 URL 重置为默认 "/{name}",
	// 导致下游客户端全部 404(与供应商 PUT 保留 api_key 的做法一致)。
	if cfg.URL == "" {
		if existing, err := s.groups.Get(cfg.Name); err == nil {
			cfg.URL = existing.URL
		}
	}
	if err := s.groups.Update(cfg); err != nil {
		writeGroupError(w, err)
		return
	}
	g, _ := s.groups.Get(cfg.Name)
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.groups.Delete(r.PathValue("name")); err != nil {
		writeGroupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeGroupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, group.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, group.ErrExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, group.ErrPersist):
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// ---- 分组虚拟供应商端点 ----

// handleGroupURL 按分组 URL 最长前缀派发请求到对应虚拟供应商。
// 作为 ServeMux 的兜底路由,具体路由优先。
func (s *Server) handleGroupURL(w http.ResponseWriter, r *http.Request) {
	g, rest, ok := s.groups.ResolveURL(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no group at this path"))
		return
	}
	switch rest {
	case "/v1/models":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		s.handleGroupModels(w, g)
	case "/v1/chat/completions":
		if g.Kind != provider.KindCompletion {
			writeError(w, http.StatusNotFound, errors.New("group does not serve chat.completions"))
			return
		}
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		s.handleGroupCompletion(w, r, g)
	case "/v1/messages":
		if g.Kind != provider.KindAnthropic {
			writeError(w, http.StatusNotFound, errors.New("group does not serve messages"))
			return
		}
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		s.handleGroupAnthropic(w, r, g)
	case "/v1/responses":
		if g.Kind != provider.KindResponses {
			writeError(w, http.StatusNotFound, errors.New("group does not serve responses"))
			return
		}
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		s.handleGroupResponses(w, r, g)
	default:
		writeError(w, http.StatusNotFound, errors.New("unknown group endpoint"))
	}
}

func (s *Server) handleGroupModels(w http.ResponseWriter, g group.Config) {
	entries := make([]modelEntry, 0, len(g.Models))
	for _, m := range g.Models {
		// context_window 与统一列表一致:合成 id 取供应商配置,聚合裸名取成员最小值。
		entries = append(entries, modelEntry{ID: m, Object: "model", OwnedBy: g.Name, ContextWindow: s.modelContextWindowK(m)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: entries})
}

func (s *Server) handleGroupCompletion(w http.ResponseWriter, r *http.Request, g group.Config) {
	raw, ok := s.readRawBody(w, r)
	if !ok {
		return
	}
	meta := s.metaModelStream(w, raw, "group completion request")
	if meta == nil {
		return
	}
	if meta.Stream {
		if s.directGroupStream(w, r, g, meta.Model, raw, gateway.FormatCompletion) {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var req gateway.CompletionRequest
		if !decodeJSON(w, r, "group completion request", &req) {
			return
		}
		s.serveGroupStream(w, r, g, req.ToInternal(), gateway.FormatCompletion)
		return
	}
	if s.directGroupComplete(w, r, g, meta.Model, raw, gateway.FormatCompletion) {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req gateway.CompletionRequest
	if !decodeJSON(w, r, "group completion request", &req) {
		return
	}
	resp, err := s.routeGroup(r.Context(), g, req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	rec, _ := r.Context().Value(logCtxKey).(*logRecord)
	writeConvertedJSON(w, http.StatusOK, resp.ToCompletion(), rec)
}

func (s *Server) handleGroupAnthropic(w http.ResponseWriter, r *http.Request, g group.Config) {
	raw, ok := s.readRawBody(w, r)
	if !ok {
		return
	}
	meta := s.metaModelStream(w, raw, "group anthropic request")
	if meta == nil {
		return
	}
	if meta.Stream {
		if s.directGroupStream(w, r, g, meta.Model, raw, gateway.FormatAnthropic) {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var req gateway.AnthropicRequest
		if !decodeJSON(w, r, "group anthropic request", &req) {
			return
		}
		s.serveGroupStream(w, r, g, req.ToInternal(), gateway.FormatAnthropic)
		return
	}
	if s.directGroupComplete(w, r, g, meta.Model, raw, gateway.FormatAnthropic) {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req gateway.AnthropicRequest
	if !decodeJSON(w, r, "group anthropic request", &req) {
		return
	}
	resp, err := s.routeGroup(r.Context(), g, req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	rec, _ := r.Context().Value(logCtxKey).(*logRecord)
	writeConvertedJSON(w, http.StatusOK, resp.ToAnthropic(), rec)
}

func (s *Server) handleGroupResponses(w http.ResponseWriter, r *http.Request, g group.Config) {
	raw, ok := s.readRawBody(w, r)
	if !ok {
		return
	}
	meta := s.metaModelStream(w, raw, "group responses request")
	if meta == nil {
		return
	}
	if meta.Stream {
		if s.directGroupStream(w, r, g, meta.Model, raw, gateway.FormatResponses) {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var req gateway.ResponsesRequest
		if !decodeJSON(w, r, "group responses request", &req) {
			return
		}
		s.serveGroupStream(w, r, g, req.ToInternal(), gateway.FormatResponses)
		return
	}
	if s.directGroupComplete(w, r, g, meta.Model, raw, gateway.FormatResponses) {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req gateway.ResponsesRequest
	if !decodeJSON(w, r, "group responses request", &req) {
		return
	}
	resp, err := s.routeGroup(r.Context(), g, req.ToInternal())
	if err != nil {
		writeRouteError(w, err)
		return
	}
	rec, _ := r.Context().Value(logCtxKey).(*logRecord)
	writeConvertedJSON(w, http.StatusOK, resp.ToResponses(), rec)
}

// directGroupComplete 分组直通(非流式):模型属于分组且支持组格式时转发,否则 false
// 交由 routeGroup 处理(组外模型由 routeGroup 走 404)。label 用组名+"→"前缀。
func (s *Server) directGroupComplete(w http.ResponseWriter, r *http.Request, g group.Config, fullModel string, raw []byte, clientFormat string) bool {
	if !contains(g.Models, fullModel) {
		return false
	}
	return s.directComplete(w, r, fullModel, raw, clientFormat, g.Name+"→")
}

// directGroupStream 分组直通(流式):模型属于分组且支持组格式时转发,否则 false
// 交由 serveGroupStream 处理。
func (s *Server) directGroupStream(w http.ResponseWriter, r *http.Request, g group.Config, fullModel string, raw []byte, clientFormat string) bool {
	if !contains(g.Models, fullModel) {
		return false
	}
	return s.directStream(w, r, fullModel, raw, clientFormat, g.Name+"→")
}

// serveGroupStream 是分组流式转发公共路径:校验模型属于分组、启动上游流、经规范化事件回写 SSE。
func (s *Server) serveGroupStream(w http.ResponseWriter, r *http.Request, g group.Config, req *gateway.Request, clientFormat string) {
	fullModel := req.Model
	body, upFormat, err := s.streamGroupRoute(r.Context(), g, req)
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
		// 客户端未断开记一切错误;客户端已断开仅记源于上游的错误(截断/读体失败)。
		recordStreamError(rec, r.Context(), err)
	}
	if rec != nil {
		rec.convertedResponseBody = captureBody(string(out.(*captureWriter).buf), "")
	}
}

// streamGroupRoute 校验模型属于分组并启动上游流式转发,返回上游 SSE 响应体(调用方负责
// Close)与其接口格式。组内模型为聚合裸名时做故障转移(流开始前可切换到其余成员)。
func (s *Server) streamGroupRoute(ctx context.Context, g group.Config, req *gateway.Request) (io.ReadCloser, string, error) {
	fullModel := req.Model
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.model = fullModel
		rec.kind = string(g.Kind)
	}
	if !contains(g.Models, fullModel) {
		err := fmt.Errorf("%w: model %q is not assigned to group %q", provider.ErrNotFound, fullModel, g.Name)
		if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
			rec.error = captureBody(err.Error(), "")
		}
		return nil, "", err
	}
	// native alias 把裸原生 slug 映射到绑定路由模型;组成员校验用原始请求模型。
	routed := s.resolveAliasedModel(fullModel)
	base := provider.StripContextMarker(routed)
	label, kind := g.Name+"→", string(g.Kind)
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
			body, upFormat, serr = s.streamComplete(ctx, req, fullModel, p, model, label+p.Name(), kind)
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
	return s.streamComplete(ctx, req, fullModel, p, model, label+p.Name(), kind)
}

// routeGroup 校验模型属于分组,并按 "{供应商}-{模型}" 解析真实上游转发,
// 响应模型回填为请求时的分组模型 ID。组内模型为聚合裸名时做故障转移。
func (s *Server) routeGroup(ctx context.Context, g group.Config, req *gateway.Request) (*gateway.Response, error) {
	fullModel := req.Model
	if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
		rec.model = fullModel
		rec.kind = string(g.Kind)
	}
	if !contains(g.Models, fullModel) {
		if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
			// 限制长度,防止客户端注入超长内容撑爆日志。
			rec.error = captureBody(fmt.Sprintf("model %q not assigned to group %q", fullModel, g.Name), "")
		}
		return nil, fmt.Errorf("%w: model %q is not assigned to group %q", provider.ErrNotFound, fullModel, g.Name)
	}
	// native alias 把裸原生 slug 映射到绑定路由模型;组成员校验用原始请求模型。
	routed := s.resolveAliasedModel(fullModel)
	base := provider.StripContextMarker(routed)
	label, kind := g.Name+"→", string(g.Kind)
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
			resp, cerr = s.complete(ctx, req, fullModel, p, model, label+p.Name(), kind)
			return cerr
		})
		return resp, err
	}
	p, model, err := s.resolveModel(routed)
	if err != nil {
		if rec, ok := ctx.Value(logCtxKey).(*logRecord); ok {
			rec.error = captureBody(err.Error(), "")
		}
		markBlocked(ctx, err)
		return nil, err
	}
	return s.complete(ctx, req, fullModel, p, model, label+p.Name(), kind)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
