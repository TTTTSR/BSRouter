package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// RewriteSSEModel 透传上游 SSE 流到 w,仅改写各格式事件中的 model 字段为 fullModel:
//   - completion: data JSON 顶层 model
//   - responses:  data JSON 内 response.model(response.created/completed 等)
//   - anthropic:  data JSON 内 message.model(message_start 事件)
//
// 供直通路径使用:模型支持请求格式时,上游 SSE 不经规范化事件转换,逐事件行级透传,
// 其余行(event: / 注释 / 非 JSON data / 空行 / [DONE])字节保真。逐事件(空行分隔,
// 兼容 CRLF)处理;事件结束时若改写命中,重排为单条 data: <json> 行输出。
func RewriteSSEModel(w io.Writer, r io.Reader, format, fullModel string) error {
	br := bufio.NewReader(r)
	var event [][]byte // 当前事件的原始行(不含行尾换行)

	flush := func() error {
		if len(event) == 0 {
			return nil
		}
		// 找出 data: 行,按 SSE 规范以单个 LF 连接。
		var dataParts [][]byte
		hasData := false
		for _, line := range event {
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				hasData = true
				dataParts = append(dataParts, bytes.TrimSpace(trimmed[len("data:"):]))
			}
		}
		if hasData {
			if rewritten := rewriteSSEJSON(bytes.Join(dataParts, []byte("\n")), format, fullModel); rewritten != nil {
				// 改写命中:保留 event:/id:/注释 等非 data 行,替换为单条 data 行。
				for _, line := range event {
					if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
						continue
					}
					if _, err := w.Write(line); err != nil {
						return err
					}
					if _, err := w.Write([]byte("\n")); err != nil {
						return err
					}
				}
				if _, err := w.Write([]byte("data: ")); err != nil {
					return err
				}
				if _, err := w.Write(rewritten); err != nil {
					return err
				}
				if _, err := w.Write([]byte("\n")); err != nil {
					return err
				}
			} else {
				// 未命中(非 JSON / 无 model 键 / 值未变化):原样透传。
				for _, line := range event {
					if _, err := w.Write(line); err != nil {
						return err
					}
					if _, err := w.Write([]byte("\n")); err != nil {
						return err
					}
				}
			}
		} else {
			// 纯 event:/注释/心跳事件,无 data 行:原样透传。
			for _, line := range event {
				if _, err := w.Write(line); err != nil {
					return err
				}
				if _, err := w.Write([]byte("\n")); err != nil {
					return err
				}
			}
		}
		// 空行分隔事件。
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
		event = event[:0]
		return nil
	}

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trim := line
			if n := len(trim); n > 0 && trim[n-1] == '\n' {
				trim = trim[:n-1]
			}
			if n := len(trim); n > 0 && trim[n-1] == '\r' {
				trim = trim[:n-1]
			}
			if len(bytes.TrimSpace(trim)) == 0 {
				// 空行 = 事件结束。
				if err := flush(); err != nil {
					return err
				}
			} else {
				event = append(event, trim)
			}
		}
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
	}
}

// rewriteSSEJSON 解析一条 SSE data JSON(对象),按格式改写其中的 model 字段。
// 返回改写后的 JSON;无需改写(非 JSON / 非对象 / 无对应 model 键 / 值未变化)时返回 nil。
// 用 UseNumber 解析,防止大整数(如时间戳)被改成 float64 丢失精度。
func rewriteSSEJSON(data []byte, format, model string) []byte {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil
	}
	// 定位各格式的 model 承载对象。
	var target any
	switch format {
	case FormatAnthropic:
		target, _ = obj["message"].(map[string]any)
	case FormatResponses:
		target, _ = obj["response"].(map[string]any)
	case FormatCompletion:
		target = obj
	default:
		return nil
	}
	m, ok := target.(map[string]any)
	if !ok {
		return nil
	}
	cur, ok := m["model"].(string)
	if !ok || cur == model {
		return nil
	}
	m["model"] = model
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}
