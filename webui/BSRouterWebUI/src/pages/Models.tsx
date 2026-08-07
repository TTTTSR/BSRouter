import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { api, KINDS, KINDS_LABEL } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Badge, Button, Empty, ErrorAlert, Field, Input, Modal, Select } from '../components/ui'
import { IconChevron, IconEdit, IconPlus, IconTrash, IconX } from '../lib/icons'
import type { AggregateModel, GroupConfig, Kind } from '../lib/types'

// 可折叠栏目:点击标题展开/收起,内容高度平滑过渡。
function CollapsibleSection({ title, actions, children }: {
  title: string
  actions?: ReactNode
  children: ReactNode
}) {
  const [open, setOpen] = useState(true)
  return (
    <section className="section">
      <div className="section-head">
        <button type="button" className="section-toggle" onClick={() => setOpen((v) => !v)}>
          <span className={`chev${open ? '' : ' chev-closed'}`}><IconChevron size={14} /></span>
          <span className="section-title">{title}</span>
        </button>
        <div className="section-actions">{actions}</div>
      </div>
      <div className={`collapse${open ? '' : ' collapse-closed'}`}>
        <div className="collapse-inner">{children}</div>
      </div>
    </section>
  )
}

function GroupFormModal({
  title, initial, onClose, onSaved,
}: {
  title: string
  initial: GroupConfig | null
  onClose: () => void
  onSaved: (msg: string) => void
}) {
  const editing = initial !== null
  const [name, setName] = useState(initial?.name ?? '')
  const [kind, setKind] = useState<Kind>(initial?.kind ?? 'completion')
  // URL 只存 /api 后的最后一段(默认 /api/{name});编辑时剥离 /api/ 前缀。
  const [url, setUrl] = useState(initial?.url?.replace(/^\/api\//, '') ?? '')
  const [models, setModels] = useState<string[]>(initial?.models ?? [])
  const [available, setAvailable] = useState<string[]>([])
  const [picked, setPicked] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  // 可用模型(含聚合裸名)供下拉选择。
  useEffect(() => {
    api.listModels().then((m) => setAvailable(m.data.map((e) => e.id))).catch(() => {})
  }, [])

  function addAvailable() {
    if (picked && !models.includes(picked)) setModels([...models, picked])
    setPicked('')
  }

  async function submit() {
    setBusy(true)
    setErr('')
    // 用户只填最后一段;自动拼回 /api/ 前缀(兼容粘贴了 /api/ 前缀的情况)。
    const seg = url.trim().replace(/^\/api\//, '')
    const cfg: GroupConfig = {
      name: name.trim(), kind, url: seg === '' ? '' : '/api/' + seg,
      models: models.map((m) => m.trim()).filter((m) => m !== ''),
    }
    try {
      if (editing) {
        await api.updateGroup(initial.name, cfg)
      } else {
        await api.addGroup(cfg)
      }
      onSaved(editing ? '分组已更新' : '分组已新增')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" onClick={() => void submit()} disabled={busy || name.trim() === ''}>
            {busy ? '保存中…' : '保存'}
          </Button>
        </>
      }
    >
      <div className="form-grid">
        <Field label="分组名">
          <Input value={name} onChange={(e) => setName(e.target.value)} disabled={editing} placeholder="如 team-a" />
        </Field>
        <Field label="接口格式">
          <Select value={kind} onChange={(e) => setKind(e.target.value as Kind)}>
            {KINDS.map((k) => (
              <option key={k} value={k}>{KINDS_LABEL[k]}</option>
            ))}
          </Select>
        </Field>
        <Field label="URL 后缀(可选)" hint="默认 /api/{name};只填最后一段,如 team-a">
          <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="team-a" />
        </Field>
        <Field label="模型列表" hint="仅从下方下拉选择添加" className="span2">
          {models.length === 0 ? (
            <span className="faint">暂无模型,从下方选择添加</span>
          ) : (
            <div className="taglist">
              <div className="taglist-tags">
                {models.map((m) => (
                  <span key={m} className="tag">
                    <span className="tag-label">{m}</span>
                    <button type="button" className="tag-x" title={`移除 ${m}`} onClick={() => setModels(models.filter((x) => x !== m))}>
                      <IconX size={11} />
                    </button>
                  </span>
                ))}
              </div>
            </div>
          )}
        </Field>
        <Field label="从可用模型添加" hint="含聚合模型(裸名)" className="span2">
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <Select value={picked} onChange={(e) => setPicked(e.target.value)}>
                <option value="">请选择…</option>
                {available.filter((id) => !models.includes(id)).map((id) => <option key={id} value={id}>{id}</option>)}
              </Select>
            </div>
            <div style={{ flex: 'none' }}>
              <Button variant="secondary" onClick={addAvailable} disabled={!picked}>添加</Button>
            </div>
          </div>
        </Field>
      </div>
      {err ? <div className="error-text">{err}</div> : null}
    </Modal>
  )
}

function GroupsSection() {
  const { data, error, loading, reload } = useAsync(() => api.listGroups())
  const [editing, setEditing] = useState<GroupConfig | null>(null)
  const [adding, setAdding] = useState(false)
  const [deleting, setDeleting] = useState<GroupConfig | null>(null)
  const [notice, setNotice] = useState('')

  async function confirmDelete() {
    if (!deleting) return
    try {
      await api.deleteGroup(deleting.name)
      setNotice(`已删除分组 ${deleting.name}`)
      setDeleting(null)
      reload()
    } catch (e) {
      setNotice(`删除失败: ${e instanceof Error ? e.message : String(e)}`)
    }
  }

  return (
    <>
      <CollapsibleSection
        title="模型分组(虚拟供应商)"
        actions={<Button onClick={() => setAdding(true)}><IconPlus size={15} />新增分组</Button>}
      >
        {notice ? <div className="alert">{notice}</div> : null}
        {error ? <ErrorAlert text={error} /> : null}
      {loading ? (
        <Empty text="加载中…" />
      ) : !data || data.length === 0 ? (
        <Empty text="暂无分组,点击「新增分组」创建虚拟供应商" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>名称</th>
                <th>格式</th>
                <th>URL</th>
                <th>模型</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {data.map((g) => (
                <tr key={g.name}>
                  <td><strong>{g.name}</strong></td>
                  <td><Badge>{g.kind}</Badge></td>
                  <td className="mono">{g.url ?? '/api/' + g.name}</td>
                  <td className="mono">{g.models.length > 0 ? g.models.join(', ') : <span className="faint">-</span>}</td>
                  <td className="actions">
                    <Button variant="ghost" className="icon-btn" title="编辑" onClick={() => setEditing(g)}>
                      <IconEdit />
                    </Button>
                    <Button variant="ghost" className="icon-btn" title="删除" onClick={() => setDeleting(g)}>
                      <IconTrash />
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      </CollapsibleSection>

      {adding ? (
        <GroupFormModal
          title="新增分组"
          initial={null}
          onClose={() => setAdding(false)}
          onSaved={(msg) => { setNotice(msg); setAdding(false); reload() }}
        />
      ) : null}
      {editing ? (
        <GroupFormModal
          title={`编辑分组 ${editing.name}`}
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={(msg) => { setNotice(msg); setEditing(null); reload() }}
        />
      ) : null}
      {deleting ? (
        <Modal
          title="删除分组"
          onClose={() => setDeleting(null)}
          footer={
            <>
              <Button onClick={() => setDeleting(null)}>取消</Button>
              <Button variant="primary" onClick={() => void confirmDelete()}>删除</Button>
            </>
          }
        >
          <p>确定删除分组 <strong>{deleting.name}</strong> 吗?该分组的下游调用将立即失效。</p>
        </Modal>
      ) : null}
    </>
  )
}

function ModelsSection() {
  const { data, error, loading, reload } = useAsync(() => api.listModels())
  const models = data?.data ?? []

  return (
    <CollapsibleSection title="已接入模型" actions={<Button variant="ghost" onClick={reload}>刷新</Button>}>
      {error ? <ErrorAlert text={error} /> : null}
      {loading ? (
        <Empty text="加载中…" />
      ) : models.length === 0 ? (
        <Empty text="暂无模型,请先在供应商管理中配置或同步模型" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>模型 ID</th>
                <th>所属</th>
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={m.id}>
                  <td className="mono"><strong>{m.id}</strong></td>
                  <td>{m.owned_by === 'unified' ? '统一供应商' : m.owned_by}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </CollapsibleSection>
  )
}

// 聚合模型:查看各聚合的成员,剔除/添加回供应商。
function AggregatesSection() {
  const { data, error, loading, reload } = useAsync(() => api.listAggregates())
  const [busyName, setBusyName] = useState('')
  const [notice, setNotice] = useState('')

  async function setMembers(agg: AggregateModel, members: string[]) {
    setBusyName(agg.name)
    try {
      await api.updateAggregate(agg.name, members)
      setNotice(`已更新聚合 ${agg.name}`)
      reload()
    } catch (e) {
      setNotice(`更新失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  return (
    <CollapsibleSection title="聚合模型(负载均衡)" actions={<Button variant="ghost" onClick={reload}>刷新</Button>}>
      {notice ? <div className="alert">{notice}</div> : null}
      {error ? <ErrorAlert text={error} /> : null}
      {loading ? (
        <Empty text="加载中…" />
      ) : !data || data.length === 0 ? (
        <Empty text="暂无聚合模型" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>模型</th>
                <th>成员</th>
                <th>可添加</th>
              </tr>
            </thead>
            <tbody>
              {data.map((a) => (
                <tr key={a.name}>
                  <td className="mono"><strong>{a.name}</strong></td>
                  <td>
                    {a.members.length === 0 ? <span className="faint">-</span> : (
                      a.members.map((p) => (
                        <span key={p} className="tag">
                          <span className="tag-label">{p}</span>
                          <button type="button" className="tag-x" title={`剔除 ${p}`} disabled={busyName === a.name}
                            onClick={() => void setMembers(a, a.members.filter((x) => x !== p))}>
                            <IconX size={11} />
                          </button>
                        </span>
                      ))
                    )}
                  </td>
                  <td>
                    {a.available.length === 0 ? <span className="faint">-</span> : (
                      a.available.map((p) => (
                        <span key={p} className="tag">
                          <span className="tag-label">{p}</span>
                          <button type="button" className="tag-x" title={`添加 ${p}`} disabled={busyName === a.name}
                            onClick={() => void setMembers(a, [...a.members, p])}>
                            <IconPlus size={11} />
                          </button>
                        </span>
                      ))
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </CollapsibleSection>
  )
}

export default function Models() {
  return (
    <div>
      <div className="page-head">
        <div>
          <div className="page-title">模型管理</div>
          <div className="page-sub">管理已接入的所有模型、聚合模型与虚拟供应商分组</div>
        </div>
      </div>
      <ModelsSection />
      <AggregatesSection />
      <GroupsSection />
    </div>
  )
}
