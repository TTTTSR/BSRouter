import { useState } from 'react'
import { api, KINDS, KINDS_LABEL } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Badge, Button, Empty, ErrorAlert, Field, Input, Modal, Select, Spinner } from '../components/ui'
import { IconActivity, IconEdit, IconPing, IconPlus, IconRefresh, IconTrash } from '../lib/icons'
import type { Kind, ModelConfig, PingResult, ProviderConfig } from '../lib/types'

function ProviderFormModal({
  title, initial, onClose, onSaved,
}: {
  title: string
  initial: ProviderConfig | null
  onClose: () => void
  onSaved: (msg: string) => void
}) {
  const editing = initial !== null
  const [kind, setKind] = useState<Kind>(initial?.kind ?? 'completion')
  const [name, setName] = useState(initial?.name ?? '')
  const [baseUrl, setBaseUrl] = useState(initial?.base_url ?? '')
  const [apiKey, setApiKey] = useState('')
  const [models, setModels] = useState<ModelConfig[]>(initial?.models ?? [])
  const [usageUrl, setUsageUrl] = useState(initial?.usage_url ?? '')
  const [modelsUrl, setModelsUrl] = useState(initial?.models_url ?? '')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [fetching, setFetching] = useState(false)
  const [fetchErr, setFetchErr] = useState('')

  async function submit() {
    setErr('')
    if (models.some((m) => m.name.trim() === '')) {
      setErr('存在模型名称为空的行,请填写或删除该行')
      return
    }
    setBusy(true)
    const cfg: ProviderConfig = {
      kind, name: name.trim(), base_url: baseUrl.trim(), api_key: apiKey.trim(),
      models: models.map((m) => ({ name: m.name.trim(), kind: m.kind })),
      usage_url: usageUrl.trim(), models_url: modelsUrl.trim(),
    }
    try {
      if (editing) {
        await api.updateProvider(initial.name, cfg)
      } else {
        await api.addProvider(cfg)
      }
      onSaved(editing ? '供应商已更新' : '供应商已新增')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  // 从已填的模型列表 URL 拉取模型并填充「模型列表」。
  // 编辑场景下 api_key 留空(保留原密钥),通过 name 让后端复用已存密钥。
  async function fetchModels() {
    setFetching(true)
    setFetchErr('')
    try {
      const r = await api.fetchModels({
        name: name.trim(), kind, base_url: baseUrl.trim(), api_key: apiKey.trim(), models_url: modelsUrl.trim(),
      })
      // 拉取到的模型使用供应商默认接口格式(模型级 Kind 留空)。
      setModels(r.models.map((m) => ({ name: m, kind: '' })))
    } catch (e) {
      setFetchErr(e instanceof Error ? e.message : String(e))
    } finally {
      setFetching(false)
    }
  }

  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" onClick={() => void submit()} disabled={busy || name.trim() === '' || baseUrl.trim() === ''}>
            {busy ? '保存中…' : '保存'}
          </Button>
        </>
      }
    >
      <div className="form-grid">
        <Field label="接口格式">
          <Select value={kind} onChange={(e) => setKind(e.target.value as Kind)}>
            {KINDS.map((k) => (
              <option key={k} value={k}>{KINDS_LABEL[k]}</option>
            ))}
          </Select>
        </Field>
        <Field label="供应商名">
          <Input value={name} onChange={(e) => setName(e.target.value)} disabled={editing} placeholder="如 openai" />
        </Field>
        <Field label="Base URL" hint={editing ? '修改会立即影响后续转发' : undefined}>
          <Input value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder="https://api.openai.com" />
        </Field>
        <Field label="API Key" hint={editing ? '留空表示保留原密钥' : undefined}>
          <Input type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} placeholder={editing ? '(保留原密钥)' : 'sk-...'} />
        </Field>
        <Field label="模型列表" hint="每个模型一行;接口格式留空则使用供应商默认" className="span2">
          <ModelRows models={models} onChange={setModels} />
        </Field>
        <Field label="用量查询 URL(可选)">
          <Input value={usageUrl} onChange={(e) => setUsageUrl(e.target.value)} />
        </Field>
        <Field label="模型列表 URL(可选)" hint="默认 {base}/v1/models">
          <Input value={modelsUrl} onChange={(e) => setModelsUrl(e.target.value)} />
        </Field>
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12 }}>
        <Button variant="secondary" onClick={() => void fetchModels()} disabled={fetching || baseUrl.trim() === ''}>
          {fetching ? <span className="spinner" style={{ width: 12, height: 12 }} /> : <IconRefresh size={14} />}
          获取模型列表
        </Button>
        <span className="form-hint">{`按已填模型列表 URL 拉取并填充(留空则用 ${baseUrl.trim() || 'base'}/v1/models)`}</span>
      </div>
      {fetchErr ? <div className="error-text">{fetchErr}</div> : null}
      {err ? <div className="error-text">{err}</div> : null}
    </Modal>
  )
}

