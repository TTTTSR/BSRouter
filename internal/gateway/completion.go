package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
type CompletionMessage struct {
	Role       string               `json:"role"`
	Content    string               `json:"content"`
	ToolCalls  []CompletionToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
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
}

// ToInternal 将 chat.completions 请求体转换为规范化请求。
func (r *CompletionRequest) ToInternal() *Request {
	out := &Request{
		Model:       r.Model,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        r.Stop,
		Stream:      r.Stream,
	}
	for _, m := range r.Messages {
		if m.Role == "system" {
			if out.System != "" {
				out.System += "\n\n"
			}
			out.System += m.Content
			continue
		}
		msg := Message{Role: Role(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
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
		Model:       r.Model,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        r.Stop,
		Stream:      r.Stream,
	}
	if r.System != "" {
		out.Messages = append(out.Messages, CompletionMessage{Role: string(RoleSystem), Content: r.System})
	}
	for _, m := range r.Messages {
		cm := CompletionMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
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
		if t.Function != nil {
			out.Tools = append(out.Tools, CompletionTool{
				Type: "function",
				Function: CompletionFunction{
					Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
				},
			})
		}
	}
	return out
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

// Do 实现 Requester 接口:chat.completions 请求体可直接发起请求。
func (r *CompletionRequest) Do(ctx context.Context, c *Client) (*Response, error) {
	return NewCompletionProvider(c).Complete(ctx, r.ToInternal())
}
