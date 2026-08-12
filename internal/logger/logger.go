// Package logger 提供追加写入的 JSONL(JSON Lines)请求日志。
// 每条日志一行 JSON,便于后续用标准工具按行解析、聚合。
package logger

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"
)

// Entry 是一条请求访问日志。除访问元信息外,还记录该请求被转发到的上游地址
// 与实际转发的请求/响应内容(forward_* 字段)。注意:
//   - 请求/响应体会包含用户 prompt 与生成内容,属敏感数据,请自行评估留存策略;
//   - 网关不会记录鉴权头,且 forward_* 内容中的 api_key 已在写入前抹除;
//   - remote_addr/user_agent 为常见访问日志字段,默认仅回环部署时风险有限。
type Entry struct {
	Timestamp       string `json:"timestamp"`
	RequestID       string `json:"request_id,omitempty"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	Status          int    `json:"status"`
	DurationMS      int64  `json:"duration_ms"`
	RemoteAddr      string `json:"remote_addr,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
	RequestBytes    int64  `json:"request_bytes,omitempty"`
	ResponseBytes   int64  `json:"response_bytes,omitempty"`
	Model           string `json:"model,omitempty"`
	Provider        string `json:"provider,omitempty"`
	Kind            string `json:"kind,omitempty"`
	UpstreamStatus  int    `json:"upstream_status,omitempty"`
	Error           string `json:"error,omitempty"`
	// 转发详情(full 完整度或出错时记录):
	//   RequestBody          客户端原始 wire 请求体(仅转换路径记录,直通不记)
	//   ForwardURL           转发的上游地址
	//   ForwardRequest       发给上游的 wire 请求体(= 转换后的请求体;直通时为改 model 后的原始)
	//   ForwardResponse      上游返回的 wire 响应体(= 转换前/上游格式)
	//   ConvertedResponseBody 转换后回客户端的 wire 响应体(仅转换路径记录,直通不记)
	RequestBody          string `json:"request_body,omitempty"`
	ForwardURL           string `json:"forward_url,omitempty"`
	ForwardRequest       string `json:"forward_request,omitempty"`
	ForwardResponse      string `json:"forward_response,omitempty"`
	ConvertedResponseBody string `json:"converted_response_body,omitempty"`
}

// Logger 以 JSONL 格式追加写入日志,线程安全。
type Logger struct {
	mu   sync.Mutex
	enc  *json.Encoder
	f    *os.File
	path string
}

// New 打开(不存在则创建)日志文件并返回 Logger。文件权限 0600,仅属主可读写。
func New(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f, enc: json.NewEncoder(f), path: path}, nil
}

// Log 追加写入一条日志(JSON 一行)。
func (l *Logger) Log(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(e)
}

// Close 关闭日志文件。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Path 返回当前日志文件路径(供管理界面展示每次运行的日志文件名)。
func (l *Logger) Path() string {
	return l.path
}

// maxLogRead 限制无过滤(keep==nil)时读取日志文件末尾的字节数。
const maxLogRead = 5 << 20 // 5 MB

// maxFilterLogRead 限制带过滤(keep!=nil)时扩大读取窗口的上限,避免扫描超大文件。
const maxFilterLogRead = 32 << 20 // 32 MB

// Recent 返回最近 n 条(最新的在前)满足 keep 条件的日志(keep 为 nil 时全部)。
// 只读取文件末尾:keep 非 nil 时若末尾窗口内匹配行不足 n 条,按指数扩大读取窗口
// 直至凑够或达到 maxFilterLogRead 上限。读取/解析失败的行会跳过。
// 供 /manage/v1/logs 等管理接口使用。
func (l *Logger) Recent(n int, keep func(Entry) bool) ([]Entry, error) {
	if n <= 0 {
		n = 100
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	total := st.Size()

	entries := make([]Entry, 0, n)
	window := int64(maxLogRead)
	read := int64(0)
	for read < total {
		size := window
		if remain := total - read; remain < size {
			size = remain
		}
		start := total - read - size
		buf := make([]byte, size)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, err
		}
		read += size
		if start > 0 {
			// 读取起点落在文件中间时,块首行是上一块的延续(半行),丢弃之;
			// 跨块整行的另一半在上块末尾,因不完整会被 JSON 解析跳过,不会重复。
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			}
		}
		lines := bytes.Split(buf, []byte("\n"))
		for i := len(lines) - 1; i >= 0 && len(entries) < n; i-- {
			line := bytes.TrimSpace(lines[i])
			if len(line) == 0 {
				continue
			}
			var e Entry
			if err := json.Unmarshal(line, &e); err != nil {
				continue
			}
			if keep != nil && !keep(e) {
				continue
			}
			entries = append(entries, e)
		}
		if len(entries) >= n || keep == nil {
			break
		}
		// 带过滤且未凑够:扩大窗口继续往前读。
		window *= 2
		if window > maxFilterLogRead {
			window = maxFilterLogRead
		}
	}
	return entries, nil
}
