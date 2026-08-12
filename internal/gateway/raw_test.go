package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doRaw 发送原始 body,返回上游收到的原始内容与状态码,并触发转发详情收集器。
func TestDoRaw(t *testing.T) {
	var gotBody []byte
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	var captured *captureCall
	ctx := WithCapture(context.Background(), func(url string, reqBody, respBody []byte, status int) {
		captured = &captureCall{url: url, reqBody: reqBody, respBody: respBody, status: status}
	})
	raw := json.RawMessage(`{"model":"m","messages":[]}`)
	status, body, err := doRaw(ctx, http.DefaultClient, http.MethodPost, up.URL, map[string]string{"Authorization": "Bearer secret"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusCreated {
		t.Errorf("status = %d, want 201", status)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s, want raw passthrough", body)
	}
	if !bytes.Equal(gotBody, raw) {
		t.Errorf("upstream got body = %s, want %s", gotBody, raw)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q, want Bearer secret", gotAuth)
	}
	if captured == nil || captured.status != 201 || !bytes.Equal(captured.reqBody, raw) || !bytes.Equal(captured.respBody, body) {
		t.Errorf("capture = %+v", captured)
	}
}

// doRaw 传输失败也记录转发目标。
func TestDoRawTransportErrorCaptures(t *testing.T) {
	// 关闭的端口 → 连接失败。
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()

	var captured *captureCall
	ctx := WithCapture(context.Background(), func(u string, reqBody, respBody []byte, status int) {
		captured = &captureCall{url: u, reqBody: reqBody, respBody: respBody, status: status}
	})
	raw := json.RawMessage(`{}`)
	if _, _, err := doRaw(ctx, http.DefaultClient, http.MethodPost, url, nil, raw); err == nil {
		t.Fatal("expected transport error")
	}
	if captured == nil || captured.status != 0 || !bytes.Equal(captured.reqBody, raw) {
		t.Errorf("capture on transport error = %+v", captured)
	}
}

func TestDoRawInvalidBody(t *testing.T) {
	if _, _, err := doRaw(context.Background(), http.DefaultClient, http.MethodPost, "http://up", nil, json.RawMessage(`{not json`)); err == nil {
		t.Fatal("expected error for invalid raw body")
	}
}

// 非 2xx 以状态码表达(不返回 error),供聚合故障转移按状态码切换成员。
func TestDoRawNon2xxNoError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer up.Close()
	status, body, err := doRaw(context.Background(), http.DefaultClient, http.MethodPost, up.URL, nil, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("non-2xx should not be an error, got %v", err)
	}
	if status != 500 || string(body) != `{"error":"boom"}` {
		t.Errorf("status/body = %d/%s", status, body)
	}
}

type captureCall struct {
	url      string
	reqBody  []byte
	respBody []byte
	status   int
}

// 三个适配器的 CompleteRaw/StreamRaw 使用各自端点与鉴权头,body 原样透传。
func TestAdapterCompleteRawPaths(t *testing.T) {
	cases := []struct {
		name string
		path string
		auth string
		run  func(c *Client) error
	}{
		{
			"anthropic", "/v1/messages", "x-api-key",
			func(c *Client) error {
				_, _, err := NewAnthropicProvider(c).CompleteRaw(context.Background(), json.RawMessage(`{}`))
				return err
			},
		},
		{
			"completion", "/v1/chat/completions", "Authorization",
			func(c *Client) error {
				_, _, err := NewCompletionProvider(c).CompleteRaw(context.Background(), json.RawMessage(`{}`))
				return err
			},
		},
		{
			"responses", "/v1/responses", "Authorization",
			func(c *Client) error {
				_, _, err := NewResponsesProvider(c).CompleteRaw(context.Background(), json.RawMessage(`{}`))
				return err
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath, gotAuth string
			var gotVersion string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotVersion = r.Header.Get("anthropic-version")
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"x"}`)
			}))
			defer up.Close()
			client := &Client{BaseURL: up.URL, APIKey: "sk-test", HTTP: up.Client()}
			if err := c.run(client); err != nil {
				t.Fatal(err)
			}
			if gotPath != c.path {
				t.Errorf("path = %q, want %q", gotPath, c.path)
			}
			if c.auth == "x-api-key" {
				if r := up.Client(); r != nil {
					_ = r
				}
			}
			if c.name == "anthropic" {
				if gotAuth != "" {
					t.Errorf("anthropic should NOT set Authorization, got %q", gotAuth)
				}
				if gotVersion != "2023-06-01" {
					t.Errorf("anthropic-version = %q, want 2023-06-01", gotVersion)
				}
			} else if gotAuth != "Bearer sk-test" {
				t.Errorf("auth = %q, want Bearer sk-test", gotAuth)
			}
		})
	}
}

func TestAdapterStreamRaw(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[]}\n\n")
	}))
	defer up.Close()
	client := &Client{BaseURL: up.URL, APIKey: "k", HTTP: up.Client()}
	resp, err := NewCompletionProvider(client).StreamRaw(context.Background(), json.RawMessage(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), `"model":"m"`) {
		t.Errorf("stream body = %s", data)
	}
}

// RewriteSSEModel:仅改写各格式事件中的 model 字段,其余行原样透传。
// 改写会重排 data JSON 的键序(Go map 无序),故用子串断言而非字节相等。
func TestRewriteSSEModel(t *testing.T) {
	cases := []struct {
		name   string
		format string
		input  string
		want   []string // 均须出现在输出中
		notIn  []string // 不得出现
	}{
		{
			"completion", FormatCompletion,
			`data: {"id":"1","object":"chat.completion.chunk","model":"deepseek","choices":[]}` + "\n\n" +
				`data: [DONE]` + "\n\n",
			[]string{`"model":"full@model"`, `data: [DONE]`},
			[]string{`"model":"deepseek"`},
		},
		{
			"responses", FormatResponses,
			`event: response.created` + "\n" +
				`data: {"type":"response.created","response":{"id":"r1","model":"deepseek","status":"in_progress"}}` + "\n\n" +
				`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n",
			[]string{`event: response.created`, `"model":"full@model"`, `"delta":"hi"`},
			[]string{`"model":"deepseek"`},
		},
		{
			"anthropic", FormatAnthropic,
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"m1","model":"deepseek","content":[]}}` + "\n\n" +
				`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n",
			[]string{`event: message_start`, `"model":"full@model"`, `"text":"hi"`},
			[]string{`"model":"deepseek"`},
		},
		{
			"non-json-data-passthrough", FormatCompletion,
			`data: plain text not json` + "\n\n" +
				`event: ping` + "\n\n",
			[]string{`data: plain text not json`, `event: ping`},
			nil,
		},
		{
			"already-correct-model", FormatCompletion,
			`data: {"model":"full@model","choices":[]}` + "\n\n",
			[]string{`data: {"model":"full@model"`},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := RewriteSSEModel(&out, strings.NewReader(c.input), c.format, "full@model"); err != nil {
				t.Fatal(err)
			}
			for _, w := range c.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output missing %q:\n%s", w, out.String())
				}
			}
			for _, n := range c.notIn {
				if strings.Contains(out.String(), n) {
					t.Errorf("output should not contain %q:\n%s", n, out.String())
				}
			}
		})
	}
}

// rewriteSSEJSON 大整数(时间戳)不应因 JSON 往返丢失精度。
func TestRewriteSSEJSONPreservesNumber(t *testing.T) {
	in := `{"model":"m","created":1760000000000,"response":{"model":"inner","ts":123456789012345}}`
	out := rewriteSSEJSON([]byte(in), FormatCompletion, "full")
	if out == nil {
		t.Fatal("expected rewrite for completion top-level model")
	}
	if !bytes.Contains(out, []byte(`"created":1760000000000`)) {
		t.Errorf("created lost precision: %s", out)
	}
	if !bytes.Contains(out, []byte(`"model":"full"`)) {
		t.Errorf("model not rewritten: %s", out)
	}
}
