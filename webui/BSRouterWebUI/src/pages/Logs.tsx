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
            {e.forward_request ? (
              <>
                <div>转发请求:</div>
                <pre>{e.forward_request}</pre>
              </>
            ) : null}
            {e.forward_response ? (
              <>
                <div>转发响应:</div>
                <pre>{e.forward_response}</pre>
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
          <div className="page-sub">网关请求访问日志(JSONL),点击行可展开转发详情</div>
        </div>
        <div className="toolbar">
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
