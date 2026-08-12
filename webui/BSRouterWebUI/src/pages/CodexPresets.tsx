import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Button, Empty, ErrorAlert, Field, Input, Modal, Select } from '../components/ui'
import { IconEdit, IconPlus, IconTrash, IconX } from '../lib/icons'
import type { CodexPresetConfig } from '../lib/types'

// Codex Desktop 只显示它原生 allowlist 里的裸原生 id,原生 id 池大小为 8
// (见 GET /manage/v1/codex-native-slugs),故预设直接配置的模型最多 8 个。
const MAX_MODELS = 8

function CodexPresetFormModal({
  title, initial, onClose, onSaved,
}: {
  title: string
  initial: CodexPresetConfig | null
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

  // 加载网关全部可路由模型(供模型列表选择器)。
  useEffect(() => {
    let cancelled = false
    api.listModels().then((ml) => {
      if (cancelled) return
      setAllModels(ml.data.map((m) => m.id))
    }).catch(() => {})
    return () => { cancelled = true }
  }, [])

  function addModel() {
    const m = pick.trim()
    if (!m || models.includes(m)) return
    if (models.length >= MAX_MODELS) {
      setErr(`最多选择 ${MAX_MODELS} 个模型(对应原生 id 池大小,超出无法在 Codex Desktop 显示)`)
      return
    }
    setModels([...models, m])
    setPick('')
    setErr('')
  }

  function removeModel(m: string) {
    setModels(models.filter((x) => x !== m))
  }

  async function submit() {
    setErr('')
    setBusy(true)
    // 直接配置模型列表;base_url 留空由网关派生统一 API 入口。api_key 可选:填写
    // 则用自定义密钥,留空走系统默认 key(新建)/保留原密钥(编辑)。
    const cfg: CodexPresetConfig = {
      name: name.trim(),
      description: description.trim(),
      models,
      api_key: apiKey.trim() || undefined,
    }
    try {
      if (editing) {
        await api.updateCodexPreset(initial.name, cfg)
      } else {
        await api.addCodexPreset(cfg)
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
      wide
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
          label={`模型列表(最多 ${MAX_MODELS} 个)`}
          hint="每个模型自动分配一个原生 slug 显示在 Codex Desktop;留空回退网关全部模型(自动分配前 7 个)"
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

export default function CodexPresets() {
  const { data, error, loading, reload } = useAsync(() => api.listCodexPresets())
  // 本地模式:仅当通过本机访问网关时,才启用"覆盖本地 Codex 配置"。
  const { data: localInfo } = useAsync(() => api.checkLocal())
  const isLocal = localInfo?.local ?? false
  // 部署形态:远程/NAT 部署下提醒用户填写出口 IP 与映射端口。
  const { data: networkInfo, reload: reloadNetwork } = useAsync(() => api.networkInfo())
  const [egressHost, setEgressHost] = useState('')
  const [egressPort, setEgressPort] = useState('')
  const [savingEgress, setSavingEgress] = useState(false)
  const [editing, setEditing] = useState<CodexPresetConfig | null>(null)
  const [adding, setAdding] = useState(false)
  const [deleting, setDeleting] = useState<CodexPresetConfig | null>(null)
  const [notice, setNotice] = useState('')
  const [busyName, setBusyName] = useState('')

  useEffect(() => {
    if (networkInfo) {
      setEgressHost(networkInfo.egress_host ?? '')
      setEgressPort(networkInfo.egress_port ?? '')
    }
  }, [networkInfo])

  async function saveEgress() {
    setSavingEgress(true)
    try {
      await api.setNetworkInfo({ egress_host: egressHost.trim(), egress_port: egressPort.trim() })
      await reloadNetwork()
      setNotice('出口地址已保存')
    } catch (e) {
      setNotice(`保存失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setSavingEgress(false)
    }
  }

  // 一键复制:从服务端现取命令(内含真实密钥),不缓存明文密钥。
  async function copyCommand(name: string, kind: 'powershell' | 'bash') {
    setBusyName(name)
    try {
      const cmd = await api.codexPresetCommand(name)
      await navigator.clipboard.writeText(cmd[kind])
      setNotice(cmd.warning
        ? `已复制 ${kind === 'powershell' ? 'PowerShell' : 'Bash'} 命令(${name});${cmd.warning}`
        : `已复制 ${kind === 'powershell' ? 'PowerShell' : 'Bash'} 命令(${name})`)
    } catch (e) {
      setNotice(`复制失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  // 应用本地:把预设覆盖到本机 ~/.codex/config.toml + auth.json(跳过登录)+ 模型目录。
  async function applyLocal(name: string) {
    setBusyName(name)
    try {
      const r = await api.applyCodexPresetLocal(name)
      setNotice(
        `已应用 ${name}:密钥写入 auth.json(codex 无需登录),模型列表已同步(${r.model_catalog ?? '网关模型目录'})`
      )
    } catch (e) {
      setNotice(`应用失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  async function confirmDelete() {
    if (!deleting) return
    try {
      await api.deleteCodexPreset(deleting.name)
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
          <div className="page-title">Codex 预设</div>
          <div className="page-sub">
            预设直接配置模型列表(最多 {MAX_MODELS} 个),每个自动分配原生 slug 显示在 Codex Desktop;
            {isLocal
              ? '本机访问,可一键覆盖本地 Codex 配置(config.toml + auth.json 跳过登录 + 模型目录)'
              : <span className="faint">当前非本机访问,「覆盖本地配置」不可用</span>}
          </div>
        </div>
        <Button variant="primary" onClick={() => setAdding(true)}>
          <IconPlus size={15} />
          新增预设
        </Button>
      </div>

      {notice ? <div className="alert">{notice}</div> : null}

      {/* 远程/NAT 部署提醒:未配置出口地址时醒目提醒填写,配置后展示广告地址。 */}
      {networkInfo?.remote && !networkInfo.advertised_base ? (
        <div className="net-warn">
          <div className="net-warn-title">检测到远程 / NAT 部署:尚未配置出口地址</div>
          <div className="net-warn-body">
            当前复制的启动命令里 base_url 仍是本机地址(127.0.0.1),在远端机器上运行无法连上网关。
            请填写网关的出口 IP 与映射后的公网端口(由 NAT / 安全组配置决定)。
          </div>
          <div className="net-warn-form">
            <Input
              value={egressHost}
              onChange={(e) => setEgressHost(e.target.value)}
              placeholder="出口 IP / 域名,如 1.2.3.4"
            />
            <Input
              value={egressPort}
              onChange={(e) => setEgressPort(e.target.value)}
              placeholder="映射端口,如 443"
              style={{ width: 110 }}
            />
            <Button variant="primary" onClick={() => void saveEgress()} disabled={savingEgress || egressHost.trim() === ''}>
              {savingEgress ? '保存中…' : '保存出口地址'}
            </Button>
          </div>
        </div>
      ) : networkInfo?.remote && networkInfo.advertised_base ? (
        <div className="alert">
          远程部署:启动命令中的 base_url 将使用 <span className="mono">{networkInfo.advertised_base}</span>
          {networkInfo.egress_host ? ';出口地址由上方配置' : ''}
        </div>
      ) : null}

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
                <th>模型(自动分配原生 slug)</th>
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
                      : <span className="faint">全部模型(自动分配前 {MAX_MODELS} 个)</span>}
                  </td>
                  <td className="mono">{p.api_key || <span className="faint">自动</span>}</td>
                  <td className="actions">
                    <Button variant="ghost" disabled={busyName === p.name} title="复制 PowerShell 启动命令" onClick={() => void copyCommand(p.name, 'powershell')}>
                      PS
                    </Button>
                    <Button variant="ghost" disabled={busyName === p.name} title="复制 Bash 启动命令" onClick={() => void copyCommand(p.name, 'bash')}>
                      Bash
                    </Button>
                    {isLocal ? (
                      <Button variant="ghost" disabled={busyName === p.name} title="覆盖本地 ~/.codex/config.toml + auth.json" onClick={() => void applyLocal(p.name)}>
                        应用本地
                      </Button>
                    ) : null}
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
        <CodexPresetFormModal
          title="新增预设"
          initial={null}
          onClose={() => setAdding(false)}
          onSaved={(msg) => { setNotice(msg); setAdding(false); reload() }}
        />
      ) : null}

      {editing ? (
        <CodexPresetFormModal
          title={`编辑预设 ${editing.name}`}
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
