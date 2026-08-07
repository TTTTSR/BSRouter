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
	ForwardURL      string `json:"forward_url,omitempty"`
	ForwardRequest  string `json:"forward_request,omitempty"`
	ForwardResponse string `json:"forward_response,omitempty"`
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

// maxLogRead 限制读取日志文件末尾的字节数,避免日志过大时整读。
const maxLogRead = 5 << 20 // 5 MB

// Recent 返回最近 n 条日志(最新的在前)。只读取文件末尾至多 maxLogRead 字节,
// 读取/解析失败的行会跳过。供 /manage/v1/logs 等管理接口使用。
func (l *Logger) Recent(n int) ([]Entry, error) {
	if n <= 0 {
		n = 100
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	data, start, err := readTail(l.path, maxLogRead)
	if err != nil {
		return nil, err
	}
	if start > 0 {
		// 读取起点落在文件中间时,首行可能被截断,丢弃之。
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := bytes.Split(data, []byte("\n"))
	entries := make([]Entry, 0, n)
	for i := len(lines) - 1; i >= 0 && len(entries) < n; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// readTail 读取文件末尾至多 maxBytes 字节,返回数据与起始偏移。
func readTail(path string, maxBytes int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, 0, err
	}
	return buf, start, nil
}
