package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Response(OpenAI responses API)相关 wire 类型与转换。
// 端点:POST {base}/v1/responses,header 携带 Authorization: Bearer。
// 与 chat.completions 不同:responses 的 input/output 都是 items 数组,
// 且工具 schema 为扁平结构(非嵌套在 function 下)。

// ResponsesContent 是 responses 消息条目中的内容块(input_text / output_text)。
type ResponsesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResponsesItem 是 responses 的 input/output 条目,类型由 Type 区分:
// message / function_call / function_call_output。
type ResponsesItem struct {
	Type      string             `json:"type"`
	Role      string             `json:"role,omitempty"`
	Content   []ResponsesContent `json:"content,omitempty"`
	CallID    string             `json:"call_id,omitempty"`
	Name      string             `json:"name,omitempty"`
	Arguments string             `json:"arguments,omitempty"` // JSON 字符串
	Output    string             `json:"output,omitempty"`
}

// ResponsesTool 是 responses 的工具定义(扁平结构)。
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ResponsesRequest 是 responses 请求体。
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions,omitempty"`
	Input           []ResponsesItem `json:"input"`
	Tools           []ResponsesTool `json:"tools,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
}

// ToInternal 将 responses 请求体转换为规范化请求。
func (r *ResponsesRequest) ToInternal() *Request {
	out := &Request{
		Model:       r.Model,
		System:      r.Instructions,
		MaxTokens:   r.MaxOutputTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        r.Stop,
		Stream:      r.Stream,
	}
	for _, it := range r.Input {
		switch it.Type {
		case "message":
			var text string
			for _, c := range it.Content {
				text += c.Text
			}
			if it.Role == "system" {
				if out.System != "" {
					out.System += "\n\n"
				}
				out.System += text
				continue
			}
			out.Messages = append(out.Messages, Message{Role: Role(it.Role), Content: text})
		case "function_call":
			out.Messages = append(out.Messages, Message{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: it.CallID, Name: it.Name, Arguments: json.RawMessage(it.Arguments)},
			}})
		case "function_call_output":
			out.Messages = append(out.Messages, Message{Role: RoleTool, ToolCallID: it.CallID, Content: it.Output})
		}
	}
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, Tool{Function: &FunctionTool{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		}})
	}
	return out
}

// ToResponses 将规范化请求转换为 responses 请求体。
func (r *Request) ToResponses() *ResponsesRequest {
	out := &ResponsesRequest{
		Model:           r.Model,
		Instructions:    r.System,
		MaxOutputTokens: r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		Stop:            r.Stop,
		Stream:          r.Stream,
	}
	for _, m := range r.Messages {
		switch m.Role {
		case RoleTool:
			out.Input = append(out.Input, ResponsesItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
		case RoleAssistant:
			if m.Content != "" {
				out.Input = append(out.Input, ResponsesItem{
					Type: "message", Role: string(RoleAssistant),
					Content: []ResponsesContent{{Type: "output_text", Text: m.Content}},
				})
			}
			for _, tc := range m.ToolCalls {
				out.Input = append(out.Input, ResponsesItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: string(tc.Arguments)})
			}
		default:
			out.Input = append(out.Input, ResponsesItem{
				Type: "message", Role: string(m.Role),
				Content: []ResponsesContent{{Type: "input_text", Text: m.Content}},
			})
		}
	}
	for _, t := range r.Tools {
		if t.Function != nil {
			out.Tools = append(out.Tools, ResponsesTool{
				Type:        "function",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
	}
	return out
}

// ResponsesUsage 是 responses 响应的 token 用量。
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponsesResponse 是 responses 响应体。
type ResponsesResponse struct {
	ID     string          `json:"id"`
	Object string          `json:"object"`
	Model  string          `json:"model"`
	Status string          `json:"status"`
	Output []ResponsesItem `json:"output"`
	Usage  ResponsesUsage  `json:"usage"`
}

// ToInternal 将 responses 响应体转换为规范化响应。
// 注意:responses 的 wire 中没有 finish_reason 字段,只有 status;
// 因此当响应包含工具调用时无法从 status 还原 tool_use,按 stop 处理。
func (r *ResponsesResponse) ToInternal() *Response {
	out := &Response{
		Model: r.Model,
		Usage: Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens},
	}
	for _, it := range r.Output {
		switch it.Type {
		case "message":
			var text string
			for _, c := range it.Content {
				text += c.Text
			}
			out.Content += text
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: it.CallID, Name: it.Name, Arguments: json.RawMessage(it.Arguments)})
		}
	}
	switch r.Status {
	case "completed":
		out.FinishReason = "stop"
	case "incomplete":
		out.FinishReason = "length"
	}
	return out
}

// ToResponses 将规范化响应转换为 responses 响应体。
// 注意:ID/Object/Status 等元信息无法从规范化响应完整还原;FinishReason 为 length
// 时映射为 status=incomplete,其余为 completed。
func (r *Response) ToResponses() *ResponsesResponse {
	out := &ResponsesResponse{Model: r.Model, Status: "completed"}
	out.Usage.InputTokens = r.Usage.InputTokens
	out.Usage.OutputTokens = r.Usage.OutputTokens
	if r.Content != "" {
		out.Output = append(out.Output, ResponsesItem{
			Type: "message", Role: string(RoleAssistant),
			Content: []ResponsesContent{{Type: "output_text", Text: r.Content}},
		})
	}
	for _, tc := range r.ToolCalls {
		out.Output = append(out.Output, ResponsesItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: string(tc.Arguments)})
	}
	if r.FinishReason == "length" {
		out.Status = "incomplete"
	}
	return out
}

// ResponsesProvider 通过 OpenAI responses 接口发送请求。
type ResponsesProvider struct {
	baseURL string
	apiKey  string
	httpc   *http.Client
}

// NewResponsesProvider 基于共享配置构造 responses 适配器。
func NewResponsesProvider(c *Client) *ResponsesProvider {
	baseURL, apiKey, httpc := resolveClient(c, "https://api.openai.com")
	return &ResponsesProvider{baseURL: baseURL, apiKey: apiKey, httpc: httpc}
}

// Complete 实现 Provider 接口。
func (p *ResponsesProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, errors.New("gateway: nil request")
	}
	wire := req.ToResponses()
	var resp ResponsesResponse
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	if err := doJSON(ctx, p.httpc, http.MethodPost, p.baseURL+"/v1/responses", headers, wire, &resp); err != nil {
		return nil, err
	}
	return resp.ToInternal(), nil
}

// Stream 发送流式请求(stream:true)并返回上游 SSE 响应体,调用方负责 Close。
func (p *ResponsesProvider) Stream(ctx context.Context, req *Request) (io.ReadCloser, error) {
	if req == nil {
		return nil, errors.New("gateway: nil request")
	}
	wire := req.ToResponses()
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	resp, err := doStream(ctx, p.httpc, http.MethodPost, p.baseURL+"/v1/responses", headers, wire)
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

// Do 实现 Requester 接口:responses 请求体可直接发起请求。
func (r *ResponsesRequest) Do(ctx context.Context, c *Client) (*Response, error) {
	return NewResponsesProvider(c).Complete(ctx, r.ToInternal())
}
