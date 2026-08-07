export type Kind = 'anthropic' | 'completion' | 'responses'

// 供应商下的一个模型;kind 留空表示使用供应商默认接口格式。
export interface ModelConfig {
  name: string
  kind?: Kind | ''
}

export interface ProviderConfig {
  kind: Kind
  name: string
  base_url: string
  api_key: string
  models?: ModelConfig[]
  usage_url?: string
  models_url?: string
}

export interface GroupConfig {
  name: string
  kind: Kind
  url?: string
  models: string[]
}

export interface ModelEntry {
  id: string
  object: string
  owned_by: string
}

export interface ModelList {
  object: string
  data: ModelEntry[]
}

export interface PingResult {
  ok: boolean
  status_code?: number
  latency_ms: number
  error?: string
}

export interface SyncResult {
  provider: string
  models: string[]
  count: number
}

// 受管的下游模型请求 API Key。
export interface APIKeyEntry {
  name: string
  key: string
  created_at: string
}

export interface LogEntry {
  timestamp: string
  request_id?: string
  method: string
  path: string
  status: number
  duration_ms: number
  remote_addr?: string
  user_agent?: string
  request_bytes?: number
  response_bytes?: number
  model?: string
  provider?: string
  kind?: string
  upstream_status?: number
  error?: string
  forward_url?: string
  forward_request?: string
  forward_response?: string
}

// Claude Code 运行配置预设,字段镜像 settings.json env 块(鉴权字段列表/单查返回掩码值)。
export interface ClaudePresetConfig {
  name: string
  description?: string
  base_url: string
  api_key?: string
  auth_token?: string
  model?: string
  subagent_model?: string
  small_fast_model?: string
  fable_model?: string
  fable_model_name?: string
  opus_model?: string
  opus_model_name?: string
  sonnet_model?: string
  sonnet_model_name?: string
  haiku_model?: string
  haiku_model_name?: string
  disable_autoupdater?: boolean
  extra_env?: Record<string, string>
  created_at?: string
}

// 预设对应的一键启动命令(PowerShell / bash)。
export interface ClaudePresetCommand {
  name?: string
  powershell: string
  bash: string
  // 远程/NAT 部署且未配置出口地址时,后端返回的提醒(命令可能无法从远端生效)。
  warning?: string
}

// 网关部署形态与出口地址(管理经 /manage/v1/network)。
export interface NetworkInfo {
  remote: boolean
  mode: 'local' | 'direct' | 'nat'
  direct_public_ip?: string
  egress_host?: string
  egress_port?: string
  advertised_base?: string
}

// 聚合模型:裸名(不含供应商前缀,挂在统一供应商下),成员为拥有该模型的供应商。
export interface AggregateModel {
  name: string
  members: string[]
  available: string[]
}
