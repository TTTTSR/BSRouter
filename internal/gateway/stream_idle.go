// 流式 idle 超时:上游响应体两字节数据到达间隔超过阈值即中止。
// 经 context 注入(与 WithCapture 同模式),在 doStream 单点包装 resp.Body,
// 直通与转换两条流式路径同时生效。
package gateway

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

type streamIdleTimeoutKey struct{}

// WithStreamIdleTimeout 在上下文中注入流式 idle 超时;d<=0 视为禁用(返回原 ctx)。
func WithStreamIdleTimeout(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, streamIdleTimeoutKey{}, d)
}

// streamIdleTimeout 从上下文读取注入的 idle 超时;未注入/禁用返回 0。
func streamIdleTimeout(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(streamIdleTimeoutKey{}).(time.Duration); ok {
		return d
	}
	return 0
}

// idleTimeoutReadCloser 包装 io.ReadCloser:任何一次 Read 阻塞超过 timeout(期间零字节
// 返回)就关闭底层 reader,使阻塞中的 Read 返回,并让后续 Read 返回明确的超时错误。
// 每次流只启动一个 watchdog goroutine,Close 时经 stop 退出,不泄漏。
type idleTimeoutReadCloser struct {
	rc      io.ReadCloser
	timeout time.Duration

	mu       sync.Mutex
	last     time.Time // 最近一次"有活动"时刻(构造时置 now,覆盖首字节迟迟不到)
	timedOut bool
	stop     chan struct{}
	stopOnce sync.Once
}

func newIdleTimeoutReadCloser(rc io.ReadCloser, d time.Duration) io.ReadCloser {
	r := &idleTimeoutReadCloser{rc: rc, timeout: d, last: time.Now(), stop: make(chan struct{})}
	go r.watchdog()
	return r
}

func (r *idleTimeoutReadCloser) watchdog() {
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	for {
		r.mu.Lock()
		idle := time.Since(r.last)
		r.mu.Unlock()
		if idle >= r.timeout {
			r.mu.Lock()
			r.timedOut = true
			r.mu.Unlock()
			// 关闭底层 reader 以解除阻塞中的 Read(net/http 文档化的取消模式,
			// 对 HTTP/1.1 与 HTTP/2 均有效)。
			_ = r.rc.Close()
			return
		}
		timer.Reset(r.timeout - idle)
		select {
		case <-timer.C:
		case <-r.stop:
			return
		}
	}
}

func (r *idleTimeoutReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()
	// 读前 arm:把"解码/写客户端耗时"排除在空闲判定外,只有上游 N 秒零数据才中止。
	r.last = time.Now()
	r.mu.Unlock()

	n, err := r.rc.Read(p)

	r.mu.Lock()
	if r.timedOut {
		r.mu.Unlock()
		return 0, fmt.Errorf("upstream stream idle timeout after %s (no data received)", r.timeout)
	}
	if n > 0 {
		r.last = time.Now()
	}
	r.mu.Unlock()
	return n, err
}

func (r *idleTimeoutReadCloser) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	return r.rc.Close()
}