// 接口格式下拉:默认(留空=供应商默认)/ anthropic / completion / responses。
function KindSelect({ value, onChange }: { value: Kind | ''; onChange: (k: Kind | '') => void }) {
  return (
    <Select value={value} onChange={(e) => onChange(e.target.value as Kind | '')}>
      <option value="">默认</option>
      {KINDS.map((k) => (
        <option key={k} value={k}>{KINDS_LABEL[k]}</option>
      ))}
    </Select>
  )
}

// 模型按行编辑:每行 = 模型名 + 接口格式下拉(留空=供应商默认)。
function ModelRows({ models, onChange }: { models: ModelConfig[]; onChange: (v: ModelConfig[]) => void }) {
  const [draft, setDraft] = useState('')
  const [draftKind, setDraftKind] = useState<Kind | ''>('')

  function add() {
    const name = draft.trim()
    if (name === '' || models.some((m) => m.name === name)) return
    onChange([...models, { name, kind: draftKind }])
    setDraft('')
  }

  return (
    <div className="model-rows">
      {models.map((m, i) => (
        <div key={i} className="model-row">
          <Input
            value={m.name}
            placeholder="模型名,如 gpt-4o"
            onChange={(e) => onChange(models.map((x, idx) => (idx === i ? { ...x, name: e.target.value } : x)))}
          />
          <KindSelect
            value={m.kind ?? ''}
            onChange={(k) => onChange(models.map((x, idx) => (idx === i ? { ...x, kind: k } : x)))}
          />
          <Button variant="ghost" className="icon-btn" title="移除" onClick={() => onChange(models.filter((_, idx) => idx !== i))}>
            <IconTrash />
          </Button>
        </div>
      ))}
      <div className="model-row">
        <Input
          value={draft}
          placeholder="新模型名"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              add()
            }
          }}
        />
        <KindSelect value={draftKind} onChange={setDraftKind} />
        <Button variant="secondary" onClick={add} disabled={draft.trim() === ''}>添加</Button>
      </div>
    </div>
  )
}

