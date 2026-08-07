package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Anthropic 相关 wire 类型与转换。
// 端点:POST {base}/v1/messages,header 需携带 x-api-key 与 anthropic-version。

// AnthropicContentBlock 是 Anthropic 消息内容块(text / tool_use / tool_result)。
// 注意:tool_result 的 payload 在 wire 上位于 content 键(规范要求,string 或块数组),
// 而非 text 键;text 键仅供 text 块使用。
type AnthropicContentBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	Content   AnthropicContent `json:"content,omitempty"` // tool_result 内容(string 或块数组)
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
}

// AnthropicContent 兼容 Anthropic 的两种 content 形式:string 与 blocks 数组。
type AnthropicContent []AnthropicContentBlock

func (c AnthropicContent) MarshalJSON() ([]byte, error) {
	if len(c) == 1 && c[0].Type == "text" {
		return json.Marshal(c[0].Text)
	}
	return json.Marshal([]AnthropicContentBlock(c))
}

func (c *AnthropicContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*c = AnthropicContent{{Type: "text", Text: s}}
		return nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	*c = blocks
	return nil
}

// AnthropicMessage 是 Anthropic 对话消息(user / assistant;工具结果以 user + tool_result 块表达)。
type AnthropicMessage struct {
	Role    string           `json:"role"`
	Content AnthropicContent `json:"content"`
}

// AnthropicTool 是 Anthropic 工具定义(input_schema 为 JSON Schema)。
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicSystem 兼容 Anthropic 的 system 字段两种形式:string 或 text 内容块数组
// (TextBlockParam,可带 cache_control)。归一化为纯文本字符串,对上游始终以 string 输出;
// 底层为 string 类型,故 omitempty 对空值仍生效。
type AnthropicSystem string

// UnmarshalJSON 接受 string 或 text 内容块数组;非 text 块跳过。
func (s *AnthropicSystem) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = AnthropicSystem(str)
		return nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return fmt.Errorf("gateway: system must be a string or an array of text blocks: %w", err)
	}
	var text strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	*s = AnthropicSystem(text.String())
	return nil
}

// MarshalJSON 归一化后始终以字符串形式输出(空值由 omitempty 省略)。
func (s AnthropicSystem) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(s))
}

// AnthropicRequest 是 Anthropic Messages API 请求体。
type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        AnthropicSystem    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

// ToInternal 将 Anthropic 请求体转换为规范化请求。
func (r *AnthropicRequest) ToInternal() *Request {
	out := &Request{
		Model:       r.Model,
		System:      string(r.System),
		MaxTokens:   &r.MaxTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        r.StopSequences,
		Stream:      r.Stream,
	}
	for _, m := range r.Messages {
		msg := Message{Role: Role(m.Role)}
		var text string
		var results []Message
		for _, blk := range m.Content {
			switch blk.Type {
			case "text":
				text += blk.Text
			case "tool_use":
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: blk.ID, Name: blk.Name, Arguments: blk.Input})
			case "tool_result":
				// payload 在 content 键(规范);兼容早期用 text 键输出的形态。
				rm := Message{Role: RoleTool, ToolCallID: blk.ToolUseID}
				if len(blk.Content) > 0 {
					for _, c := range blk.Content {
						if c.Type == "text" {
							rm.Content += c.Text
						}
					}
				} else {
					rm.Content = blk.Text
				}
				results = append(results, rm)
			}
		}
		if len(results) > 0 {
			// 一条 user 消息可含多个 tool_result 块(客户端批量返回并行工具结果):
			// 每块独立成一条 tool 消息,避免 ToolCallID 被逐块覆盖导致配对丢失
			// (孤儿的 tool_use 会被 DeepSeek 等严格校验上游以 400 拒绝)。
			// 同消息内的文本块(异常形态)并入最后一条结果。
			results[len(results)-1].Content += text
			out.Messages = append(out.Messages, results...)
			continue
		}
		msg.Content = text
		out.Messages = append(out.Messages, msg)
	}
	// 兜底:丢弃没有紧随 tool_result 配对的孤儿 tool_use(如工具调用被中断)。
	out.Messages = repairOrphanToolUse(out.Messages)
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, Tool{Function: &FunctionTool{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		}})
	}
	return out
}

// repairOrphanToolUse 删除 assistant 消息中缺少紧随其后 tool_result 配对的
// tool_use 块(工具调用被中断、客户端未回传结果),避免被严格校验的上游
// (如 DeepSeek 的 Anthropic 兼容端点)以 400 拒绝。清理后为空的 assistant
// 消息连同其后的孤儿 tool 结果一并删除。
func repairOrphanToolUse(msgs []Message) []Message {
	out := msgs[:0]
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			// 紧随其后的连续 tool 消息构成该批次的配对结果集合。
			has := map[string]bool{}
			for j := i + 1; j < len(msgs) && msgs[j].Role == RoleTool; j++ {
				has[msgs[j].ToolCallID] = true
			}
			kept := m.ToolCalls[:0]
			for _, tc := range m.ToolCalls {
				if has[tc.ID] {
					kept = append(kept, tc)
				}
			}
			m.ToolCalls = kept
		}
		if m.Role == RoleAssistant && len(m.ToolCalls) == 0 && m.Content == "" {
			i++
			for i < len(msgs) && msgs[i].Role == RoleTool {
				i++
			}
			continue
		}
		out = append(out, m)
		i++
	}
	return out
}

