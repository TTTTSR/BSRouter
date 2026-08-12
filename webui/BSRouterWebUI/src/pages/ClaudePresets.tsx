import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { useAsync } from '../lib/useAsync'
import { Button, Empty, ErrorAlert, Field, Input, Modal, Select } from '../components/ui'
import { IconEdit, IconPlus, IconTrash } from '../lib/icons'
import type { AggregateModel, ClaudePresetConfig, GroupConfig, ProviderConfig } from '../lib/types'

// 前端所在源的网关地址;命令/预设 base_url 由所选入口派生。
const origin = window.location.origin

// 从全局模型 id 推导档位显示名:合成 id "{供应商}@{模型}" 取 @ 后半段;
// 聚合模型(无 @ 的裸名)原样保留。再去掉 [..] 上下文后缀。
function deriveName(m: string): string {
  const at = m.indexOf('@')
  const rest = at >= 0 ? m.slice(at + 1) : m
  const j = rest.indexOf('[')
  return j > 0 ? rest.slice(0, j) : rest
}

// 模型下拉:不设置 + 所选入口包含的模型。若当前值不在选项中则前置显示(编辑旧预设兜底)。
function ModelSelect({ value, onChange, options }: {
  value: string
  onChange: (v: string) => void
  options: string[]
}) {
  const opts = value !== '' && !options.includes(value) ? [value, ...options] : options
  return (
    <Select value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">不设置</option>
      {opts.map((m) => <option key={m} value={m}>{m}</option>)}
    </Select>
  )
}

// 档位模型行:实际模型下拉 + 显示名输入放在同一行(span2,复用 .model-row 布局)。
function TierField({ label, hint, model, onModel, name, onName, options }: {
  label: string
  hint?: string
  model: string
  onModel: (v: string) => void
  name: string
  onName: (v: string) => void
  options: string[]
}) {
  return (
    <Field label={label} hint={hint} className="span2">
      <div className="model-row">
        <ModelSelect value={model} onChange={onModel} options={options} />
        <Input value={name} onChange={(e) => onName(e.target.value)} placeholder="显示名(留空自动推导)" />
      </div>
    </Field>
  )
}

// 一条 extra_env 键值对。
interface EnvRow { key: string; value: string }

// KEY=VALUE 行编辑器。
function EnvRows({ rows, onChange }: { rows: EnvRow[]; onChange: (v: EnvRow[]) => void }) {
  const [draftKey, setDraftKey] = useState('')
  const [draftValue, setDraftValue] = useState('')

  function add() {
    const k = draftKey.trim()
    if (k === '' || rows.some((r) => r.key === k)) return
    onChange([...rows, { key: k, value: draftValue }])
    setDraftKey('')
    setDraftValue('')
  }

  return (
    <div className="model-rows">
      {rows.map((r, i) => (
        <div key={i} className="model-row">
          <Input
            value={r.key}
            placeholder="变量名"
            onChange={(e) => onChange(rows.map((x, idx) => (idx === i ? { ...x, key: e.target.value } : x)))}
          />
          <Input
            value={r.value}
            placeholder="值"
            onChange={(e) => onChange(rows.map((x, idx) => (idx === i ? { ...x, value: e.target.value } : x)))}
          />
          <Button variant="ghost" className="icon-btn" title="移除" onClick={() => onChange(rows.filter((_, idx) => idx !== i))}>
            <IconTrash />
          </Button>
        </div>
      ))}
      <div className="model-row">
        <Input value={draftKey} placeholder="新变量名" onChange={(e) => setDraftKey(e.target.value)} />
        <Input
          value={draftValue}
          placeholder="新值"
          onChange={(e) => setDraftValue(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              add()
            }
          }}
        />
        <Button variant="secondary" onClick={add} disabled={draftKey.trim() === ''}>添加</Button>
      </div>
    </div>
  )
}

// 入口选择值:'' = 统一 API;'group:{name}' = 分组;'custom' = 保留原 base_url(编辑旧预设兜底)。
function entryBaseURL(entry: string, groups: GroupConfig[], customBaseUrl: string): string {
  if (entry === '') return `${origin}/api`
  if (entry.startsWith('group:')) {
    const g = groups.find((x) => x.name === entry.slice('group:'.length))
    return g ? `${origin}${g.url}` : ''
  }
  return customBaseUrl
}

function entryModels(entry: string, groups: GroupConfig[], unifiedModels: string[]): string[] {
  if (entry === '') return unifiedModels
  if (entry.startsWith('group:')) {
    const g = groups.find((x) => x.name === entry.slice('group:'.length))
    return g ? g.models : []
  }
  return []
}

