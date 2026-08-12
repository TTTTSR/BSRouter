import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Button, Empty, ErrorAlert, Select } from '../components/ui'
import { IconChevron, IconRefresh } from '../lib/icons'
import type { LogEntry } from '../lib/types'

function fmtTime(ts: string): string {
  // RFC3339Nano -> 本地 HH:MM:SS.mmm
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ts
  const p = (n: number, w = 2) => String(n).padStart(w, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`
}

function statusClass(status: number): string {
  if (status >= 500) return 'error-text'
  if (status >= 400) return 'muted'
  return ''
}

function Row({ e }: { e: LogEntry }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <tr className="log-row" onClick={() => setOpen((o) => !o)}>
        <td className="mono">{fmtTime(e.timestamp)}</td>
        <td>{e.method}</td>
        <td className="mono">{e.path}</td>
        <td className={statusClass(e.status)}>{e.status}</td>
        <td className="mono">{e.duration_ms}ms</td>
        <td className="mono">{e.model ?? '-'}</td>
        <td>{e.provider ?? '-'}</td>
        <td className="mono">{e.upstream_status ?? '-'}</td>
        <td className="actions"><IconChevron size={14} /></td>
      </tr>
      {open ? (
        <tr>
          <td colSpan={9} className="log-detail">
            {e.error ? <div className="error-text">错误: {e.error}</div> : null}
            {e.forward_url ? <div>转发地址: {e.forward_url}</div> : null}
            {e.request_body ? (
              <>
                <div>原始请求体(转换前):</div>
                <pre>{e.request_body}</pre>
              </>
            ) : null}
            {e.forward_request ? (
              <>
                <div>转换后请求体(发给上游):</div>
                <pre>{e.forward_request}</pre>
              </>
            ) : null}
            {e.forward_response ? (
              <>
                <div>上游响应体:</div>
                <pre>{e.forward_response}</pre>
              </>
            ) : null}
            {e.converted_response_body ? (
              <>
                <div>转换后响应体(回客户端):</div>
                <pre>{e.converted_response_body}</pre>
              </>
            ) : null}
            {e.request_id ? <div className="faint">request_id: {e.request_id} · remote: {e.remote_addr ?? '-'}</div> : null}
          </td>
        </tr>
      ) : null}
    </>
  )
}

export default function Logs() {
  const [limit, setLimit] = useState(200)
  const { data, error, loading, reload } = useAsync(() => api.listLogs(limit))
  const entries = data ?? []
  const [auto, setAuto] = useState(false)
  const [detail, setDetail] = useState<'default' | 'full' | ''>('')
  // 当前日志文件名(每次运行默认以启动时间戳命名)。
  const logFile = useAsync(() => api.logFile())
  const fileName = logFile.data?.path ? logFile.data.path.split(/[\\/]/).pop() ?? '' : ''

  // 初始读取完整度;切换时 PUT 并本地更新。
  useEffect(() => {
    api.logDetail().then((d) => setDetail(d.detail)).catch(() => {})
  }, [])

  async function changeDetail(d: 'default' | 'full') {
    try {
      await api.setLogDetail(d)
      setDetail(d)
    } catch {
      setDetail(detail) // 失败回滚
    }
  }

  // 自动刷新:每 5 秒拉取一次。
  useEffect(() => {
    if (!auto) return
    const id = setInterval(() => reload(), 5000)
    return () => clearInterval(id)
  }, [auto, reload])

  function toggleAuto() {
    const next = !auto
    setAuto(next)
    if (next) reload()
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <div className="page-title">日志查看</div>
          <div className="page-sub">
            {fileName
              ? `网关 API 请求访问日志(JSONL,不含管理接口) · 当前文件: ${fileName}`
              : '网关 API 请求访问日志(JSONL,不含管理接口)'}
          </div>
        </div>
        <div className="toolbar">
          <Select
            value={detail || 'default'}
            onChange={(e) => void changeDetail(e.target.value as 'default' | 'full')}
            title="日志完整度:完整模式记录全部请求的请求/响应体;默认模式仅出错时记录"
          >
            <option value="default">完整度:默认(出错才记详情)</option>
            <option value="full">完整度:完整(全部记录详情)</option>
          </Select>
          <Select
            value={String(limit)}
            onChange={(e) => {
              setLimit(Number(e.target.value))
              reload()
            }}
          >
            <option value="50">最近 50 条</option>
            <option value="100">最近 100 条</option>
            <option value="200">最近 200 条</option>
            <option value="500">最近 500 条</option>
          </Select>
          <Button variant="ghost" onClick={() => void reload()}>
            <IconRefresh size={15} />
            刷新
          </Button>
          <Button variant={auto ? 'primary' : 'secondary'} onClick={toggleAuto}>
            {auto ? '自动刷新中' : '自动刷新'}
          </Button>
        </div>
      </div>

      {error ? <ErrorAlert text={error} /> : null}

      {loading ? (
        <Empty text="加载中…" />
      ) : entries.length === 0 ? (
        <Empty text="暂无日志记录" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>时间</th>
                <th>方法</th>
                <th>路径</th>
                <th>状态</th>
                <th>耗时</th>
                <th>模型</th>
                <th>供应商</th>
                <th>上游</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e, i) => <Row key={e.request_id ?? `${e.timestamp}-${i}`} e={e} />)}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