// ToAnthropic 将规范化请求转换为 Anthropic 请求体。
func (r *Request) ToAnthropic() *AnthropicRequest {
	out := &AnthropicRequest{
		Model:         r.Model,
		System:        AnthropicSystem(r.System),
		Temperature:   r.Temperature,
		TopP:          r.TopP,
		StopSequences: r.Stop,
		Stream:        r.Stream,
		MaxTokens:     1024, // max_tokens 必填,未设置时兜底
	}
	if r.MaxTokens != nil {
		out.MaxTokens = *r.MaxTokens
	}
	for _, m := range r.Messages {
		if m.Role == RoleTool {
			out.appendToolResult(m)
			continue
		}
		am := AnthropicMessage{Role: string(m.Role)}
		if m.Content != "" {
			am.Content = append(am.Content, AnthropicContentBlock{Type: "text", Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			am.Content = append(am.Content, AnthropicContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Arguments})
		}
		out.Messages = append(out.Messages, am)
	}
	for _, t := range r.Tools {
		if t.Function != nil {
			out.Tools = append(out.Tools, AnthropicTool{
				Name: t.Function.Name, Description: t.Function.Description, InputSchema: t.Function.Parameters,
			})
		}
	}
	return out
}

// appendToolResult 将工具结果追加为 user + tool_result 块;连续的 tool 消息合并进同一条
// user 消息,以满足 Anthropic 要求 user/assistant 角色交替的约束。
func (r *AnthropicRequest) appendToolResult(m Message) {
	blk := toolResultBlock(m)
	if n := len(r.Messages); n > 0 {
		last := r.Messages[n-1]
		if last.Role == "user" && len(last.Content) > 0 && last.Content[0].Type == "tool_result" {
			r.Messages[n-1].Content = append(r.Messages[n-1].Content, blk)
			return
		}
	}
	r.Messages = append(r.Messages, AnthropicMessage{Role: "user", Content: AnthropicContent{blk}})
}

// toolResultBlock 构造 tool_result 块:payload 按规范放在 content 键(单 text 块,
// 序列化时收敛为字符串),空结果省略 content。
func toolResultBlock(m Message) AnthropicContentBlock {
	blk := AnthropicContentBlock{Type: "tool_result", ToolUseID: m.ToolCallID}
	if m.Content != "" {
		blk.Content = AnthropicContent{{Type: "text", Text: m.Content}}
	}
	return blk
}

// AnthropicUsage 是 Anthropic 响应的 token 用量。
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicResponse 是 Anthropic Messages API 响应体。
type AnthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	Content    []AnthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      AnthropicUsage          `json:"usage"`
}

// ToInternal 将 Anthropic 响应体转换为规范化响应。
func (r *AnthropicResponse) ToInternal() *Response {
	out := &Response{
		Model: r.Model,
		Usage: Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens},
	}
	for _, blk := range r.Content {
		switch blk.Type {
		case "text":
			out.Content += blk.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: blk.ID, Name: blk.Name, Arguments: blk.Input})
		}
	}
	switch r.StopReason {
	case "end_turn", "stop_sequence":
		out.FinishReason = "stop"
	case "max_tokens":
		out.FinishReason = "length"
	case "tool_use":
		out.FinishReason = "tool_use"
	}
	return out
}

// ToAnthropic 将规范化响应转换为 Anthropic 响应体。
// 注意:ID 等仅存在于 Anthropic 的元信息无法从规范化响应还原,置零值。
func (r *Response) ToAnthropic() *AnthropicResponse {
	out := &AnthropicResponse{
		Type:  "message",
		Role:  "assistant",
		Model: r.Model,
		Usage: AnthropicUsage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens},
	}
	if r.Content != "" {
		out.Content = append(out.Content, AnthropicContentBlock{Type: "text", Text: r.Content})
	}
	for _, tc := range r.ToolCalls {
		out.Content = append(out.Content, AnthropicContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Arguments})
	}
	switch r.FinishReason {
	case "stop":
		out.StopReason = "end_turn"
	case "length":
		out.StopReason = "max_tokens"
	case "tool_use":
		out.StopReason = "tool_use"
	}
	return out
}

// AnthropicProvider 通过 Anthropic Messages API 发送请求。
type AnthropicProvider struct {
	baseURL string
	apiKey  string
	httpc   *http.Client
}

// NewAnthropicProvider 基于共享配置构造 Anthropic 适配器。
func NewAnthropicProvider(c *Client) *AnthropicProvider {
	baseURL, apiKey, httpc := resolveClient(c, "https://api.anthropic.com")
	return &AnthropicProvider{baseURL: baseURL, apiKey: apiKey, httpc: httpc}
}

// Complete 实现 Provider 接口。
func (p *AnthropicProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, errors.New("gateway: nil request")
	}
	wire := req.ToAnthropic()
	var resp AnthropicResponse
	headers := map[string]string{
		"x-api-key":         p.apiKey,
		"anthropic-version": "2023-06-01",
	}
	if err := doJSON(ctx, p.httpc, http.MethodPost, p.baseURL+"/v1/messages", headers, wire, &resp); err != nil {
		return nil, err
	}
	return resp.ToInternal(), nil
}

// Stream 发送流式请求(stream:true)并返回上游 SSE 响应体,调用方负责 Close。
func (p *AnthropicProvider) Stream(ctx context.Context, req *Request) (io.ReadCloser, error) {
	if req == nil {
		return nil, errors.New("gateway: nil request")
	}
	wire := req.ToAnthropic()
	headers := map[string]string{
		"x-api-key":         p.apiKey,
		"anthropic-version": "2023-06-01",
	}
	resp, err := doStream(ctx, p.httpc, http.MethodPost, p.baseURL+"/v1/messages", headers, wire)
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

// Do 实现 Requester 接口:Anthropic 请求体可直接发起请求。
func (r *AnthropicRequest) Do(ctx context.Context, c *Client) (*Response, error) {
	return NewAnthropicProvider(c).Complete(ctx, r.ToInternal())
}
