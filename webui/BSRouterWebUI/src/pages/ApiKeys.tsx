import { useState } from 'react'
import { api } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Button, Empty, ErrorAlert, Field, Input } from '../components/ui'
import { IconPlus, IconTrash } from '../lib/icons'
import type { APIKeyEntry } from '../lib/types'

function fmtTime(ts: string): string {
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ts
  return d.toLocaleString()
}

export default function ApiKeys() {
  const { data, error, loading, reload } = useAsync(() => api.listKeys())
  const [name, setName] = useState('')
  const [generating, setGenerating] = useState(false)
  const [newKey, setNewKey] = useState<APIKeyEntry | null>(null)
  const [notice, setNotice] = useState('')
  const [err, setErr] = useState('')

  async function generate() {
    setGenerating(true)
    setErr('')
    try {
      const k = await api.generateKey(name)
      setNewKey(k)
      setName('')
      setNotice('已生成,请立即复制并妥善保存(之后列表仍可查看)')
      reload()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setGenerating(false)
    }
  }

  async function copy(key: string) {
    try {
      await navigator.clipboard.writeText(key)
      setNotice('已复制到剪贴板')
    } catch {
      setNotice('复制失败,请手动选择复制')
    }
  }

  async function remove(k: APIKeyEntry) {
    setErr('')
    try {
      await api.deleteKey(k.name)
      setNotice(`已删除 ${k.name}`)
      reload()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <div className="page-title">API Key 管理</div>
          <div className="page-sub">为下游模型请求生成、查看与删除受管 API Key(/api 鉴权)</div>
        </div>
      </div>

      {notice ? <div className="alert">{notice}</div> : null}
      {err ? <ErrorAlert text={err} /> : null}

      <section className="section">
        <div className="section-head">
          <span className="section-title">生成新 Key</span>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'flex-end' }}>
          <div style={{ flex: 1 }}>
            <Field label="名称">
              <Input
                value={name}
                placeholder="如 team-a / 某下游客户端"
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void generate()
                }}
              />
            </Field>
          </div>
          <Button variant="primary" onClick={() => void generate()} disabled={generating || name.trim() === ''}>
            <IconPlus size={15} />
            {generating ? '生成中…' : '生成'}
          </Button>
        </div>
      </section>

      {newKey ? (
        <section className="section">
          <div className="section-head">
            <span className="section-title">新生成的 Key</span>
          </div>
          <div className="table-wrap">
            <table className="table">
              <tbody>
                <tr>
                  <td style={{ width: 80 }}><strong>{newKey.name}</strong></td>
                  <td className="mono" style={{ wordBreak: 'break-all' }}>{newKey.key}</td>
                  <td style={{ width: 70 }}>
                    <Button variant="secondary" onClick={() => void copy(newKey.key)}>复制</Button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      ) : null}

      <section className="section">
        <div className="section-head">
          <span className="section-title">已有 Key</span>
          <Button variant="ghost" onClick={reload}>刷新</Button>
        </div>
        {error ? <ErrorAlert text={error} /> : null}
        {loading ? (
          <Empty text="加载中…" />
        ) : !data || data.length === 0 ? (
          <Empty text="暂无 API Key,先生成一个" />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>名称</th>
                  <th>Key</th>
                  <th>创建时间</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {data.map((k) => (
                  <tr key={k.name}>
                    <td><strong>{k.name}</strong></td>
                    <td className="mono" style={{ wordBreak: 'break-all' }}>{k.key}</td>
                    <td className="mono">{fmtTime(k.created_at)}</td>
                    <td className="actions">
                      <Button variant="ghost" onClick={() => void copy(k.key)}>复制</Button>
                      <Button variant="ghost" className="icon-btn" title="删除" onClick={() => void remove(k)}>
                        <IconTrash />
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  )
}
