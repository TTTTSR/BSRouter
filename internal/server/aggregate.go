package server

import (
	"errors"
	"net/http"

	"BSRouter/internal/aggregate"
)

// ---- 聚合模型端点 ----

// handleListAggregates 返回所有聚合模型(成员 + 可添加回成员),按名称排序。
func (s *Server) handleListAggregates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.aggregates.Models())
}

// handleUpdateAggregate 设置聚合模型的成员(body {members:[...]}),持久化剔除名单。
func (s *Server) handleUpdateAggregate(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var req struct {
		Members []string `json:"members"`
	}
	if !decodeJSON(w, r, "aggregate members", &req) {
		return
	}
	if err := s.aggregates.SetMembers(r.PathValue("name"), req.Members); err != nil {
		writeAggregateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "members": req.Members})
}

// writeAggregateError 映射聚合端点错误:不存在 -> 404,持久化失败 -> 500,
// 其余(成员不含该模型等非法输入)-> 400。
func writeAggregateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, aggregate.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, aggregate.ErrPersist):
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}
