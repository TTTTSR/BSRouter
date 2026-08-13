import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Button, Empty, ErrorAlert, Field, Input, Modal, Select } from '../components/ui'
import { IconEdit, IconPlus, IconTrash, IconX } from '../lib/icons'
import type { DshPresetConfig } from '../lib/types'

function DshPresetFormModal({
  title, initial, onClose, onSaved,
}: {
  title: string
  initial: DshPresetConfig | null
  onClose: () => void
  onSaved: (msg: string) => void
}) {
  const editing = initial !== null
  const [name, setName] = useState(initial?.name ?? '')
  const [description, setDescription] = useState(initial?.description ?? '')
  const [models, setModels] = useState<string[]>(initial?.models ?? [])
  const [pick, setPick] = useState('')
  const [allModels, setAllModels] = useState<string[]>([])
  const [apiKey, setApiKey] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let cancelled = false
    api.listModels().then((ml) => { if (!cancelled) setAllModels(ml.data.map((m) => m.id)) }).catch(() => {})
    return () => { cancelled = true }
  }, [])

  function addModel() {
    const m = pick.trim()
    if (!m || models.includes(m)) return
    setModels([...models, m])
    setPick('')
  }

  function removeModel(m: string) {
    setModels(models.filter((x) => x !== m))
  }

  async function submit() {
    setErr('')
    setBusy(true)
    // 与 zcode 预设一致:只配置名称/描述/模型列表/鉴权密钥,接口格式、base_url、
    // apiKeyEnv、display_name 均由 apply-local 自动派生。
    const cfg: DshPresetConfig = {
      name: name.trim(),
      description: description.trim(),
      api_key: apiKey.trim() || undefined,
      models,
    }
    try {
      if (editing) {
        await api.updateDshPreset(initial.name, cfg)
      } else {
        await api.addDshPreset(cfg)
      }
      onSaved(editing ? '预设已更新' : '预设已新增')
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
        <Field label="名称">
          <Input value={name} onChange={(e) => setName(e.target.value)} disabled={editing} placeholder="如 term-a" />
        </Field>
        <Field label="描述(可选)">
          <Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="如 开发终端" />
        </Field>
        <Field
          label="模型列表(可选)"
          hint="写入 llm-pi-ai.providers.<name>.models;留空回退网关全部可路由模型;接口格式/base_url/apiKeyEnv/display_name 自动派生"
          className="span2"
        >
          <div className="taglist">
            <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 8 }}>
              <Select value={pick} onChange={(e) => setPick(e.target.value)} style={{ flex: 1 }}>
                <option value="">选择网关模型…</option>
                {allModels.filter((m) => !models.includes(m)).map((m) => (
                  <option key={m} value={m}>{m}</option>
                ))}
              </Select>
              <Button variant="ghost" onClick={addModel} disabled={!pick}>添加</Button>
            </div>
            <div className="taglist-tags">
              {models.map((m) => (
                <span key={m} className="tag">
                  <span className="tag-label">{m}</span>
                  <button type="button" className="tag-x" title={`移除 ${m}`} onClick={() => removeModel(m)}>
                    <IconX size={11} />
                  </button>
                </span>
              ))}
              {models.length === 0 ? <span className="faint">未选择(将回退网关全部模型)</span> : null}
            </div>
          </div>
        </Field>
        <Field label="鉴权密钥(可选)" hint="留空时自动注入网关默认 key">
          <Input type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} placeholder="sk-..." />
        </Field>
      </div>
      {err ? <div className="error-text">{err}</div> : null}
    </Modal>
  )
}
export default function DshPresets() {
  const { data, error, loading, reload } = useAsync(() => api.listDshPresets())
  const { data: localInfo } = useAsync(() => api.checkLocal())
  const isLocal = localInfo?.local ?? false
  const [editing, setEditing] = useState<DshPresetConfig | null>(null)
  const [adding, setAdding] = useState(false)
  const [deleting, setDeleting] = useState<DshPresetConfig | null>(null)
  const [notice, setNotice] = useState('')
  const [busyName, setBusyName] = useState('')

  // 「应用本地」:把 BSRouter 作为自定义供应商覆盖到本机 ~/.dsh/settings.yaml(后端写文件)。
  async function applyLocal(name: string) {
    setBusyName(name)
    try {
      const r = await api.applyDshPresetLocal(name)
      setNotice('已应用 ' + name + ' 到 ' + r.path + '(' + (r.models ?? 0) + ' 个模型 / api=' + (r.api ?? '') + ' / env=' + (r.api_key_env ?? '') + ')')
    } catch (e) {
      setNotice('应用失败: ' + (e instanceof Error ? e.message : String(e)))
    } finally {
      setBusyName('')
    }
  }

  // 一键复制命令:设置 apiKeyEnv 环境变量(含真实密钥)后启动 dsh。
  async function copyCommand(name: string, kind: 'powershell' | 'bash') {
    setBusyName(name)
    try {
      const cmd = await api.dshPresetCommand(name)
      await navigator.clipboard.writeText(cmd[kind])
      setNotice('已复制 ' + (kind === 'powershell' ? 'PowerShell' : 'Bash') + ' 命令(' + name + ')')
    } catch (e) {
      setNotice('复制失败: ' + (e instanceof Error ? e.message : String(e)))
    } finally {
      setBusyName('')
    }
  }

  async function confirmDelete() {
    if (!deleting) return
    try {
      await api.deleteDshPreset(deleting.name)
      setNotice('已删除 ' + deleting.name)
      setDeleting(null)
      reload()
    } catch (e) {
      setNotice('删除失败: ' + (e instanceof Error ? e.message : String(e)))
    }
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <div className="page-title">dsh 预设</div>
          <div className="page-sub">
            把 BSRouter 作为一条自定义供应商覆盖进本地 ~/.dsh/settings.yaml(保留其余内置/自定义供应商,接口格式/base_url/apiKeyEnv 自动派生);
            {isLocal
              ? '本机访问,可一键覆盖本地 dsh 配置'
              : <span className="faint">当前非本机访问,「覆盖本地配置」不可用</span>}
          </div>
        </div>
        <Button variant="primary" onClick={() => setAdding(true)}>
          <IconPlus size={15} />
          新增预设
        </Button>
      </div>

      {notice ? <div className="alert">{notice}</div> : null}

      {error ? <ErrorAlert text={error} /> : null}

      {loading ? (
        <Empty text="加载中…" />
      ) : !data || data.length === 0 ? (
        <Empty text="暂无预设,点击「新增预设」创建第一条" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>名称</th>
                <th>描述</th>
                <th>模型</th>
                <th>鉴权</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {data.map((p) => (
                <tr key={p.name}>
                  <td><strong>{p.name}</strong></td>
                  <td>{p.description || <span className="faint">-</span>}</td>
                  <td className="mono">
                    {(p.models ?? []).length > 0
                      ? (p.models ?? []).join(' · ')
                      : <span className="faint">全部模型</span>}
                  </td>
                  <td className="mono">{p.api_key || <span className="faint">自动</span>}</td>
                  <td className="actions">
                    {isLocal ? (
                      <Button variant="ghost" disabled={busyName === p.name} title="覆盖本地 ~/.dsh/settings.yaml" onClick={() => void applyLocal(p.name)}>
                        应用本地
                      </Button>
                    ) : null}
                    <Button variant="ghost" disabled={busyName === p.name} title="复制 PowerShell 启动命令" onClick={() => void copyCommand(p.name, 'powershell')}>
                      PS
                    </Button>
                    <Button variant="ghost" disabled={busyName === p.name} title="复制 Bash 启动命令" onClick={() => void copyCommand(p.name, 'bash')}>
                      命令
                    </Button>
                    <Button variant="ghost" className="icon-btn" title="编辑" onClick={() => setEditing(p)}>
                      <IconEdit />
                    </Button>
                    <Button variant="ghost" className="icon-btn" title="删除" onClick={() => setDeleting(p)}>
                      <IconTrash />
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {adding ? (
        <DshPresetFormModal
          title="新增预设"
          initial={null}
          onClose={() => setAdding(false)}
          onSaved={(msg) => { setNotice(msg); setAdding(false); reload() }}
        />
      ) : null}

      {editing ? (
        <DshPresetFormModal
          title={'编辑预设 ' + editing.name}
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={(msg) => { setNotice(msg); setEditing(null); reload() }}
        />
      ) : null}

      {deleting ? (
        <Modal
          title="删除预设"
          onClose={() => setDeleting(null)}
          footer={
            <>
              <Button onClick={() => setDeleting(null)}>取消</Button>
              <Button variant="primary" onClick={() => void confirmDelete()}>删除</Button>
            </>
          }
        >
          <p>确定删除预设 <strong>{deleting.name}</strong> 吗?</p>
        </Modal>
      ) : null}
    </div>
  )
}