export default function Providers() {
  const { data, error, loading, reload } = useAsync(() => api.listProviders())
  const [editing, setEditing] = useState<ProviderConfig | null>(null)
  const [adding, setAdding] = useState(false)
  const [deleting, setDeleting] = useState<ProviderConfig | null>(null)
  const [notice, setNotice] = useState('')
  const [pingResult, setPingResult] = useState<{ name: string; result: PingResult } | null>(null)
  const [usageResult, setUsageResult] = useState<{ name: string; body: string } | null>(null)
  const [busyName, setBusyName] = useState('')

  async function runPing(name: string) {
    setBusyName(name)
    try {
      setPingResult({ name, result: await api.pingProvider(name) })
    } catch (e) {
      setNotice(`测试失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  async function runSync(name: string) {
    setBusyName(name)
    try {
      const r = await api.syncModels(name)
      setNotice(`${name} 模型已同步:${r.count} 个`)
      reload()
    } catch (e) {
      setNotice(`同步失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  async function runUsage(name: string) {
    setBusyName(name)
    try {
      const r = (await api.providerUsage(name)) as unknown
      setUsageResult({ name, body: JSON.stringify(r, null, 2) })
    } catch (e) {
      setNotice(`用量查询失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  async function confirmDelete() {
    if (!deleting) return
    try {
      await api.deleteProvider(deleting.name)
      setNotice(`已删除 ${deleting.name}`)
      setDeleting(null)
      reload()
    } catch (e) {
      setNotice(`删除失败: ${e instanceof Error ? e.message : String(e)}`)
    }
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <div className="page-title">供应商管理</div>
          <div className="page-sub">接入并管理真实的大模型供应商</div>
        </div>
        <Button variant="primary" onClick={() => setAdding(true)}>
          <IconPlus size={15} />
          新增供应商
        </Button>
      </div>

      {notice ? <div className="alert">{notice}</div> : null}
      {error ? <ErrorAlert text={error} /> : null}

      {loading ? (
        <Empty text="加载中…" />
      ) : !data || data.length === 0 ? (
        <Empty text="暂无供应商,点击「新增供应商」接入" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>名称</th>
                <th>格式</th>
                <th>Base URL</th>
                <th>模型</th>
                <th>API Key</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {data.map((p) => (
                <tr key={p.name}>
                  <td><strong>{p.name}</strong></td>
                  <td><Badge>{p.kind}</Badge></td>
                  <td className="mono">{p.base_url}</td>
                  <td className="mono">{(p.models ?? []).length > 0 ? p.models!.map((m) => m.name).join(', ') : <span className="faint">-</span>}</td>
                  <td className="mono">{p.api_key || '-'}</td>
                  <td className="actions">
                    <Button variant="ghost" className="icon-btn" title="连通性测试" disabled={busyName === p.name} onClick={() => void runPing(p.name)}>
                      <IconActivity />
                    </Button>
                    <Button variant="ghost" className="icon-btn" title="同步模型" disabled={busyName === p.name} onClick={() => void runSync(p.name)}>
                      <IconRefresh />
                    </Button>
                    <Button variant="ghost" className="icon-btn" title="查询用量" disabled={busyName === p.name} onClick={() => void runUsage(p.name)}>
                      <IconPing />
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

      {busyName ? <div style={{ marginTop: 12 }}><Spinner /> <span className="muted"> 处理 {busyName}…</span></div> : null}

      {adding ? (
        <ProviderFormModal
          title="新增供应商"
          initial={null}
          onClose={() => setAdding(false)}
          onSaved={(msg) => { setNotice(msg); setAdding(false); reload() }}
        />
      ) : null}

      {editing ? (
        <ProviderFormModal
          title={`编辑供应商 ${editing.name}`}
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={(msg) => { setNotice(msg); setEditing(null); reload() }}
        />
      ) : null}

      {deleting ? (
        <Modal
          title="删除供应商"
          onClose={() => setDeleting(null)}
          footer={
            <>
              <Button onClick={() => setDeleting(null)}>取消</Button>
              <Button variant="primary" onClick={() => void confirmDelete()}>删除</Button>
            </>
          }
        >
          <p>确定删除供应商 <strong>{deleting.name}</strong> 吗?该操作会同步从本地配置移除。</p>
        </Modal>
      ) : null}

      {pingResult ? (
        <Modal title={`连通性测试 ${pingResult.name}`} onClose={() => setPingResult(null)}>
          <p>
            {pingResult.result.ok ? (
              <Badge invert>可达</Badge>
            ) : (
              <Badge>不可达</Badge>
            )}
            <span className="muted"> 状态码 {pingResult.result.status_code ?? '-'} · 耗时 {pingResult.result.latency_ms}ms</span>
          </p>
          {pingResult.result.error ? <div className="error-text mono">{pingResult.result.error}</div> : null}
        </Modal>
      ) : null}

      {usageResult ? (
        <Modal title={`用量查询 ${usageResult.name}`} onClose={() => setUsageResult(null)} wide>
          <pre className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', margin: 0 }}>{usageResult.body}</pre>
        </Modal>
      ) : null}
    </div>
  )
}
