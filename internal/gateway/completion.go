package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Completion(OpenAI chat.completions)相关 wire 类型与转换。
// 端点:POST {base}/v1/chat/completions,header 携带 Authorization: Bearer。

// CompletionFunctionCall 是 chat.completions 中一次函数调用的参数(Arguments 为 JSON 字符串)。
type CompletionFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// CompletionToolCall 是 assistant 消息中的工具调用。
type CompletionToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function CompletionFunctionCall `json:"function"`
}

// CompletionMessage 是 chat.completions 对话消息。
// Content 兼容字符串与内容块数组两种形态(OpenAI 规范允许 content 为字符串或
// 多模态内容块数组;OpenAI SDK 等客户端对部分消息角色总是发数组),解码时经
// UnmarshalJSON 归一化为纯文本字符串,规范中间类型仅承载文本。
// ReasoningContent 是 deepseek thinking 模式的思考内容:上游返回它、多轮对话要求原样回传。
// 用指针以区分"无思考"(缺省)与"思考为空但字段必须存在"(thinking 上游的 400 校验)。
type CompletionMessage struct {
	Role             string               `json:"role"`
	Content          string               `json:"content"`
	ReasoningContent *string              `json:"reasoning_content,omitempty"`
	ToolCalls        []CompletionToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
}

// UnmarshalJSON 兼容 content 为字符串或内容块数组两种形态,归一化为纯文本。
// 其余字段按默认规则解码;content 单独以 RawMessage 收取后转文本,避免
// "cannot unmarshal array into string" 拒绝整条消息。
func (m *CompletionMessage) UnmarshalJSON(data []byte) error {
	type raw CompletionMessage // 别名避免递归调用本方法
	var p struct {
		raw
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*m = CompletionMessage(p.raw)
	m.Content = completionContentToText(p.Content)
	return nil
}

// CompletionContentPart 是 OpenAI content 内容块(仅关注文本块,其余类型忽略)。
type CompletionContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// completionContentToText 把 OpenAI content 字段(string、null 或内容块数组)
// 归一化为纯文本:字符串原样返回;数组提取所有 text 块并以换行连接;无法识别时为空。
func completionContentToText(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return s // 字符串或 null(空串)
	}
	var parts []CompletionContentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return "" // 非法形态按空处理,不阻断转发
	}
	var text strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(p.Text)
		}
	}
	return text.String()
}

// CompletionFunction 是 chat.completions 的工具函数定义(嵌套在 tools[] 中)。
type CompletionFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// CompletionTool 是 chat.completions 工具定义。
type CompletionTool struct {
	Type     string             `json:"type"`
	Function CompletionFunction `json:"function"`
}

// CompletionRequest 是 chat.completions 请求体。
type CompletionRequest struct {
	Model       string              `json:"model"`
	Messages    []CompletionMessage `json:"messages"`
	Tools       []CompletionTool    `json:"tools,omitempty"`
	MaxTokens   *int                `json:"max_tokens,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
	TopP        *float64            `json:"top_p,omitempty"`
	Stop        []string            `json:"stop,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	// ReasoningEffort 是 OpenAI 风格的思考档位(pi 对 opencode 等上游发顶层 reasoning_effort)。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// ToInternal 将 chat.completions 请求体转换为规范化请求。
func (r *CompletionRequest) ToInternal() *Request {
	out := &Request{
		Model:           r.Model,
		MaxTokens:       r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		Stop:            r.Stop,
		Stream:          r.Stream,
		ReasoningEffort: r.ReasoningEffort,
	}
	for _, m := range r.Messages {
		if m.Role == "system" || m.Role == "developer" {
			// developer 与 system 同为指令角色:并入顶层 System(见 responses.go 同类处理),
			// 避免 developer 作为消息角色原样透传给不认该角色的上游。
			if out.System != "" {
				out.System += "\n\n"
			}
			out.System += m.Content
			continue
		}
		msg := Message{Role: Role(m.Role), Content: m.Content, ToolCallID: m.ToolCallID, Reasoning: ""}
		if m.ReasoningContent != nil {
			msg.Reasoning = *m.ReasoningContent
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Arguments != "" {
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: json.RawMessage(tc.Function.Arguments)})
			}
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, Tool{Function: &FunctionTool{
			Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
		}})
	}
	return out
}

// ToCompletion 将规范化请求转换为 chat.completions 请求体。
func (r *Request) ToCompletion() *CompletionRequest {
	out := &CompletionRequest{
		Model:           r.Model,
		MaxTokens:       r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		Stop:            r.Stop,
		Stream:          r.Stream,
		ReasoningEffort: r.ReasoningEffort,
	}
	if r.System != "" {
		out.Messages = append(out.Messages, CompletionMessage{Role: string(RoleSystem), Content: r.System})
	}
	// deepseek thinking 模式校验:带 tools 的请求中,工具调用轮的 assistant 消息必须把
	// reasoning_content 原样回传,缺失即 400("The `reasoning_content` in the thinking mode
	// must be passed back to the API.")。只要会话内存在思考内容(thinking 模式),就给所有
	// assistant 消息补上该字段(内容缺失时为 ""),保证字段存在、校验通过。
	fillReasoning := len(r.Tools) > 0 && conversationHasReasoning(r.Messages)
	for _, m := range r.Messages {
		cm := CompletionMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
		if m.Role == RoleAssistant && (m.Reasoning != "" || fillReasoning) {
			// deepseek thinking 模式要求把历史 reasoning_content 原样回传。
			rc := m.Reasoning
			cm.ReasoningContent = &rc
		}
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, CompletionToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: CompletionFunctionCall{Name: tc.Name, Arguments: string(tc.Arguments)},
			})
		}
		out.Messages = append(out.Messages, cm)
	}
	for _, t := range r.Tools {
		// 兜底:过滤空名工具,防止空 function.name 被严格上游拒绝。
		if t.Function == nil || t.Function.Name == "" {
			continue
		}
		out.Tools = append(out.Tools, CompletionTool{
			Type: "function",
			Function: CompletionFunction{
				Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
			},
		})
	}
	return out
}

