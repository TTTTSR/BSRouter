import { useState } from 'react'
import { api, KINDS, KINDS_LABEL } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Badge, Button, Empty, ErrorAlert, Field, Input, Modal, Select, Spinner } from '../components/ui'
import { IconActivity, IconEdit, IconGrid, IconPing, IconPlus, IconRefresh, IconTrash } from '../lib/icons'
import type { Kind, ModelConfig, PingResult, ProviderConfig, ProviderTemplate } from '../lib/types'

function ProviderFormModal({
  title, initial, seed, onClose, onSaved,
}: {
  title: string
  initial: ProviderConfig | null
  seed?: ProviderConfig | null
  onClose: () => void
  onSaved: (msg: string) => void
}) {
  const editing = initial !== null
  const base = initial ?? seed ?? null
  const [kind, setKind] = useState<Kind>(base?.kind ?? 'completion')
  const [name, setName] = useState(base?.name ?? '')
  const [baseUrl, setBaseUrl] = useState(base?.base_url ?? '')
  const [basePath, setBasePath] = useState(base?.base_path ?? '')
  const [apiKey, setApiKey] = useState('')
  const [models, setModels] = useState<ModelConfig[]>(base?.models ?? [])
  const [usageUrl, setUsageUrl] = useState(base?.usage_url ?? '')
  const [modelsUrl, setModelsUrl] = useState(base?.models_url ?? '')
  // 故障阻塞配置:限流(默认 429、120 分钟、启用)与余额不足(默认 402)均可在供应商
  // 编辑表单自定义;余额不足留空 = 默认 402,填 0 = 禁用该分类。
  const [rateLimitEnabled, setRateLimitEnabled] = useState(base?.rate_limit_enabled !== false)
  const [rateLimitStatus, setRateLimitStatus] = useState(base?.rate_limit_status === undefined ? '' : String(base.rate_limit_status))
  const [rateLimitDuration, setRateLimitDuration] = useState(base?.rate_limit_duration_minutes === undefined ? '' : String(base.rate_limit_duration_minutes))
  const [insufficientStatus, setInsufficientStatus] = useState(base?.insufficient_balance_status === undefined ? '' : String(base.insufficient_balance_status))
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
    const rls = rateLimitStatus.trim()
    const rld = rateLimitDuration.trim()
    const ibs = insufficientStatus.trim()
    const rlsN = rls === '' ? undefined : Number(rls)
    const rldN = rld === '' ? undefined : Number(rld)
    const ibsN = ibs === '' ? undefined : Number(ibs)
    if ((rlsN !== undefined && (!Number.isInteger(rlsN) || rlsN < 400 || rlsN > 599)) ||
        (rldN !== undefined && (!Number.isInteger(rldN) || rldN < 1)) ||
        (ibsN !== undefined && (!Number.isInteger(ibsN) || ibsN < 0 || ibsN > 599))) {
      setErr('限流错误码需为 400-599、时长需为正整数(分钟);余额不足错误码需为 0(禁用)或 400-599;留空用默认')
      return
    }
    setBusy(true)
    const cfg: ProviderConfig = {
      kind, name: name.trim(), base_url: baseUrl.trim(), base_path: basePath.trim(), api_key: apiKey.trim(),
      models: models.map((m) => ({ name: m.name.trim(), kind: m.kind, kinds: m.kinds, context_window: m.context_window })),
      usage_url: usageUrl.trim(), models_url: modelsUrl.trim(),
      rate_limit_enabled: rateLimitEnabled, rate_limit_status: rlsN, rate_limit_duration_minutes: rldN,
      insufficient_balance_status: ibsN,
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
      // 拉取到的模型使用供应商默认接口格式(模型级 Kind 留空);已存在的模型保留
      // 其上下文窗口配置,避免一次拉取清空手工填写的窗口。
      const existingWindow = new Map(
        models.filter((m) => m.context_window != null && m.context_window > 0)
          .map((m) => [m.name, m.context_window]))
      setModels(r.models.map((m) => ({ name: m, kind: '', context_window: existingWindow.get(m) })))
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
        <Field label="Base Path" hint="base_url 与端点之间的路径段;留空默认 /v1(智谱 /api/paas/v4、百度 /v2、火山 /api/v3 等已由模板填好)">
          <Input value={basePath} onChange={(e) => setBasePath(e.target.value)} placeholder="/v1" />
        </Field>
        <Field label="API Key" hint={editing ? '留空表示保留原密钥' : undefined}>
          <Input type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} placeholder={editing ? '(保留原密钥)' : 'sk-...'} />
        </Field>
        <Field label="模型列表" hint="每个模型一行;接口格式留空则使用供应商默认;窗口(k)留空默认 200k" className="span2">
          <ModelRows models={models} onChange={setModels} />
        </Field>
        <Field label="用量查询 URL(可选)">
          <Input value={usageUrl} onChange={(e) => setUsageUrl(e.target.value)} />
        </Field>
        <Field label="模型列表 URL(可选)" hint="默认 {base}/v1/models">
          <Input value={modelsUrl} onChange={(e) => setModelsUrl(e.target.value)} />
        </Field>
        <Field label="限流阻塞(可选)" hint="启用后:上游返回该错误码时阻塞该供应商,时长到期自动解除;默认 429 / 120 分钟(适配 codingplan 等 5 小时限额提供商)">
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
            <label style={{ display: 'flex', alignItems: 'center', gap: 6, whiteSpace: 'nowrap' }}>
              <input type="checkbox" checked={rateLimitEnabled} onChange={(e) => setRateLimitEnabled(e.target.checked)} />
              <span>启用</span>
            </label>
            <Input value={rateLimitStatus} onChange={(e) => setRateLimitStatus(e.target.value)} placeholder="429" title="触发限流阻塞的错误码" style={{ width: 84 }} />
            <span className="faint">码</span>
            <Input value={rateLimitDuration} onChange={(e) => setRateLimitDuration(e.target.value)} placeholder="120" title="限流阻塞时长(分钟)" style={{ width: 84 }} />
            <span className="faint">分钟</span>
          </div>
        </Field>
        <Field label="余额不足阻塞错误码(可选)" hint="上游返回该状态码时阻塞直到手动解除;留空默认 402,填 0 禁用">
          <Input value={insufficientStatus} onChange={(e) => setInsufficientStatus(e.target.value)} placeholder="402" />
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

// 模型支持的接口格式多选:勾选任意格式则该模型支持这些格式(可多选);全部不勾 =
// 继承供应商默认接口格式。
function KindCheckboxes({ value, onChange }: { value: Kind[]; onChange: (kinds: Kind[]) => void }) {
  function toggle(k: Kind) {
    if (value.includes(k)) {
      onChange(value.filter((x) => x !== k))
    } else {
      onChange([...value, k])
    }
  }
  return (
    <div className="kind-checkboxes">
      {KINDS.map((k) => (
        <label key={k} className={`kind-check${value.includes(k) ? ' on' : ''}`}>
          <input type="checkbox" checked={value.includes(k)} onChange={() => toggle(k)} />
          <span>{KINDS_LABEL[k]}</span>
        </label>
      ))}
      <span className={`kind-inherit${value.length === 0 ? ' on' : ''}`}>继承默认</span>
    </div>
  )
}

// 从旧配置(kind 单格式)与新配置(kinds 多格式)推导当前选中格式。
function modelKinds(m: ModelConfig): Kind[] {
  if (m.kinds && m.kinds.length > 0) return m.kinds
  if (m.kind) return [m.kind as Kind]
  return []
}

// 解析上下文窗口输入(k):空串 → 未设置(默认 200k);非法/负数/非整数 → 未设置。
function parseWindow(v: string): number | undefined {
  const t = v.trim()
  if (t === '') return undefined
  const n = Number(t)
  return Number.isFinite(n) && Number.isInteger(n) && n >= 0 ? n : undefined
}

// 上下文窗口数字输入(k 为单位,留空默认 200k)。
function WindowInput({ value, onChange }: { value?: number; onChange: (v?: number) => void }) {
  return (
    <Input
      type="number"
      min={0}
      className="model-window"
      value={value ?? ''}
      placeholder="200"
      title="上下文窗口(k),留空默认 200k"
      onChange={(e) => onChange(parseWindow(e.target.value))}
    />
  )
}

// 模型按行编辑:每行 = 模型名 + 上下文窗口 + 支持的接口格式多选(全不勾=供应商默认)。
function ModelRows({ models, onChange }: { models: ModelConfig[]; onChange: (v: ModelConfig[]) => void }) {
  const [draft, setDraft] = useState('')
  const [draftKinds, setDraftKinds] = useState<Kind[]>([])
  const [draftWindow, setDraftWindow] = useState('')

  function update(i: number, m: ModelConfig) {
    onChange(models.map((x, idx) => (idx === i ? m : x)))
  }

  function add() {
    const name = draft.trim()
    if (name === '' || models.some((m) => m.name === name)) return
    onChange([...models, { name, kinds: draftKinds.length > 0 ? draftKinds : undefined, context_window: parseWindow(draftWindow) }])
    setDraft('')
    setDraftKinds([])
    setDraftWindow('')
  }

  return (
    <div className="model-rows">
      {models.map((m, i) => (
        <div key={i} className="model-row">
          <Input
            value={m.name}
            placeholder="模型名,如 gpt-4o"
            onChange={(e) => update(i, { ...m, name: e.target.value })}
          />
          <WindowInput value={m.context_window} onChange={(v) => update(i, { ...m, context_window: v })} />
          <KindCheckboxes
            value={modelKinds(m)}
            onChange={(kinds) => update(i, { ...m, kind: undefined, kinds: kinds.length > 0 ? kinds : undefined })}
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
        <WindowInput value={parseWindow(draftWindow)} onChange={(v) => setDraftWindow(v === undefined ? '' : String(v))} />
        <KindCheckboxes value={draftKinds} onChange={setDraftKinds} />
        <Button variant="secondary" onClick={add} disabled={draft.trim() === ''}>添加</Button>
      </div>
    </div>
  )
}

// 模板分类中文标签。
const TEMPLATE_CATEGORY_LABEL: Record<string, string> = {
  international: '国际',
  chinese: '国内',
  aggregator: '聚合 / 开放模型',
  cloud: '云平台',
}

// 把模板转换为可直接预填表单的供应商配置(api_key 留空,由用户补)。
// 模板不含硬编码模型,模型列表由用户在表单里点「获取模型列表」从服务商 API 拉取。
function templateToSeed(t: ProviderTemplate): ProviderConfig {
  return {
    kind: t.kind,
    name: t.name,
    base_url: t.base_url,
    base_path: t.base_path ?? '',
    api_key: '',
    models: [],
    usage_url: t.usage_url ?? '',
    models_url: t.models_url ?? '',
  }
}

// 供应商模板库:按分类列出内置模板,点击「使用」预填表单(仅需补 api_key)。
function TemplatePickerModal({
  onClose, onPick,
}: {
  onClose: () => void
  onPick: (seed: ProviderConfig) => void
}) {
  const { data, error, loading } = useAsync(() => api.listProviderTemplates())
  const groups = new Map<string, ProviderTemplate[]>()
  for (const t of data ?? []) {
    const cat = TEMPLATE_CATEGORY_LABEL[t.category] ?? t.category
    if (!groups.has(cat)) groups.set(cat, [])
    groups.get(cat)!.push(t)
  }

  return (
    <Modal
      title="供应商模板库"
      onClose={onClose}
      wide
      footer={<Button onClick={onClose}>取消</Button>}
    >
      <p className="form-hint" style={{ marginTop: 0, marginBottom: 14 }}>
        选择一家服务商,自动填入 base_url / 接口格式。补上 API Key 后在表单里点「获取模型列表」从服务商 API 拉取模型,即可接入。
      </p>
      {loading ? <Empty text="加载中…" /> : error ? <ErrorAlert text={error} /> : null}
      {!loading && !error
        ? [...groups.entries()].map(([cat, items]) => (
            <div key={cat} style={{ marginBottom: 16 }}>
              <div className="page-sub" style={{ marginBottom: 6 }}>{cat}</div>
              <div className="template-list">
                {items.map((t) => (
                  <div key={t.name} className="template-item">
                    <div className="template-item-main">
                      <div className="template-item-title">
                        <strong>{t.display_name}</strong>
                        <span className="mono faint">{t.name}</span>
                        <Badge>{t.kind}</Badge>
                        {t.base_path && t.base_path !== '/v1' ? <span className="mono faint">{t.base_path}</span> : null}
                      </div>
                      {t.description ? <div className="form-hint" style={{ marginTop: 0 }}>{t.description}</div> : null}
                      <div className="mono faint" style={{ fontSize: 12 }}>{t.base_url}</div>
                      {t.note ? <div className="form-hint">{t.note}</div> : null}
                    </div>
                    <Button variant="secondary" onClick={() => onPick(templateToSeed(t))}>使用</Button>
                  </div>
                ))}
              </div>
            </div>
          ))
        : null}
    </Modal>
  )
}

export default function Providers() {
  const { data, error, loading, reload } = useAsync(() => api.listProviders())
  const [editing, setEditing] = useState<ProviderConfig | null>(null)
  const [adding, setAdding] = useState(false)
  const [picking, setPicking] = useState(false)
  const [templateSeed, setTemplateSeed] = useState<ProviderConfig | null>(null)
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
        <div className="toolbar">
          <Button onClick={() => setPicking(true)}>
            <IconGrid size={15} />
            模板库
          </Button>
          <Button variant="primary" onClick={() => { setTemplateSeed(null); setAdding(true) }}>
            <IconPlus size={15} />
            新增供应商
          </Button>
        </div>
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

      {picking ? (
        <TemplatePickerModal
          onClose={() => setPicking(false)}
          onPick={(seed) => { setTemplateSeed(seed); setPicking(false); setAdding(true) }}
        />
      ) : null}

      {adding ? (
        <ProviderFormModal
          title={templateSeed ? `接入 ${templateSeed.name}` : '新增供应商'}
          initial={null}
          seed={templateSeed}
          onClose={() => { setAdding(false); setTemplateSeed(null) }}
          onSaved={(msg) => { setNotice(msg); setAdding(false); setTemplateSeed(null); reload() }}
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
