import type {
  AggregateModel, APIKeyEntry, ClaudePresetCommand, ClaudePresetConfig, CodexPresetCommand, CodexPresetConfig, FaultList, GroupConfig, Kind, LogEntry, ModelList, NetworkInfo, PingResult, ProviderConfig, ProviderTemplate, SyncResult, ZcodePresetConfig, DshPresetCommand, DshPresetConfig,
} from './types'

const KEY_STORAGE = 'bsrouter.api_key'

export function getAPIKey(): string {
  return sessionStorage.getItem(KEY_STORAGE) ?? ''
}
export function setAPIKey(key: string): void {
  sessionStorage.setItem(KEY_STORAGE, key)
}
export function clearAPIKey(): void {
  sessionStorage.removeItem(KEY_STORAGE)
}

export class APIError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const key = getAPIKey()
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  if (key) headers['Authorization'] = `Bearer ${key}`
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status === 401) {
    // 仅管理端(/manage)的 401 视为网关 key 失效,回到登录页;
    // 下游 /api 的 401 是凭据问题(如仅配置受管 key 的网关),不登出,交由页面展示错误。
    if (path.startsWith('/manage')) {
      clearAPIKey()
      window.dispatchEvent(new Event('bsrouter:unauthorized'))
    }
    throw new APIError(401, 'unauthorized: 请重新接入网关 API Key')
  }
  if (!res.ok) {
    let msg = res.statusText
    try {
      const j = (await res.json()) as { error?: string }
      if (j?.error) msg = j.error
    } catch { /* ignore */ }
    throw new APIError(res.status, msg)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

const KINDS: Kind[] = ['anthropic', 'completion', 'responses']
export const KINDS_LABEL: Record<Kind, string> = {
  anthropic: 'anthropic',
  completion: 'completion',
  responses: 'responses',
}
export { KINDS }

export const api = {
  listProviders: () => request<ProviderConfig[]>('GET', '/manage/v1/providers'),
  listProviderTemplates: () => request<ProviderTemplate[]>('GET', '/manage/v1/provider-templates'),
  addProvider: (c: ProviderConfig) => request<ProviderConfig>('POST', '/manage/v1/providers', c),
  updateProvider: (name: string, c: ProviderConfig) =>
    request<ProviderConfig>('PUT', `/manage/v1/providers/${encodeURIComponent(name)}`, c),
  deleteProvider: (name: string) =>
    request<void>('DELETE', `/manage/v1/providers/${encodeURIComponent(name)}`),
  pingProvider: (name: string) =>
    request<PingResult>('POST', `/manage/v1/providers/${encodeURIComponent(name)}/ping`),
  syncModels: (name: string) =>
    request<SyncResult>('POST', `/manage/v1/providers/${encodeURIComponent(name)}/sync-models`),
  // 更新单个模型的上下文窗口(k;0 = 清空回默认 200k)。
  updateModelContextWindow: (name: string, model: string, contextWindow: number) =>
    request<ProviderConfig>('PUT',
      `/manage/v1/providers/${encodeURIComponent(name)}/models/${encodeURIComponent(model)}`,
      { context_window: contextWindow }),
  providerUsage: (name: string) =>
    request<unknown>(`GET`, `/manage/v1/providers/${encodeURIComponent(name)}/usage`),

  listGroups: () => request<GroupConfig[]>('GET', '/manage/v1/groups'),
  addGroup: (c: GroupConfig) => request<GroupConfig>('POST', '/manage/v1/groups', c),
  updateGroup: (name: string, c: GroupConfig) =>
    request<GroupConfig>('PUT', `/manage/v1/groups/${encodeURIComponent(name)}`, c),
  deleteGroup: (name: string) =>
    request<void>('DELETE', `/manage/v1/groups/${encodeURIComponent(name)}`),

  listModels: () => request<ModelList>('GET', '/manage/v1/models'),
  listAggregates: () => request<AggregateModel[]>('GET', '/manage/v1/aggregates'),
  // members 顺序即渠道优先级;loadBalance 可选(undefined 时不改负载均衡开关)。
  updateAggregate: (name: string, members: string[], loadBalance?: boolean) =>
    request<{ name: string; members: string[] }>('PUT', `/manage/v1/aggregates/${encodeURIComponent(name)}`,
      loadBalance === undefined ? { members } : { members, load_balance: loadBalance }),
  listLogs: (limit = 200) => request<LogEntry[]>('GET', `/manage/v1/logs?limit=${limit}`),
  logFile: () => request<{ path: string }>('GET', '/manage/v1/logs/file'),
  logDetail: () => request<{ detail: 'default' | 'full' }>('GET', '/manage/v1/logs/detail'),
  setLogDetail: (detail: 'default' | 'full') =>
    request<{ detail: 'default' | 'full' }>('PUT', '/manage/v1/logs/detail', { detail }),
  fetchModels: (input: { name?: string; kind: Kind; base_url: string; api_key: string; models_url: string }) =>
    request<{ models: string[]; count: number }>('POST', '/manage/v1/fetch-models', input),

  listKeys: () => request<APIKeyEntry[]>('GET', '/manage/v1/keys'),
  generateKey: (name: string) => request<APIKeyEntry>('POST', '/manage/v1/keys', { name }),
  deleteKey: (name: string) => request<void>('DELETE', `/manage/v1/keys/${encodeURIComponent(name)}`),

  listClaudePresets: () => request<ClaudePresetConfig[]>('GET', '/manage/v1/claude-presets'),
  addClaudePreset: (c: ClaudePresetConfig) => request<ClaudePresetConfig>('POST', '/manage/v1/claude-presets', c),
  updateClaudePreset: (name: string, c: ClaudePresetConfig) =>
    request<ClaudePresetConfig>('PUT', `/manage/v1/claude-presets/${encodeURIComponent(name)}`, c),
  deleteClaudePreset: (name: string) =>
    request<void>('DELETE', `/manage/v1/claude-presets/${encodeURIComponent(name)}`),
  claudePresetCommand: (name: string) =>
    request<ClaudePresetCommand>('GET', `/manage/v1/claude-presets/${encodeURIComponent(name)}/command`),
  applyClaudePresetLocal: (name: string) =>
    request<{ applied: boolean; path: string }>('POST', `/manage/v1/claude-presets/${encodeURIComponent(name)}/apply-local`),

  listCodexPresets: () => request<CodexPresetConfig[]>('GET', '/manage/v1/codex-presets'),
  addCodexPreset: (c: CodexPresetConfig) => request<CodexPresetConfig>('POST', '/manage/v1/codex-presets', c),
  updateCodexPreset: (name: string, c: CodexPresetConfig) =>
    request<CodexPresetConfig>('PUT', `/manage/v1/codex-presets/${encodeURIComponent(name)}`, c),
  deleteCodexPreset: (name: string) =>
    request<void>('DELETE', `/manage/v1/codex-presets/${encodeURIComponent(name)}`),
  codexPresetCommand: (name: string) =>
    request<CodexPresetCommand>('GET', `/manage/v1/codex-presets/${encodeURIComponent(name)}/command`),
  applyCodexPresetLocal: (name: string) =>
    request<{ applied: boolean; path: string; auth_path?: string; model_catalog?: string }>('POST', `/manage/v1/codex-presets/${encodeURIComponent(name)}/apply-local`),

  listZcodePresets: () => request<ZcodePresetConfig[]>('GET', '/manage/v1/zcode-presets'),
  addZcodePreset: (c: ZcodePresetConfig) => request<ZcodePresetConfig>('POST', '/manage/v1/zcode-presets', c),
  updateZcodePreset: (name: string, c: ZcodePresetConfig) =>
    request<ZcodePresetConfig>('PUT', `/manage/v1/zcode-presets/${encodeURIComponent(name)}`, c),
  deleteZcodePreset: (name: string) =>
    request<void>('DELETE', `/manage/v1/zcode-presets/${encodeURIComponent(name)}`),
  applyZcodePresetLocal: (name: string) =>
    request<{ applied: boolean; path: string; models?: number; providers?: number }>('POST', `/manage/v1/zcode-presets/${encodeURIComponent(name)}/apply-local`),

  listDshPresets: () => request<DshPresetConfig[]>('GET', '/manage/v1/dsh-presets'),
  addDshPreset: (c: DshPresetConfig) => request<DshPresetConfig>('POST', '/manage/v1/dsh-presets', c),
  updateDshPreset: (name: string, c: DshPresetConfig) =>
    request<DshPresetConfig>('PUT', `/manage/v1/dsh-presets/${encodeURIComponent(name)}`, c),
  deleteDshPreset: (name: string) =>
    request<void>('DELETE', `/manage/v1/dsh-presets/${encodeURIComponent(name)}`),
  dshPresetCommand: (name: string) =>
    request<DshPresetCommand>('GET', `/manage/v1/dsh-presets/${encodeURIComponent(name)}/command`),
  applyDshPresetLocal: (name: string) =>
    request<{ applied: boolean; path: string; provider?: string; api?: string; api_key_env?: string; models?: number }>('POST', `/manage/v1/dsh-presets/${encodeURIComponent(name)}/apply-local`),

  checkLocal: () => request<{ local: boolean }>('GET', '/manage/v1/local'),

  networkInfo: () => request<NetworkInfo>('GET', '/manage/v1/network'),
  setNetworkInfo: (c: { egress_host: string; egress_port: string }) =>
    request<NetworkInfo>('PUT', '/manage/v1/network', c),

  listFaults: () => request<FaultList>('GET', '/manage/v1/faults'),
  deleteFault: (id: string) =>
    request<void>('DELETE', `/manage/v1/faults/${encodeURIComponent(id)}`),
}