// conversationHasReasoning 判断会话中是否存在思考内容(thinking 模式)。
func conversationHasReasoning(msgs []Message) bool {
	for _, m := range msgs {
		if m.Role == RoleAssistant && m.Reasoning != "" {
			return true
		}
	}
	return false
}

// CompletionUsage 是 chat.completions 响应的 token 用量。
type CompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CompletionChoice 是 chat.completions 响应中的一个候选。
type CompletionChoice struct {
	Index        int               `json:"index"`
	Message      CompletionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

// CompletionResponse 是 chat.completions 响应体。
type CompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   CompletionUsage    `json:"usage"`
}

// ToInternal 将 chat.completions 响应体转换为规范化响应。
func (r *CompletionResponse) ToInternal() *Response {
	out := &Response{
		Model: r.Model,
		Usage: Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens},
	}
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		out.Content = c.Message.Content
		for _, tc := range c.Message.ToolCalls {
			if tc.Function.Arguments != "" {
				out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: json.RawMessage(tc.Function.Arguments)})
			}
		}
		if c.FinishReason == "tool_calls" {
			out.FinishReason = "tool_use"
		} else {
			out.FinishReason = c.FinishReason
		}
	}
	return out
}

// ToCompletion 将规范化响应转换为 chat.completions 响应体。
// 注意:ID/Object/Created 等元信息无法从规范化响应还原,置零值。
func (r *Response) ToCompletion() *CompletionResponse {
	out := &CompletionResponse{Model: r.Model}
	out.Usage.PromptTokens = r.Usage.InputTokens
	out.Usage.CompletionTokens = r.Usage.OutputTokens
	out.Choices = []CompletionChoice{{
		Index:   0,
		Message: CompletionMessage{Role: string(RoleAssistant), Content: r.Content},
	}}
	for _, tc := range r.ToolCalls {
		out.Choices[0].Message.ToolCalls = append(out.Choices[0].Message.ToolCalls, CompletionToolCall{
			ID:       tc.ID,
			Type:     "function",
			Function: CompletionFunctionCall{Name: tc.Name, Arguments: string(tc.Arguments)},
		})
	}
	if r.FinishReason == "tool_use" {
		out.Choices[0].FinishReason = "tool_calls"
	} else {
		out.Choices[0].FinishReason = r.FinishReason
	}
	return out
}

// CompletionProvider 通过 OpenAI chat.completions 接口发送请求。
type CompletionProvider struct {
	baseURL string
	apiKey  string
	httpc   *http.Client
}

// NewCompletionProvider 基于共享配置构造 chat.completions 适配器。
func NewCompletionProvider(c *Client) *CompletionProvider {
	baseURL, apiKey, httpc := resolveClient(c, "https://api.openai.com")
	return &CompletionProvider{baseURL: baseURL, apiKey: apiKey, httpc: httpc}
}

// Complete 实现 Provider 接口。
func (p *CompletionProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, errors.New("gateway: nil request")
	}
	wire := req.ToCompletion()
	var resp CompletionResponse
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	if err := doJSON(ctx, p.httpc, http.MethodPost, p.baseURL+"/v1/chat/completions", headers, wire, &resp); err != nil {
		return nil, err
	}
	return resp.ToInternal(), nil
}

// CompleteRaw 发送原始 wire 请求体(直通)并返回原始响应体与上游状态码,不解析。
func (p *CompletionProvider) CompleteRaw(ctx context.Context, raw json.RawMessage) (int, []byte, error) {
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	return doRaw(ctx, p.httpc, http.MethodPost, p.baseURL+"/v1/chat/completions", headers, raw)
}

// Stream 发送流式请求(stream:true)并返回上游 SSE 响应体,调用方负责 Close。
func (p *CompletionProvider) Stream(ctx context.Context, req *Request) (io.ReadCloser, error) {
	if req == nil {
		return nil, errors.New("gateway: nil request")
	}
	wire := req.ToCompletion()
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	resp, err := doStream(ctx, p.httpc, http.MethodPost, p.baseURL+"/v1/chat/completions", headers, wire)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
		return nil, &apiError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	return resp.Body, nil
}

// StreamRaw 发送原始流式请求体(直通)并返回上游 SSE 响应体,调用方负责 Close。
// 非 2xx 由调用方检查 resp.StatusCode 并读取/关闭响应体。
func (p *CompletionProvider) StreamRaw(ctx context.Context, raw json.RawMessage) (*http.Response, error) {
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	return doStream(ctx, p.httpc, http.MethodPost, p.baseURL+"/v1/chat/completions", headers, json.RawMessage(raw))
}

// Do 实现 Requester 接口:chat.completions 请求体可直接发起请求。
func (r *CompletionRequest) Do(ctx context.Context, c *Client) (*Response, error) {
	return NewCompletionProvider(c).Complete(ctx, r.ToInternal())
}
