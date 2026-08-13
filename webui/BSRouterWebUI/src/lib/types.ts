export type Kind = 'anthropic' | 'completion' | 'responses'

// 供应商下的一个模型;kinds 为支持的接口格式列表(可多格式),kind 为旧单格式字段
// (兼容旧配置)。两者皆空表示使用供应商默认接口格式。
// context_window 为上下文窗口(k 为单位,如 128 表示 128k);留空/0 默认 200k。
export interface ModelConfig {
  name: string
  kind?: Kind | ''
  kinds?: Kind[]
  context_window?: number
}

export interface ProviderConfig {
  kind: Kind
  name: string
  base_url: string
  base_path?: string
  api_key: string
  models?: ModelConfig[]
  usage_url?: string
  models_url?: string
  // 故障阻塞配置(供应商编辑表单可改,默认启用):
  // - 限流:错误码默认 429,时长默认 120 分钟(rate_limit_duration_minutes),可经
  //   rate_limit_enabled=false 关闭该分类阻塞;
  // - 余额不足:错误码默认 402,0 = 禁用该分类阻塞。
  rate_limit_status?: number
  rate_limit_enabled?: boolean
  rate_limit_duration_minutes?: number
  insufficient_balance_status?: number
}

// 内置供应商接入模板:base_url/格式/模型列表 URL 已填好,用户只需补 api_key;
// 不携带硬编码模型列表,模型用表单里的「获取模型列表」从服务商 API 拉取。
export interface ProviderTemplate {
  name: string
  display_name: string
  category: 'international' | 'chinese' | 'aggregator' | 'cloud'
  description?: string
  kind: Kind
  base_url: string
  base_path?: string
  models_url?: string
  usage_url?: string
  note?: string
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
  // 上下文窗口(k);供应商模型取配置值,聚合裸名取全部有效成员的最小值,未配置时省略。
  context_window?: number
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
  request_body?: string
  forward_url?: string
  forward_request?: string
  forward_response?: string
  converted_response_body?: string
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

// OpenAI Codex 运行配置预设:直接配置模型列表(最多 7 个,对应原生 id 池大小),
// 每个模型自动分配一个裸原生 slug 显示在 Codex Desktop(桌面渲染层只放行它认识的
// 原生 id)。base_url 可选:留空时命令/应用本地自动派生网关统一 API 入口;也可以
// 显式指向 BSRouter 的 <入口>/v1(codex 会拼接 /responses)。鉴权密钥可选,留空时
// 命令/应用本地自动注入网关默认 key。
export interface CodexPresetConfig {
  name: string
  description?: string
  base_url?: string
  models?: string[]
  api_key?: string
  created_at?: string
}

// Codex 预设对应的一键启动命令(PowerShell / bash)。
export interface CodexPresetCommand {
  name?: string
  powershell: string
  bash: string
  warning?: string
}

// Z.ai zcode 运行配置预设:把 BSRouter 作为自定义供应商覆盖进本地
// ~/.zcode/v2/config.json 的 provider map(保留其余内置/自定义供应商与顶层字段)。
// 预设只配置 api_key 与模型列表:zcode 的模型列表手动配置在 config.json,apply-local
// 把 models 按模型原生接口格式分割为多个供应商(openai/anthropic/responses),全部走
// 网关统一 API 入口。models 留空回退网关全部可路由模型;api_key 可选(留空注入网关
// 默认 key)。
export interface ZcodePresetConfig {
  name: string
  description?: string
  api_key?: string
  models?: string[]
  created_at?: string
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

// 聚合模型:裸名(不含供应商前缀,挂在统一供应商下),成员为拥有该模型的供应商
// (顺序即渠道优先级,故障转移/负载均衡按此流转)。load_balance 为该聚合是否启用轮询。
export interface AggregateModel {
  name: string
  members: string[]
  available: string[]
  load_balance?: boolean
}

// 故障提示:一条已记录的故障(category 为 insufficient_balance/rate_limited/internal/upstream)。
// expires_at 为自动解除时间(仅限流类);空 = 持久阻塞(需手动删除解除)。
export interface Fault {
  id: string
  timestamp: string
  category: string
  message: string
  model?: string
  provider?: string
  status?: number
  upstream_status?: number
  expires_at?: string
}

// 故障列表响应:当前捕捉模式(user/dev)+ 故障列表(最新在前)。
export interface FaultList {
  mode: 'user' | 'dev' | string
  faults: Fault[]
}
