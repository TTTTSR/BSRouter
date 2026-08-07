// 流式支持:上游 SSE 解析、流式请求发起,以及**规范化流式事件**(通用中间类型)与
// 各接口格式之间的编解码。流式转换与非流式同构:上游 SSE → 解码为规范化事件 →
// 规范化事件(模型回填等格式无关变换)→ 编码为目标格式 SSE。新增接口格式只需实现
// 一对 解码器(格式 → 规范化) 与 编码器(规范化 → 格式),即可双向互通。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 客户端/上游接口格式标识(与 provider.Kind 字符串一致)。
const (
	FormatAnthropic  = "anthropic"
	FormatCompletion = "completion"
	FormatResponses  = "responses"
)

// maxStreamCapture 限制流式转发详情累计的响应体字节数(与日志截断 256 KB 对齐);
// 流式响应无法整包缓存,只保留前段用于日志排障。
const maxStreamCapture = 256 << 10

// SSEBlock 是解析后的一条原始上游 SSE 事件。
// Event 为事件名(Anthropic 的 message_start 等;OpenAI 格式通常为空),
// Data 为 data 行的载荷(如 OpenAI 结尾的 [DONE] 等非 JSON 内容原样保留)。
type SSEBlock struct {
	Event string
	Data  json.RawMessage
}

// ReadSSE 从 reader 逐条读取 SSE 事件并回调 fn。
// 兼容 LF / CRLF 行尾;连续多行 data: 按规范以 \n 连接成单个事件载荷;
// 空行分隔事件;忽略注释行与空事件;末尾缺空行的最后一条事件也会被回调。
func ReadSSE(r io.Reader, fn func(SSEBlock) error) error {
	br := bufio.NewReader(r)
	var (
		event string
		data  bytes.Buffer
	)
	emit := func() error {
		if event == "" && data.Len() == 0 {
			return nil // 连续空行,无有效事件
		}
		payload := json.RawMessage(append([]byte(nil), data.Bytes()...))
		blk := SSEBlock{Event: event, Data: payload}
		event = ""
		data.Reset()
		return fn(blk)
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				v := strings.TrimPrefix(line, "data:")
				v = strings.TrimPrefix(v, " ")
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(v)
			}
		}
		if err != nil {
			if err == io.EOF {
				return emit()
			}
			return err
		}
		if line == "" {
			if err := emit(); err != nil {
				return err
			}
		}
	}
}

// ---- 规范化流式事件(通用中间类型) ----

// StreamEventType 是规范化流式事件类型。
type StreamEventType string

const (
	StreamMessageStart StreamEventType = "message_start"
	StreamContentStart StreamEventType = "content_start"
	StreamContentDelta StreamEventType = "content_delta"
	StreamContentStop  StreamEventType = "content_stop"
	StreamMessageDelta StreamEventType = "message_delta"
	StreamMessageStop  StreamEventType = "message_stop"
	StreamError        StreamEventType = "error"
)

