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
// message / function_call / function_call_output / reasoning。
// reasoning 是 assistant 的思考内容(deepseek thinking 经 responses 的形态):
// input 回传 / output 生成均以 summary[].text 承载思考文本。
// Content 兼容字符串与内容块数组两种形态(Responses API 规范允许消息 content 为
// 字符串或数组;OpenAI SDK 等客户端对 system/user 消息常发简写形式 {role, content}
// 且省略 type),解码时经 UnmarshalJSON 归一化为内容块数组并补全 type。
type ResponsesItem struct {
	Type      string             `json:"type"`
	Role      string             `json:"role,omitempty"`
	Content   []ResponsesContent `json:"content,omitempty"`
	Summary   []ResponsesContent `json:"summary,omitempty"`
	CallID    string             `json:"call_id,omitempty"`
	Name      string             `json:"name,omitempty"`
	Arguments string             `json:"arguments,omitempty"` // JSON 字符串
	Output    string             `json:"output,omitempty"`
}

// UnmarshalJSON 兼容 content 为字符串或内容块数组两种形态,并给简写消息
// ({role, content} 且省略 type)补全 type="message",避免消息被静默丢弃。
func (it *ResponsesItem) UnmarshalJSON(data []byte) error {
	type raw ResponsesItem // 别名避免递归调用本方法
	var p struct {
		raw
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*it = ResponsesItem(p.raw)
	it.Content = responsesContentToBlocks(p.Content)
	if it.Type == "" && it.Role != "" {
		it.Type = "message"
	}
	return nil
}

// responsesContentToBlocks 把 responses content 字段(string、null 或内容块数组)
// 归一化为内容块数组:字符串转单块,数组原样;无法识别时为空。
func responsesContentToBlocks(data json.RawMessage) []ResponsesContent {
	if len(data) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ResponsesContent{{Type: "text", Text: s}}
	}
	var blocks []ResponsesContent
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil // 非法形态按空处理,不阻断转发
	}
	return blocks
}

