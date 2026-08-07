package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// systemOrMessages 是完整 Anthropic 请求体:system 取两种形式,其余字段固定。
func systemOrMessages(system string) string {
	return `{"model":"cli@claude-sonnet-4-5","max_tokens":100,"system":` + system +
		`,"messages":[{"role":"user","content":"hi"}]}`
}

// Claude Code 等客户端按 Anthropic 规范既可能把 system 发成 string,也可能发成
// text 内容块数组(常带 cache_control),网关都应收敛为纯文本字符串。
func TestAnthropicSystemStringForm(t *testing.T) {
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(systemOrMessages(`"You are helpful."`)), &req); err != nil {
		t.Fatalf("unmarshal string system: %v", err)
	}
	if string(req.System) != "You are helpful." {
		t.Errorf("system = %q, want string form preserved", string(req.System))
	}
	if got := req.ToInternal().System; got != "You are helpful." {
		t.Errorf("canonical system = %q, want %q", got, "You are helpful.")
	}
}

func TestAnthropicSystemArrayForm(t *testing.T) {
	var req AnthropicRequest
	body := systemOrMessages(`[{"type":"text","text":"part one"},{"type":"text","text":"part two"}]`)
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal array system: %v", err)
	}
	if got := string(req.System); got != "part onepart two" {
		t.Errorf("system = %q, want concatenated blocks", got)
	}
	if got := req.ToInternal().System; got != "part onepart two" {
		t.Errorf("canonical system = %q, want %q", got, "part onepart two")
	}
}

// cache_control 等 Anthropic 附加字段不在本网关结构体内,解码时被忽略且不影响拼接。
func TestAnthropicSystemArrayWithCacheControl(t *testing.T) {
	var req AnthropicRequest
	body := systemOrMessages(`[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b"}]`)
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal array system with cache_control: %v", err)
	}
	if got := string(req.System); got != "ab" {
		t.Errorf("system = %q, want %q", got, "ab")
	}
}

// 非 string 也非内容块数组的 system(如数字)应明确报错,而非静默吞掉。
func TestAnthropicSystemRejectsInvalid(t *testing.T) {
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(`{"model":"m","system":42}`), &req); err == nil {
		t.Fatal("expected error for numeric system")
	}
}

// Anthropic 规范:tool_result 块的 payload 在 content 键(而非 text),且可为 string 或
// 块数组。此前结构体只建模 text,真实 wire 上的工具结果会被静默丢弃。
func TestAnthropicToolResultContentWire(t *testing.T) {
	body := `{"model":"m","max_tokens":64,"messages":[` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"72F, sunny"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_02","content":[{"type":"text","text":"42"}]}]}]}`
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := req.ToInternal()
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	if m := got.Messages[0]; m.Role != RoleTool || m.ToolCallID != "toolu_01" || m.Content != "72F, sunny" {
		t.Errorf("tool_result string form = %+v", m)
	}
	if m := got.Messages[1]; m.Role != RoleTool || m.ToolCallID != "toolu_02" || m.Content != "42" {
		t.Errorf("tool_result array form = %+v", m)
	}
}

// 输出侧:system 空时省略,非空时始终以 string 输出(即使输入端是数组)。
func TestAnthropicSystemMarshal(t *testing.T) {
	empty := &AnthropicRequest{Model: "m", Messages: []AnthropicMessage{{Role: "user", Content: AnthropicContent{{Type: "text", Text: "hi"}}}}}
	if s := mustJSON(t, empty); strings.Contains(s, `"system"`) {
		t.Errorf("empty system should be omitted, got %s", s)
	}

	filled := &AnthropicRequest{Model: "m", System: AnthropicSystem("hi"), Messages: []AnthropicMessage{{Role: "user", Content: AnthropicContent{{Type: "text", Text: "hi"}}}}}
	if s := mustJSON(t, filled); !strings.Contains(s, `"system":"hi"`) {
		t.Errorf("system should marshal as string, got %s", s)
	}
}
