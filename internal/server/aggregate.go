package server

import (
	"errors"
	"fmt"
	"net/http"

	"BSRouter/internal/aggregate"
)

// ---- 聚合模型端点 ----

// handleListAggregates 返回所有聚合模型(成员 + 可添加回成员),按名称排序。
func (s *Server) handleListAggregates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.aggregates.Models())
}

// handleUpdateAggregate 设置聚合模型的成员与负载均衡开关(body {members:[...],
// load_balance:bool} 均可选,只更新提供的字段),持久化剔除名单/优先级/开关。
// 现有客户端只发 {members} 仍兼容(不改负载均衡开关)。
func (s *Server) handleUpdateAggregate(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	var req struct {
		Members     *[]string `json:"members"`
		LoadBalance *bool     `json:"load_balance"`
	}
	if !decodeJSON(w, r, "aggregate update", &req) {
		return
	}
	name := r.PathValue("name")
	if req.Members == nil && req.LoadBalance == nil {
		writeError(w, http.StatusBadRequest, errors.New("nothing to update"))
		return
	}
	// 先确认聚合存在(404),再按提供的字段更新。
	if _, ok := s.aggregates.Members(name); !ok {
		writeAggregateError(w, fmt.Errorf("%w: %s", aggregate.ErrNotFound, name))
		return
	}
	members := []string{}
	if req.Members != nil {
		members = *req.Members
		if err := s.aggregates.SetMembers(name, members); err != nil {
			writeAggregateError(w, err)
			return
		}
	}
	if req.LoadBalance != nil {
		if err := s.aggregates.SetLoadBalance(name, *req.LoadBalance); err != nil {
			writeAggregateError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "members": members, "load_balance": s.aggregates.LoadBalanceOf(name)})
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
