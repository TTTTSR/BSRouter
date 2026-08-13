import { useState } from 'react'
import { api } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Badge, Button, Empty, ErrorAlert } from '../components/ui'
import { IconRefresh, IconTrash } from '../lib/icons'

const CATEGORY_LABEL: Record<string, string> = {
  insufficient_balance: '余额不足',
  rate_limited: '限流',
  internal: '内部错误',
  upstream: '上游错误',
}

function fmtTime(ts: string): string {
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ts
  const p = (n: number, w = 2) => String(n).padStart(w, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

export default function Faults() {
  const { data, error, loading, reload } = useAsync(() => api.listFaults())
  const faults = data?.faults ?? []
  const mode = data?.mode ?? 'user'
  const [deleting, setDeleting] = useState<string | null>(null)

  async function del(id: string) {
    setDeleting(id)
    try {
      await api.deleteFault(id)
    } catch {
      // 删除失败(如已被删除)也刷新,使界面与后端一致。
    } finally {
      setDeleting(null)
      await reload()
    }
  }

  const modeLabel =
    mode === 'dev'
      ? '开发模式：捕捉所有错误（内部错误与上游错误）'
      : '用户模式：仅捕捉可阻塞故障（余额不足 402、限流 429，可自定义）'

  return (
    <div>
      <div className="page-head">
        <div>
          <div className="page-title">故障提示</div>
          <div className="page-sub">{modeLabel}</div>
        </div>
        <div className="toolbar">
          <Button variant="ghost" onClick={() => void reload()}>
            <IconRefresh size={15} />
            刷新
          </Button>
        </div>
      </div>

      {error ? <ErrorAlert text={error} /> : null}

      {loading ? (
        <Empty text="加载中…" />
      ) : faults.length === 0 ? (
        <Empty text="暂无故障记录" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>时间</th>
                <th>类型</th>
                <th>故障内容</th>
                <th>模型</th>
                <th>供应商</th>
                <th>状态</th>
                <th>自动解除</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {faults.map((f) => (
                <tr key={f.id}>
                  <td className="mono">{fmtTime(f.timestamp)}</td>
                  <td><Badge>{CATEGORY_LABEL[f.category] ?? f.category}</Badge></td>
                  <td className="fault-msg">{f.message}</td>
                  <td className="mono">{f.model ?? '-'}</td>
                  <td>{f.provider ?? '-'}</td>
                  <td className="mono">{f.status ?? '-'}{f.upstream_status ? ` / ${f.upstream_status}` : ''}</td>
                  <td className="mono">{f.expires_at ? fmtTime(f.expires_at) : '-'}</td>
                  <td className="actions">
                    <Button
                      variant="ghost"
                      className="icon-btn"
                      title="删除该故障"
                      disabled={deleting === f.id}
                      onClick={() => void del(f.id)}
                    >
                      <IconTrash size={14} />
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