// ResponsesTool 是 responses 的工具定义(扁平结构)。
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ResponsesReasoning 是 responses 请求的思考启用配置(reasoning 字段)。
type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
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
	// Reasoning 是思考启用参数(pi 等客户端发 reasoning:{effort})。
	Reasoning *ResponsesReasoning `json:"reasoning,omitempty"`
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
	if r.Reasoning != nil {
		out.ReasoningEffort = r.Reasoning.Effort
	}
	for _, it := range r.Input {
		switch it.Type {
		case "message":
			var text string
			for _, c := range it.Content {
				text += c.Text
			}
			if it.Role == "system" || it.Role == "developer" {
				// developer 与 system 同为指令角色(Responses API 标准):并入顶层 System,
				// 避免 developer 作为消息角色原样透传给不认该角色的上游
				// (如 chat.completions 严格校验的 opencode.ai,会以 400 拒绝)。
				if out.System != "" {
					out.System += "\n\n"
				}
				out.System += text
				continue
			}
			msg := Message{Role: Role(it.Role), Content: text}
			// 若上一条是"由 reasoning item 开启"的 assistant(带思考内容),把消息文本并入
			// 同一条 assistant(思考 + 文本配对),避免拆成两条 assistant 导致 thinking 上游
			// (如 opencode.ai)要求 reasoning_content 与其所属内容在同一消息内而拒绝。
			if n := len(out.Messages); n > 0 && msg.Role == RoleAssistant {
				last := &out.Messages[n-1]
				if last.Role == RoleAssistant && last.Reasoning != "" {
					last.Content = text
					continue
				}
			}
			out.Messages = append(out.Messages, msg)
		case "function_call":
			msg := Message{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: it.CallID, Name: it.Name, Arguments: json.RawMessage(it.Arguments)},
			}}
			// 若上一条是"由 reasoning item 开启"的 assistant(带思考内容,可能已并入文本),
			// 把工具调用并入同一条(思考 + 内容 + tool_calls 全部配对)——deepseek thinking
			// 上游要求 reasoning_content 与所属内容/工具调用在同一 assistant 消息内,否则 400。
			// 连续多个 function_call 全部并入(保持 reasoning 与整组工具调用配对)。
			if n := len(out.Messages); n > 0 {
				last := &out.Messages[n-1]
				if last.Role == RoleAssistant && last.Reasoning != "" {
					last.ToolCalls = append(last.ToolCalls, msg.ToolCalls...)
					continue
				}
			}
			out.Messages = append(out.Messages, msg)
		case "function_call_output":
			out.Messages = append(out.Messages, Message{Role: RoleTool, ToolCallID: it.CallID, Content: it.Output})
		case "reasoning":
			// codex 等 responses 客户端会回传历史 reasoning item(deepseek thinking 要求
			// 原样回传);提取 summary/content 文本作为 assistant 思考内容,转 completion
			// 时输出为 reasoning_content,否则上游以 400 拒绝。
			var text string
			for _, s := range it.Summary {
				text += s.Text
			}
			for _, c := range it.Content {
				text += c.Text
			}
			if text == "" {
				continue
			}
			out.Messages = append(out.Messages, Message{Role: RoleAssistant, Reasoning: text})
		}
	}
	for _, t := range r.Tools {
		// 仅转换 function 类型工具:responses API 的内置工具(如 web_search_preview)
		// 没有 name,无法映射为其它格式的 function 工具,硬转会产出空名 function
		// 被严格上游(如 opencode.ai)以 400 拒绝。非 function 类型直接跳过。
		if t.Type != "" && t.Type != "function" {
			continue
		}
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
	if r.ReasoningEffort != "" {
		// 启用思考:转发为 responses 的 reasoning:{effort, summary}。
		out.Reasoning = &ResponsesReasoning{Effort: r.ReasoningEffort, Summary: "auto"}
	}
	for _, m := range r.Messages {
		switch m.Role {
		case RoleTool:
			out.Input = append(out.Input, ResponsesItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
		case RoleAssistant:
			if m.Reasoning != "" {
				// assistant 思考内容作为独立 reasoning item(summary 承载)。
				out.Input = append(out.Input, ResponsesItem{
					Type:    "reasoning",
					Summary: []ResponsesContent{{Type: "summary_text", Text: m.Reasoning}},
				})
			}
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
	baseURL  string
	basePath string
	apiKey   string
	httpc    *http.Client
}

// NewResponsesProvider 基于共享配置构造 responses 适配器。
func NewResponsesProvider(c *Client) *ResponsesProvider {
	baseURL, basePath, apiKey, httpc := resolveClient(c, "https://api.openai.com")
	return &ResponsesProvider{baseURL: baseURL, basePath: basePath, apiKey: apiKey, httpc: httpc}
}

// Complete 实现 Provider 接口。
func (p *ResponsesProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, errors.New("gateway: nil request")
	}
	wire := req.ToResponses()
	var resp ResponsesResponse
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	if err := doJSON(ctx, p.httpc, http.MethodPost, joinPath(p.baseURL, p.basePath, "/responses"), headers, wire, &resp); err != nil {
		return nil, err
	}
	return resp.ToInternal(), nil
}

// CompleteRaw 发送原始 wire 请求体(直通)并返回原始响应体与上游状态码,不解析。
func (p *ResponsesProvider) CompleteRaw(ctx context.Context, raw json.RawMessage) (int, []byte, error) {
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	return doRaw(ctx, p.httpc, http.MethodPost, joinPath(p.baseURL, p.basePath, "/responses"), headers, raw)
}

// Stream 发送流式请求(stream:true)并返回上游 SSE 响应体,调用方负责 Close。
func (p *ResponsesProvider) Stream(ctx context.Context, req *Request) (io.ReadCloser, error) {
	if req == nil {
		return nil, errors.New("gateway: nil request")
	}
	wire := req.ToResponses()
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	resp, err := doStream(ctx, p.httpc, http.MethodPost, joinPath(p.baseURL, p.basePath, "/responses"), headers, wire)
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
func (p *ResponsesProvider) StreamRaw(ctx context.Context, raw json.RawMessage) (*http.Response, error) {
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}
	return doStream(ctx, p.httpc, http.MethodPost, joinPath(p.baseURL, p.basePath, "/responses"), headers, json.RawMessage(raw))
}

// Do 实现 Requester 接口:responses 请求体可直接发起请求。
func (r *ResponsesRequest) Do(ctx context.Context, c *Client) (*Response, error) {
	return NewResponsesProvider(c).Complete(ctx, r.ToInternal())
}