function ClaudePresetFormModal({
  title, initial, onClose, onSaved,
}: {
  title: string
  initial: ClaudePresetConfig | null
  onClose: () => void
  onSaved: (msg: string) => void
}) {
  const editing = initial !== null
  const [name, setName] = useState(initial?.name ?? '')
  const [description, setDescription] = useState(initial?.description ?? '')
  const [entry, setEntry] = useState('')
  // 自定义入口兜底:保留旧预设的原 base_url(本表单不再提供手动输入)。
  const customBaseUrl = initial?.base_url ?? ''
  const [groups, setGroups] = useState<GroupConfig[]>([])
  const [providers, setProviders] = useState<ProviderConfig[]>([])
  const [aggregateModels, setAggregateModels] = useState<AggregateModel[]>([])
  // 每个模型字段:状态存模型名(可含旧预设的 [1M] 显式标记,后端同步时会处理)。
  const [model, setModel] = useState(initial?.model ?? '')
  const [subagentModel, setSubagentModel] = useState(initial?.subagent_model ?? '')
  const [smallFastModel, setSmallFastModel] = useState(initial?.small_fast_model ?? '')
  const [fableModel, setFableModel] = useState(initial?.fable_model ?? '')
  const [fableModelName, setFableModelName] = useState(initial?.fable_model_name ?? '')
  const [opusModel, setOpusModel] = useState(initial?.opus_model ?? '')
  const [opusModelName, setOpusModelName] = useState(initial?.opus_model_name ?? '')
  const [sonnetModel, setSonnetModel] = useState(initial?.sonnet_model ?? '')
  const [sonnetModelName, setSonnetModelName] = useState(initial?.sonnet_model_name ?? '')
  const [haikuModel, setHaikuModel] = useState(initial?.haiku_model ?? '')
  const [haikuModelName, setHaikuModelName] = useState(initial?.haiku_model_name ?? '')
  const [disableAutoupdater, setDisableAutoupdater] = useState(initial?.disable_autoupdater ?? false)
  const [extraEnv, setExtraEnv] = useState<EnvRow[]>(() =>
    Object.entries(initial?.extra_env ?? {}).map(([k, v]) => ({ key: k, value: v })))
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  // 加载网关分组与供应商列表(统一模型的来源)。分组/供应商列表是 /manage 管理端点;
  // 模型列表虽已公开,但分组与供应商数据只能从 /manage 取,故统一走 /manage 鉴权。
  // 编辑时按存储的 base_url 反推入口选择(无法匹配时保留原地址兜底)。
  useEffect(() => {
    let cancelled = false
    api.listGroups().then((gs) => {
      if (cancelled) return
      setGroups(gs)
      if (initial) {
        const b = initial.base_url
        if (b === `${origin}/api`) {
          setEntry('')
        } else {
          const g = gs.find((x) => b === `${origin}${x.url}`)
          setEntry(g ? `group:${g.name}` : 'custom')
        }
      }
    }).catch(() => { if (initial) setEntry('custom') })
    api.listProviders().then((ps) => {
      if (cancelled) return
      setProviders(ps)
    }).catch(() => {})
    api.listAggregates().then((ags) => {
      if (cancelled) return
      setAggregateModels(ags)
    }).catch(() => {})
    return () => { cancelled = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const baseUrl = entryBaseURL(entry, groups, customBaseUrl)
  // 统一 API 的模型集 = 各供应商模型拼接全局 id(合成) + 聚合裸名。
  const unifiedModels: string[] = []
  for (const p of providers) {
    for (const m of p.models ?? []) {
      const id = `${p.name}@${m.name}`
      if (!unifiedModels.includes(id)) unifiedModels.push(id)
    }
  }
  const aggregateNames = aggregateModels.map((a) => a.name)
  const providerModels = entry === ''
    ? [...unifiedModels, ...aggregateNames]
    : entryModels(entry, groups, unifiedModels)

  // 选择档位模型时自动推导显示名(合成取 @ 后半段;聚合裸名原样)。
  const pickWithName = (setM: (v: string) => void, setName: (v: string) => void) => (v: string) => {
    setM(v)
    setName(deriveName(v))
  }

  async function submit() {
    setErr('')
    if (!baseUrl) {
      setErr('请选择网关入口')
      return
    }
    setBusy(true)
    const extra: Record<string, string> = {}
    for (const r of extraEnv) {
      const k = r.key.trim()
      if (k && r.value !== '') extra[k] = r.value
    }
    // 不携带 api_key/auth_token:新建使用系统默认 key;编辑时后端保留原密钥。
    // 模型名的上下文窗口后缀([Nk]/[1m])由后端按供应商模型配置在命令/应用本地时
    // 自动派生,此处只存裸模型名。
    const cfg: ClaudePresetConfig = {
      name: name.trim(),
      description: description.trim(),
      base_url: baseUrl,
      model: model.trim(),
      subagent_model: subagentModel.trim(),
      small_fast_model: smallFastModel.trim(),
      fable_model: fableModel.trim(),
      fable_model_name: fableModelName.trim(),
      opus_model: opusModel.trim(),
      opus_model_name: opusModelName.trim(),
      sonnet_model: sonnetModel.trim(),
      sonnet_model_name: sonnetModelName.trim(),
      haiku_model: haikuModel.trim(),
      haiku_model_name: haikuModelName.trim(),
      disable_autoupdater: disableAutoupdater,
      extra_env: Object.keys(extra).length > 0 ? extra : undefined,
    }
    try {
      if (editing) {
        await api.updateClaudePreset(initial.name, cfg)
      } else {
        await api.addClaudePreset(cfg)
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
          <Button variant="primary" onClick={() => void submit()} disabled={busy || name.trim() === '' || !baseUrl}>
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
        <Field label="网关入口" hint={baseUrl ? `命令将使用 ${baseUrl}` : '请选择统一 API 或一个分组'} className="span2">
          <Select value={entry} onChange={(e) => setEntry(e.target.value)}>
            <option value="">统一 API（{origin}/api）</option>
            {groups.map((g) => (
              <option key={g.name} value={`group:${g.name}`}>
                分组 {g.name}（{origin}{g.url}）
              </option>
            ))}
            {editing && entry === 'custom' ? <option value="custom">自定义（{customBaseUrl}）</option> : null}
          </Select>
        </Field>
        <Field label="主模型" hint="ANTHROPIC_MODEL">
          <div className="model-row">
            <ModelSelect value={model} onChange={setModel} options={providerModels} />
          </div>
        </Field>
        <Field label="子代理模型" hint="CLAUDE_CODE_SUBAGENT_MODEL">
          <div className="model-row">
            <ModelSelect value={subagentModel} onChange={setSubagentModel} options={providerModels} />
          </div>
        </Field>
        <Field label="旧版小模型" hint="ANTHROPIC_SMALL_FAST_MODEL" className="span2">
          <div className="model-row">
            <ModelSelect value={smallFastModel} onChange={setSmallFastModel} options={providerModels} />
          </div>
        </Field>
        <TierField label="Fable 档位" hint="ANTHROPIC_DEFAULT_FABLE_MODEL"
          model={fableModel} onModel={pickWithName(setFableModel, setFableModelName)}
          name={fableModelName} onName={setFableModelName} options={providerModels} />
        <TierField label="Opus 档位" hint="ANTHROPIC_DEFAULT_OPUS_MODEL"
          model={opusModel} onModel={pickWithName(setOpusModel, setOpusModelName)}
          name={opusModelName} onName={setOpusModelName} options={providerModels} />
        <TierField label="Sonnet 档位" hint="ANTHROPIC_DEFAULT_SONNET_MODEL"
          model={sonnetModel} onModel={pickWithName(setSonnetModel, setSonnetModelName)}
          name={sonnetModelName} onName={setSonnetModelName} options={providerModels} />
        <TierField label="Haiku 档位" hint="ANTHROPIC_DEFAULT_HAIKU_MODEL"
          model={haikuModel} onModel={pickWithName(setHaikuModel, setHaikuModelName)}
          name={haikuModelName} onName={setHaikuModelName} options={providerModels} />
        <Field label="禁用自动更新" className="span2">
          <label style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <input type="checkbox" checked={disableAutoupdater} onChange={(e) => setDisableAutoupdater(e.target.checked)} />
            <span>禁用自动更新</span>
          </label>
        </Field>
        <Field label="额外环境变量" hint="任意 KEY=VALUE,按需添加" className="span2">
          <EnvRows rows={extraEnv} onChange={setExtraEnv} />
        </Field>
      </div>
      {err ? <div className="error-text">{err}</div> : null}
    </Modal>
  )
}

export default function ClaudePresets() {
  const { data, error, loading, reload } = useAsync(() => api.listClaudePresets())
  const { data: groups } = useAsync(() => api.listGroups())
  // 本地模式:仅当通过本机(127.0.0.1/localhost)访问网关时,才启用"覆盖本地 Claude Code 配置"。
  const { data: localInfo } = useAsync(() => api.checkLocal())
  const isLocal = localInfo?.local ?? false
  // 部署形态:远程/NAT 部署下提醒用户填写出口 IP 与映射端口。
  const { data: networkInfo, reload: reloadNetwork } = useAsync(() => api.networkInfo())
  const [egressHost, setEgressHost] = useState('')
  const [egressPort, setEgressPort] = useState('')
  const [savingEgress, setSavingEgress] = useState(false)
  const [editing, setEditing] = useState<ClaudePresetConfig | null>(null)
  const [adding, setAdding] = useState(false)
  const [deleting, setDeleting] = useState<ClaudePresetConfig | null>(null)
  const [notice, setNotice] = useState('')
  const [busyName, setBusyName] = useState('')

  // 出口表单初始值:networkInfo 加载后回填已保存的 egress。
  useEffect(() => {
    if (networkInfo) {
      setEgressHost(networkInfo.egress_host ?? '')
      setEgressPort(networkInfo.egress_port ?? '')
    }
  }, [networkInfo])

  // 保存出口地址(NAT 部署下命令中的 base_url 才能被替换为公网地址)。
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

  // 把存储的 base_url 反推为友好的入口标签。
  function entryLabel(baseUrl: string): string {
    if (baseUrl === `${origin}/api`) return '统一 API'
    const g = (groups ?? []).find((x) => baseUrl === `${origin}${x.url}`)
    return g ? `分组 ${g.name}` : baseUrl
  }

  // 一键复制:从服务端现取命令(内含真实密钥),不缓存明文密钥。
  async function copyCommand(name: string, kind: 'powershell' | 'bash') {
    setBusyName(name)
    try {
      const cmd = await api.claudePresetCommand(name)
      await navigator.clipboard.writeText(cmd[kind])
      // 远程/NAT 部署未配置出口地址时,后端返回 warning 提醒命令可能无法生效。
      setNotice(cmd.warning
        ? `已复制 ${kind === 'powershell' ? 'PowerShell' : 'Bash'} 命令(${name});${cmd.warning}`
        : `已复制 ${kind === 'powershell' ? 'PowerShell' : 'Bash'} 命令(${name})`)
    } catch (e) {
      setNotice(`复制失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  // 应用本地:把预设覆盖到本机 ~/.claude/settings.json 的 env 块(由后端完成写入)。
  async function applyLocal(name: string) {
    setBusyName(name)
    try {
      const r = await api.applyClaudePresetLocal(name)
      setNotice(`已应用 ${name} 到本地配置: ${r.path}`)
    } catch (e) {
      setNotice(`应用失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusyName('')
    }
  }

  async function confirmDelete() {
    if (!deleting) return
    try {
      await api.deleteClaudePreset(deleting.name)
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
          <div className="page-title">Claude Code 预设</div>
          <div className="page-sub">
            选择网关入口与模型,一键复制 PowerShell / bash 启动命令,实现多终端环境分隔;
            {isLocal
              ? '本机访问,可一键覆盖本地 Claude Code 配置'
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
                <th>入口</th>
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
                  <td className="mono">{entryLabel(p.base_url)}</td>
                  <td className="mono">{p.model || <span className="faint">-</span>}</td>
                  <td className="mono">{p.api_key || p.auth_token || <span className="faint">自动</span>}</td>
                  <td className="actions">
                    <Button variant="ghost" disabled={busyName === p.name} title="复制 PowerShell 启动命令" onClick={() => void copyCommand(p.name, 'powershell')}>
                      PS
                    </Button>
                    <Button variant="ghost" disabled={busyName === p.name} title="复制 Bash 启动命令" onClick={() => void copyCommand(p.name, 'bash')}>
                      Bash
                    </Button>
                    {isLocal ? (
                      <Button variant="ghost" disabled={busyName === p.name} title="覆盖本地 ~/.claude/settings.json 的 env 块" onClick={() => void applyLocal(p.name)}>
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
        <ClaudePresetFormModal
          title="新增预设"
          initial={null}
          onClose={() => setAdding(false)}
          onSaved={(msg) => { setNotice(msg); setAdding(false); reload() }}
        />
      ) : null}

      {editing ? (
        <ClaudePresetFormModal
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
