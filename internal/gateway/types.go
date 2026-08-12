// Package gateway 提供大模型接入层:统一规范化请求/响应中间类型,
// 以及 Anthropic / OpenAI chat.completions / OpenAI responses 三种接口格式的
// wire 结构、互相转换与 Provider 适配器。
package gateway

import "encoding/json"

// Role 表示规范化对话消息的角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 表示一次工具调用(由 assistant 发起)。
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// FunctionTool 定义一个 function 类型的工具。
type FunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Tool 是工具的规范化表示,当前支持 function 类型。
type Tool struct {
	Function *FunctionTool `json:"function"`
}

// Message 是规范化对话消息。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Reasoning 是 assistant 消息的思考内容(deepseek thinking 模式的 reasoning_content)。
	// 多轮对话中上游要求把历史 reasoning 原样回传,故在 canonical 层保留并在各格式间透传。
	Reasoning string `json:"reasoning,omitempty"`
}

// Usage 记录一次请求的 token 用量。缓存字段仅部分格式携带(Anthropic / 兼容上游),
// OpenAI 格式用 prompt_tokens_details 表达,流式转换时互相映射。
type Usage struct {
	InputTokens             int `json:"input_tokens"`
	OutputTokens            int `json:"output_tokens"`
	CacheReadInputTokens    int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// Request 是规范化请求(中间类型),三种接口格式共享。
type Request struct {
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	// ReasoningEffort 是"启用思考"的档位名(如 low/medium/high/xhigh/max),空表示不启用。
	// 各格式的思考启用参数形态不同(anthropic thinking.budget_tokens、completion
	// reasoning_effort、responses reasoning.effort),经中间层归一化为统一的档位名,
	// 跨格式转发时再映射回目标格式——否则思考启用参数在转换路径被丢弃,上游不会思考。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// Response 是规范化响应(中间类型),三种接口格式共享。
type Response struct {
	Model        string     `json:"model"`
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Usage        Usage      `json:"usage"`
}

// effortToBudget 把统一思考档位名映射为 Anthropic 的 thinking.budget_tokens。
// 启发式取值,与 pi 的 thinkingBudgets 同量级;默认给中等预算。
func effortToBudget(effort string) int {
	switch effort {
	case "low":
		return 2048
	case "medium":
		return 4096
	case "high":
		return 8192
	case "xhigh":
		return 16384
	case "max":
		return 32768
	default:
		return 4096
	}
}

// budgetToEffort 把 Anthropic 的 thinking.budget_tokens 映射为统一档位名。
// 启发式:小预算(含 pi 默认 1024)视为中等思考,确保转发后仍启用思考。
func budgetToEffort(budget int) string {
	switch {
	case budget >= 16384:
		return "xhigh"
	case budget >= 8192:
		return "high"
	default:
		return "medium"
	}
}
