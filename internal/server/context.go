package server

import (
	"strings"

	"BSRouter/internal/claude"
	"BSRouter/internal/provider"
)

// defaultContextWindowK 是模型未配置 context_window 时的默认窗口(k 为单位,200k),
// 与 BSRouter 对未配置窗口模型的全库约定一致(Claude Code 自定义模型 / codex 目录 /
// zcode limit.context 均回退 200k)。聚合取最小值的上界也用它:未配置成员的窗口
// 按此默认参与取小,保证聚合路由到该成员也不超窗。
const defaultContextWindowK = 200

// modelContextWindowK 返回模型 id 配置的上下文窗口(k 为单位)。模型 id 形如
// "{供应商}@{模型}";聚合裸名(无 @)时取全部有效成员的最小值(见
// aggregateContextWindowK);供应商不存在或模型未配置 context_window 时返回 0
// (调用方按默认 200k 处理)。先剥离上下文标记再解析,兼容带 [Nk]/[1m] 的 id。
func (s *Server) modelContextWindowK(model string) int {
	name, rest, ok := strings.Cut(provider.StripContextMarker(model), "@")
	if !ok || rest == "" {
		return s.aggregateContextWindowK(name) // 聚合裸名:成员窗口的最小值
	}
	p, err := s.mgr.Get(name)
	if err != nil {
		return 0
	}
	for _, m := range p.Models() {
		if m.Name == rest {
			return m.ContextWindow
		}
	}
	return 0
}

// aggregateContextWindowK 返回聚合裸名的上下文窗口(k):有效成员模型配置窗口的
// 最小值(语义见 membersContextWindowK)。非聚合(无成员)/聚合未启用时返回 0。
func (s *Server) aggregateContextWindowK(name string) int {
	if s.aggregates == nil {
		return 0
	}
	members, ok := s.aggregates.Members(name)
	if !ok {
		return 0
	}
	return s.membersContextWindowK(name, members)
}

// membersContextWindowK 由聚合的有效成员列表计算上下文窗口(k):各成员配置窗口的
// 最小值。聚合故障转移/负载均衡会路由到任一有效成员,取最小才能保证任意成员都不
// 超窗;剔除的供应商不在成员表内,天然不参与。**故障被禁用的成员**(余额不足等,
// fault.Block)当前不可路由,不参与取小——窗口声明服务于"路由到任一成员不超窗",
// 与 faultFilteredOrder 的路由可及集合一致;冷却中成员仍计入(10 分钟级瞬时状态,
// 窗口是配置派生属性,不随运行时冷却波动)。未配置窗口的成员按默认 200k 参与取小
// 作为上界;全部(可路由)成员未配置时返回 0(调用方按默认 200k 处理,与单模型
// "未配置=0"语义一致)。
func (s *Server) membersContextWindowK(name string, members []string) int {
	minK := 0
	hasConfigured := false
	hasUnconfigured := false
	for _, p := range members {
		if s.faults != nil {
			if _, blocked := s.faults.Block(p); blocked {
				continue // 被故障禁用的成员当前不可路由,不拖低窗口
			}
		}
		cfg, err := s.mgr.Get(p)
		if err != nil {
			continue // 成员供应商已被删(聚合成员表理论一致,防御处理)
		}
		for _, m := range cfg.Models() {
			if m.Name != name {
				continue
			}
			if m.ContextWindow > 0 {
				if !hasConfigured || m.ContextWindow < minK {
					minK = m.ContextWindow
				}
				hasConfigured = true
			} else {
				hasUnconfigured = true
			}
			break
		}
	}
	if !hasConfigured {
		return 0
	}
	if hasUnconfigured && minK > defaultContextWindowK {
		minK = defaultContextWindowK
	}
	return minK
}

// modelContextWindows 返回模型 id 列表对应的上下文窗口映射(id → tokens),
// 供 Codex 模型目录按模型精确同步;未配置窗口的模型不在映射中(目录回退默认 200k)。
func (s *Server) modelContextWindows(models []string) map[string]int {
	out := make(map[string]int, len(models))
	for _, id := range models {
		if k := s.modelContextWindowK(id); k > 0 {
			out[id] = k * 1000
		}
	}
	return out
}

// syncModelContextSuffix 为单个模型名追加上下文窗口后缀(Claude Code 识别):
// 先剥离已有标记,再按配置的窗口重新生成;未配置窗口时保留原显式标记(兼容旧 [1M]
// 预设),否则不加后缀(Claude Code 对自定义模型默认 200k)。
func (s *Server) syncModelContextSuffix(model string) string {
	if strings.TrimSpace(model) == "" {
		return model
	}
	base, marker := splitContextMarker(model)
	if k := s.modelContextWindowK(base); k > 0 {
		return base + claude.ContextSuffix(k)
	}
	if marker != "" {
		return base + marker
	}
	return base
}

// syncClaudeContextWindow 对预设的所有模型字段应用上下文窗口后缀同步,使 /command
// 与 apply-local 生成的 ANTHROPIC_*_MODEL 携带正确的 [Nk]/[1m] 声明。
func (s *Server) syncClaudeContextWindow(cfg claude.Config) claude.Config {
	cfg.Model = s.syncModelContextSuffix(cfg.Model)
	cfg.SubagentModel = s.syncModelContextSuffix(cfg.SubagentModel)
	cfg.SmallFastModel = s.syncModelContextSuffix(cfg.SmallFastModel)
	cfg.FableModel = s.syncModelContextSuffix(cfg.FableModel)
	cfg.OpusModel = s.syncModelContextSuffix(cfg.OpusModel)
	cfg.SonnetModel = s.syncModelContextSuffix(cfg.SonnetModel)
	cfg.HaikuModel = s.syncModelContextSuffix(cfg.HaikuModel)
	return cfg
}
