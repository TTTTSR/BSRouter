package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)// Client 是上游连接配置,三种接口格式共用。
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// Provider 是各格式适配器统一实现的接口,消费/产出规范化类型。
type Provider interface {
	// Complete 发送规范化请求并返回规范化响应。
	Complete(ctx context.Context, req *Request) (*Response, error)
	// Stream 发送规范化请求(stream:true)并返回上游 SSE 响应体,调用方负责 Close。
	Stream(ctx context.Context, req *Request) (io.ReadCloser, error)
}

// Requester 是请求体统一接口:任意格式的请求体都能直接发起请求。
type Requester interface {
	// Do 使用连接配置向大模型发起请求,返回规范化响应。
	Do(ctx context.Context, c *Client) (*Response, error)
}

// apiError 表示上游返回的非 2xx 错误。
type apiError struct {
	StatusCode int
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.StatusCode, e.Body)
}

// HTTPStatus 返回上游 HTTP 状态码(供日志等消费)。
func (e *apiError) HTTPStatus() int { return e.StatusCode }

// resolveClient 从共享配置解析出上游地址、密钥与 HTTP 客户端,未设置时使用默认值。
func resolveClient(c *Client, defaultBaseURL string) (baseURL, apiKey string, httpc *http.Client) {
	baseURL = defaultBaseURL
	if c != nil {
		if c.BaseURL != "" {
			baseURL = c.BaseURL
		}
		apiKey = c.APIKey
		httpc = c.HTTP
	}
	if httpc == nil {
		httpc = &http.Client{Timeout: 120 * time.Second}
	}
	return baseURL, apiKey, httpc
}

// maxUpstreamBody 限制读取上游响应体的上限,防止异常上游耗尽内存。
const maxUpstreamBody = 100 << 20 // 100 MB

// CaptureFunc 在一次上游请求完成后被回调,携带转发详情(URL / 请求体 / 响应体 / 状态码),
// 供请求日志等收集"请求转发到了哪里、发了什么、回了什么"。
type CaptureFunc func(url string, reqBody, respBody []byte, statusCode int)

type captureKey struct{}

// WithCapture 在上下文中注入转发详情收集器。
func WithCapture(ctx context.Context, f CaptureFunc) context.Context {
	return context.WithValue(ctx, captureKey{}, f)
}

// capture 若上下文中注入了收集器,则回调一次(无收集器时为空操作)。
func capture(ctx context.Context, url string, reqBody, respBody []byte, statusCode int) {
	if f, ok := ctx.Value(captureKey{}).(CaptureFunc); ok {
		f(url, reqBody, respBody, statusCode)
	}
}

// doJSON 向 url 发送 JSON 请求,并把响应解码到 out。headers 为额外请求头。
// 无论成功、传输失败还是读体失败,都会(若配置了收集器)记录一次转发详情。
func doJSON(ctx context.Context, httpc *http.Client, method, url string, headers map[string]string, body, out any) error {
	var reqBytes []byte
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBytes = b
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		// 传输层失败(连接/DNS/TLS)也记录转发目标,便于排障。
		capture(ctx, url, reqBytes, nil, 0)
		return fmt.Errorf("request upstream: %w", err)
	}
	defer resp.Body.Close()
	// 跟随重定向后,resp.Request.URL 才是请求真正落到的地址。
	landed := resp.Request.URL.String()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if err != nil {
		capture(ctx, landed, reqBytes, nil, resp.StatusCode)
		return fmt.Errorf("read response body: %w", err)
	}
	capture(ctx, landed, reqBytes, data, resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unmarshal upstream response (%d): %w", resp.StatusCode, err)
		}
	}
	return nil
}
