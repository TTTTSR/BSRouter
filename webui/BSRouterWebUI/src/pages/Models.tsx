import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { api, KINDS, KINDS_LABEL } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Badge, Button, Empty, ErrorAlert, Field, Input, Modal, Select } from '../components/ui'
import { IconArrowDown, IconArrowUp, IconChevron, IconEdit, IconGrip, IconPlus, IconTrash, IconX } from '../lib/icons'
import type { AggregateModel, GroupConfig, Kind, ModelEntry } from '../lib/types'

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
  // 行内编辑中的窗口输入(id → 输入框文本);保存成功后清除,回退到列表值。
  const [draft, setDraft] = useState<Record<string, string>>({})
  const [busyId, setBusyId] = useState('')
  const [notice, setNotice] = useState('')
  const [editErr, setEditErr] = useState('')

  function windowValue(m: ModelEntry): string {
    if (m.id in draft) return draft[m.id]
    return m.context_window ? String(m.context_window) : ''
  }

  // 保存某模型的上下文窗口(k;空 = 清空回默认 200k)。
  async function saveWindow(m: ModelEntry) {
    if (!(m.id in draft)) return
    const v = draft[m.id]
    const k = v === undefined || v.trim() === '' ? 0 : Number(v)
    if (!Number.isInteger(k) || k < 0) {
      setEditErr(`无效的上下文窗口: ${v} (k,留空为默认 200k)`)
      return
    }
    const model = m.id.slice(m.id.indexOf('@') + 1)
    setBusyId(m.id)
    setEditErr('')
    try {
      await api.updateModelContextWindow(m.owned_by, model, k)
      setNotice(`已保存 ${m.id} 上下文窗口${k > 0 ? ` ${k}k` : '(默认 200k)'}`)
      setDraft((d) => {
        const next = { ...d }
        delete next[m.id]
        return next
      })
      reload()
    } catch (e) {
      setEditErr(`保存失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyId('')
    }
  }

  return (
    <CollapsibleSection title="已接入模型" actions={<Button variant="ghost" onClick={reload}>刷新</Button>}>
      {error ? <ErrorAlert text={error} /> : null}
      {notice ? <div className="alert">{notice}</div> : null}
      {editErr ? <div className="error-text">{editErr}</div> : null}
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
                <th>上下文窗口(k)<span className="faint" title="留空表示默认 200k"> ?</span></th>
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={m.id}>
                  <td className="mono"><strong>{m.id}</strong></td>
                  <td>{m.owned_by === 'unified' ? '统一供应商' : m.owned_by}</td>
                  <td>
                    {m.owned_by === 'unified' ? (
                      <span className="faint">—</span>
                    ) : (
                      <Input
                        type="number"
                        min={0}
                        className="table-window"
                        value={windowValue(m)}
                        placeholder="200"
                        disabled={busyId === m.id}
                        title="上下文窗口(k),留空默认 200k;回车或失焦保存"
                        onChange={(e) => setDraft((d) => ({ ...d, [m.id]: e.target.value }))}
                        onBlur={() => void saveWindow(m)}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter') {
                            e.preventDefault()
                            ;(e.target as HTMLInputElement).blur()
                          }
                        }}
                      />
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

// 聚合模型:查看各聚合的成员,剔除/添加回供应商;拖拽调整成员顺序(渠道优先级,
// 故障转移/负载均衡按此流转);开启/关闭该聚合的轮询负载均衡(默认关闭)。
function AggregatesSection() {
  const { data, error, loading, reload } = useAsync(() => api.listAggregates())
  const [busyName, setBusyName] = useState('')
  const [notice, setNotice] = useState('')
  // 开启负载均衡前的确认弹窗。
  const [confirmLb, setConfirmLb] = useState<AggregateModel | null>(null)
  // 拖拽状态。
  const [dragIdx, setDragIdx] = useState<number | null>(null)
  const [dragOverIdx, setDragOverIdx] = useState<number | null>(null)

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

  async function setLoadBalance(agg: AggregateModel, enabled: boolean) {
    setBusyName(agg.name)
    try {
      await api.updateAggregate(agg.name, agg.members, enabled)
      setNotice(enabled ? `已开启聚合 ${agg.name} 的负载均衡` : `已关闭聚合 ${agg.name} 的负载均衡`)
      reload()
    } catch (e) {
      setNotice(`更新失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  // 开启需确认(缓存命中率提醒);关闭直接生效。
  function toggleLb(agg: AggregateModel) {
    if (agg.load_balance) {
      void setLoadBalance(agg, false)
    } else {
      setConfirmLb(agg)
    }
  }

  // 移动成员到新位置(重排优先级)。
  function moveMember(agg: AggregateModel, from: number, to: number) {
    if (to < 0 || to >= agg.members.length || from === to) return
    const next = [...agg.members]
    const [m] = next.splice(from, 1)
    next.splice(to, 0, m)
    void setMembers(agg, next)
  }

  return (
    <CollapsibleSection title="聚合模型(渠道优先级)" actions={<Button variant="ghost" onClick={reload}>刷新</Button>}>
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
                <th>成员(拖拽调整优先级)</th>
                <th>可添加</th>
                <th>负载均衡</th>
              </tr>
            </thead>
            <tbody>
              {data.map((a) => (
                <tr key={a.name}>
                  <td className="mono"><strong>{a.name}</strong></td>
                  <td>
                    {a.members.length === 0 ? <span className="faint">-</span> : (
                      <div className="taglist-tags">
                        {a.members.map((p, i) => (
                          <span
                            key={p}
                            className={`tag tag-draggable${dragIdx === i ? ' tag-dragging' : ''}${dragOverIdx === i ? ' drag-over' : ''}`}
                            draggable={busyName !== a.name}
                            onDragStart={() => setDragIdx(i)}
                            onDragOver={(e) => { e.preventDefault(); setDragOverIdx(i) }}
                            onDragLeave={() => setDragOverIdx((cur) => (cur === i ? null : cur))}
                            onDrop={() => {
                              if (dragIdx !== null) { moveMember(a, dragIdx, i); setDragIdx(null); setDragOverIdx(null) }
                            }}
                            onDragEnd={() => { setDragIdx(null); setDragOverIdx(null) }}
                          >
                            <span className="tag-grip" title="拖拽调整优先级"><IconGrip size={11} /></span>
                            <span className="tag-label">{p}</span>
                            <button type="button" className="tag-move" title="上移" disabled={i === 0 || busyName === a.name}
                              onClick={() => moveMember(a, i, i - 1)}>
                              <IconArrowUp size={10} />
                            </button>
                            <button type="button" className="tag-move" title="下移" disabled={i === a.members.length - 1 || busyName === a.name}
                              onClick={() => moveMember(a, i, i + 1)}>
                              <IconArrowDown size={10} />
                            </button>
                            <button type="button" className="tag-x" title={`剔除 ${p}`} disabled={busyName === a.name}
                              onClick={() => void setMembers(a, a.members.filter((x) => x !== p))}>
                              <IconX size={11} />
                            </button>
                          </span>
                        ))}
                      </div>
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
                  <td>
                    <button
                      type="button"
                      className={`toggle${a.load_balance ? ' on' : ''}`}
                      title={a.load_balance ? '关闭负载均衡' : '开启负载均衡'}
                      disabled={busyName === a.name}
                      onClick={() => toggleLb(a)}
                    >
                      <span className="toggle-knob" />
                    </button>
                    <span className="faint" style={{ fontSize: 12, marginLeft: 6 }}>
                      {a.load_balance ? '轮询' : '优先级'}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {confirmLb ? (
        <Modal
          title="开启负载均衡"
          onClose={() => setConfirmLb(null)}
          footer={
            <>
              <Button onClick={() => setConfirmLb(null)}>取消</Button>
              <Button variant="primary" onClick={() => { const a = confirmLb; setConfirmLb(null); void setLoadBalance(a, true) }}>
                确认开启
              </Button>
            </>
          }
        >
          <p>开启负载均衡会大幅降低缓存命中率:轮询请求分散到不同渠道,上游 prompt 缓存基本失效。</p>
          <p><strong>确认开启 {confirmLb.name} 的负载均衡?</strong></p>
        </Modal>
      ) : null}
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