// StreamEvent 是规范化流式事件,三种接口格式共享,由各格式解码器产出、编码器消费。
// 各类型使用的字段:
//
//	message_start:  ID / Model / Usage
//	content_start:  Index / BlockType("text"|"tool_use"|"thinking") / BlockID / BlockName
//	content_delta:  Index / DeltaType("text"|"thinking"|"input_json") / DeltaText / PartialJSON
//	content_stop:   Index
//	message_delta:  StopReason / Usage
//	message_stop:   (无)
//	error:          Error
//
// 约定:解码器产出的规范化流是"良构"的——恰一条 message_start 开头、内容块
// start/delta/stop 配对、恰一条 message_delta(带最终 usage)、一条 message_stop 收尾,
// 出错时以 error 事件终止。编码器据此可安全直接编码。
type StreamEvent struct {
	Type        StreamEventType `json:"type"`
	ID          string          `json:"id,omitempty"`
	Model       string          `json:"model,omitempty"`
	Usage       *Usage          `json:"usage,omitempty"`
	Index       int             `json:"index,omitempty"`
	BlockType   string          `json:"block_type,omitempty"`
	BlockID     string          `json:"block_id,omitempty"`
	BlockName   string          `json:"block_name,omitempty"`
	DeltaType   string          `json:"delta_type,omitempty"`
	DeltaText   string          `json:"delta_text,omitempty"`
	PartialJSON string          `json:"partial_json,omitempty"`
	StopReason  string          `json:"stop_reason,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// StreamDecoder 把某接口格式的上游 SSE 流解码为规范化流式事件(逐个回调 emit)。
type StreamDecoder func(r io.Reader, emit func(StreamEvent) error) error

// StreamEncoder 把一条规范化流式事件编码为该接口格式的一条 SSE 写入 w。
type StreamEncoder func(w io.Writer, ev StreamEvent) error

// DecoderFor 返回指定接口格式的流式解码器(格式 → 规范化)。
func DecoderFor(format string) (StreamDecoder, bool) {
	switch format {
	case FormatAnthropic:
		return DecodeAnthropicSSE, true
	case FormatCompletion:
		return DecodeCompletionSSE, true
	case FormatResponses:
		return DecodeResponsesSSE, true
	}
	return nil, false
}

// EncoderFor 返回指定接口格式的流式编码器(规范化 → 格式)。
// 每次调用返回**新实例**(携带每请求状态:id/model、工具块索引映射等),可安全并发复用。
func EncoderFor(format string) (StreamEncoder, bool) {
	switch format {
	case FormatAnthropic:
		return EncodeAnthropicSSE, true // 无跨事件状态,单函数即可
	case FormatCompletion:
		enc := &completionEncoder{}
		return enc.Encode, true
	case FormatResponses:
		enc := &responsesEncoder{items: map[int]*respItemState{}}
		return enc.Encode, true
	}
	return nil, false
}

// streamClient 返回不带总超时的 http.Client 副本(共享 Transport,连接池不丢)。
// http.Client.Timeout 覆盖整个请求含读体,流式会话时长不受限,须移除该上限,
// 改由调用方的 context 取消来终止。
func streamClient(c *http.Client) *http.Client {
	if c == nil || c.Timeout == 0 {
		return c
	}
	c2 := *c
	c2.Timeout = 0
	return &c2
}

// doStream 发送 JSON 请求并返回未读取的响应体(流式)。
// 与 doJSON 不同:不读体、不解码;响应体被包装,读取时透传并把前 maxStreamCapture
// 字节累计进转发详情收集器(关闭时回调一次)。调用方负责 Close resp.Body。
func doStream(ctx context.Context, httpc *http.Client, method, url string, headers map[string]string, body any) (*http.Response, error) {
	var reqBytes []byte
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBytes = b
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := streamClient(httpc).Do(req)
	if err != nil {
		capture(ctx, url, reqBytes, nil, 0)
		return nil, fmt.Errorf("request upstream: %w", err)
	}
	landed := resp.Request.URL.String()
	resp.Body = newCaptureBody(ctx, resp.Body, landed, reqBytes, resp.StatusCode)
	return resp, nil
}

// captureBodyRC 包装上游响应体:读取透传,同时累计前 maxStreamCapture 字节,
// Close 时把累计内容与转发元信息回传给收集器。
type captureBodyRC struct {
	io.ReadCloser
	ctx     context.Context
	url     string
	reqBody []byte
	status  int
	buf     []byte
}

func newCaptureBody(ctx context.Context, rc io.ReadCloser, url string, reqBody []byte, status int) io.ReadCloser {
	return &captureBodyRC{ReadCloser: rc, ctx: ctx, url: url, reqBody: reqBody, status: status}
}

func (c *captureBodyRC) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		if remain := maxStreamCapture - len(c.buf); remain > 0 {
			if n < remain {
				remain = n
			}
			c.buf = append(c.buf, p[:remain]...)
		}
	}
	return n, err
}

func (c *captureBodyRC) Close() error {
	capture(c.ctx, c.url, c.reqBody, c.buf, c.status)
	return c.ReadCloser.Close()
}
