// 流式失败可观测性:各解码器截断返回 error、直通截断追加错误帧、idle 超时 reader。
package gateway

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// anthropic 上游流缺 message_stop → 返回 UpstreamStreamError 且事件以 StreamError 收尾。
func TestDecodeAnthropicSSETruncated(t *testing.T) {
	input := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"m\",\"content\":[]}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	// 无 message_stop
	evs, err := collectDecodedAllowErr(t, FormatAnthropic, input)
	if err == nil {
		t.Fatalf("truncated anthropic stream should return an error")
	}
	if evs[len(evs)-1].Type != StreamError {
		t.Fatalf("last event = %+v, want StreamError", evs[len(evs)-1])
	}
	if !strings.Contains(evs[len(evs)-1].Error, "missing message_stop") {
		t.Errorf("error = %q, want message_stop mention", evs[len(evs)-1].Error)
	}
}

// 上游 anthropic error 事件 + EOF:只发一次 StreamError,不再补报截断,返回 nil。
func TestDecodeAnthropicSSEErrorEventNoDouble(t *testing.T) {
	input := "event: error\n" +
		"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
	evs, err := collectDecodedAllowErr(t, FormatAnthropic, input)
	if err != nil {
		t.Fatalf("error event should terminate stream normally, got %v", err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == StreamError {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("StreamError count = %d, want 1 (no double report): %+v", n, evs)
	}
}

// responses 上游流缺 response.completed → 返回 UpstreamStreamError 且事件以 StreamError 收尾。
func TestDecodeResponsesSSETruncated(t *testing.T) {
	input := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	// 无 response.completed
	evs, err := collectDecodedAllowErr(t, FormatResponses, input)
	if err == nil {
		t.Fatalf("truncated responses stream should return an error")
	}
	if evs[len(evs)-1].Type != StreamError {
		t.Fatalf("last event = %+v, want StreamError", evs[len(evs)-1])
	}
}

// 直通路径:三格式缺终止标记 → 返回 UpstreamStreamError,且输出尾部追加对应格式错误帧。
func TestRewriteSSEModelTruncation(t *testing.T) {
	cases := []struct {
		format string
		input  string
		frame  string // 追加的错误帧特征
	}{
		{FormatCompletion, `data: {"id":"1","model":"m","choices":[]}` + "\n\n", `data: {"error":{"message":`},
		{FormatAnthropic, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n", "event: error\n"},
		{FormatResponses, `data: {"type":"response.created","response":{"id":"r1"}}` + "\n\n", `"code":"stream_error"`},
	}
	for _, c := range cases {
		var out bytes.Buffer
		err := RewriteSSEModel(&out, strings.NewReader(c.input), c.format, "full@model")
		if !IsUpstreamStreamError(err) {
			t.Fatalf("%s: err = %v, want UpstreamStreamError", c.format, err)
		}
		if !strings.Contains(out.String(), c.frame) {
			t.Errorf("%s: output missing error frame %q:\n%s", c.format, c.frame, out.String())
		}
	}
}

// 直通 completion 以 finish_reason 结尾(无 [DONE])→ 不算截断,不报错。
func TestRewriteSSEModelCompletionFinishReasonTerminal(t *testing.T) {
	input := `data: {"id":"1","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"
	var out bytes.Buffer
	if err := RewriteSSEModel(&out, strings.NewReader(input), FormatCompletion, "full@model"); err != nil {
		t.Fatalf("finish_reason should be terminal, got %v", err)
	}
	if strings.Contains(out.String(), `"error"`) {
		t.Errorf("no error frame expected, got:\n%s", out.String())
	}
}

// idleTimeoutReadCloser:data-then-stall,超时后返回明确的 idle timeout 错误,Close 后 watchdog 退出。
func TestIdleTimeoutReader(t *testing.T) {
	before := time.Now()
	ir := newIdleTimeoutReadCloser(newStallReader("hello", 500*time.Millisecond), 50*time.Millisecond)
	defer ir.Close()

	buf := make([]byte, 16)
	n, err := ir.Read(buf)
	if n != 5 || err != nil {
		t.Fatalf("first read = %d, %v, want 5, nil", n, err)
	}
	// 第二次读:stall 中 watchdog 触发 Close → 立即返回 idle timeout 错误(不必等满 stall)。
	_, err = ir.Read(buf)
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("second read err = %v, want idle timeout", err)
	}
	if time.Since(before) > 400*time.Millisecond {
		t.Errorf("idle timeout took too long (%v), watchdog should close the blocked read", time.Since(before))
	}
}

// stallReader:第一次返回 data,之后阻塞直到 Close 或 stall 超时。
type stallReader struct {
	data  []byte
	stall time.Duration
	read  bool
	done  chan struct{}
	once  sync.Once
}

func newStallReader(data string, stall time.Duration) *stallReader {
	return &stallReader{data: []byte(data), stall: stall, done: make(chan struct{})}
}

func (s *stallReader) Read(p []byte) (int, error) {
	if !s.read {
		s.read = true
		return copy(p, s.data), nil
	}
	select {
	case <-s.done:
		return 0, io.EOF
	case <-time.After(s.stall):
		return 0, io.EOF
	}
}

func (s *stallReader) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}
