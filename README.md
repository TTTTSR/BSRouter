# BSRouter

> **Language / 语言**: [中文](#中文) · [English](#english)

---

## 中文

大模型网关（LLM Gateway）——为 **agent 使用者**提供统一接入入口，连接来自不同供应商的大模型服务（OpenAI、Anthropic、Azure OpenAI、国内厂商等），屏蔽各供应商 API 的差异。

- **三种接口格式互通**：Anthropic Messages / OpenAI chat.completions / responses，请求响应格式由客户端决定，上游任意；**流式输出同样互通**（跨格式经规范化事件中转，格式匹配时直通透传）。
- **直通 + 转换双路径**：模型支持客户端格式时请求原样转发（仅改写模型名，避免转换损失），否则经规范化中间层转换。
- **聚合模型与故障转移**：自动聚合同名模型，按渠道优先级故障转移，失败成员冷却 10 分钟；**聚合上下文窗口取全部成员的最小值**（未配置按默认 200k 参与、故障禁用的成员不参与），保证路由到任一成员都不超窗。
- **故障提示与阻塞**：上游返回特定错误（如余额不足/限流）时记录故障并在「故障提示」页陈列、可逐条删除；该供应商的模型随之被**禁用**（聚合跳过、单独请求返回 503 阻塞原因）。**余额不足（默认 402）持久阻塞**直到手动删除故障；**限流（默认 429、启用、120 分钟）到期自动解除**（也可手动删除提前解除，适配 codingplan 等 5 小时限额提供商）；限流的**错误码 / 开关 / 时长**与余额不足错误码均可按供应商自定义（前端编辑表单）。故障在**成员级失败时即时记录**——即使聚合的兜底成员成功，报错的成员也会被立即记录并阻塞。
- **内置 Web 界面**：黑白灰扁平管理 UI，管理供应商、模型分组、聚合、日志、故障、API Key、Claude Code / Codex / zcode 预设。
- **单二进制分发**：前端经 `go:embed` 内嵌，编译后一个可执行文件即可运行。
- **零外部依赖**：纯 Go 标准库实现。

### 一键安装（无需编译）

下载对应平台的网关二进制与 `bsr` 命令行，自动放入 PATH：

```bash
# Linux / macOS(无需 root,装到 ~/.local/bin;新终端生效或按提示粘贴一行 export)
curl -fsSL https://raw.githubusercontent.com/TTTTSR/BSRouter/main/scripts/install.sh | sh

# Linux 想当前终端立即生效:装到系统 PATH 目录 /usr/local/bin(需 sudo,注意是 | sudo sh)
curl -fsSL https://raw.githubusercontent.com/TTTTSR/BSRouter/main/scripts/install.sh | sudo sh

# Windows（PowerShell,当前窗口立即生效）
irm https://raw.githubusercontent.com/TTTTSR/BSRouter/main/scripts/install.ps1 | iex
```

安装后 `bsr` 位于 `~/.local/bin`（Linux/macOS）或 `%LOCALAPPDATA%\BSRouter\bin`（Windows）。`bsr start` 后台启动网关，`bsr stop` / `restart` / `status` / `log` 管理进程。需要编译或自定义配置时见「快速开始」。

### 快速开始

#### 前置

- Go 1.25+
- Node.js（仅构建前端时需要）

#### 构建

前端与后端分别编译。后端经 `go:embed` 内嵌前端构建产物 `dist/`，因此**必须先构建前端再编译 Go**：

```bash
# 1) 构建前端(go:embed 依赖 dist,若缺失 go build 会失败)
cd webui/BSRouterWebUI && npm install && npm run build

# 2) 编译 Go(内嵌前端,在仓库根目录)
cd ../.. && go build ./...
```

#### 运行

```bash
# 启动网关(默认监听 127.0.0.1:18154,配置文件默认存 OS 用户配置目录,见下方「配置文件位置」)
go run ./cmd/gateway

# 自动生成随机 API Key 并打印
go run ./cmd/gateway -private

# 显式指定 API Key
go run ./cmd/gateway -api-key <key>
```

启动后访问 `http://127.0.0.1:18154/` 打开管理界面，输入网关 API Key 登录（未配置 key 时网关不鉴权，适合纯本地使用）。

#### 启动参数

`go run ./cmd/gateway`（或编译后的二进制）支持的完整参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:18154` | HTTP 监听地址。**默认仅绑定本机回环**，避免管理/转发端点直接暴露到局域网；需局域网或公网访问时改为 `:18154` 或 `0.0.0.0:18154`（部署形态与广告地址见「远程部署」） |
| `-private` | — | 自动生成随机 API Key（64 位 hex）并打印，用于登录/调用 |
| `-api-key <key>` | `GATEWAY_API_KEY` 环境变量 | 显式指定网关 API Key；不加 `-private`/`-api-key` 时网关**不鉴权** |
| `-config <path>` | 配置目录 `providers.json` | 供应商配置（相对当前运行目录解析；任一配置参数显式传路径即覆盖默认并跳过运行目录迁移） |
| `-groups <path>` | 配置目录 `groups.json` | 模型分组配置，传空串禁用 |
| `-keys <path>` | 配置目录 `keys.json` | 受管 API Key 配置，传空串禁用 |
| `-claude <path>` | 配置目录 `claude.json` | Claude Code 配置预设，传空串禁用 |
| `-codex <path>` | 配置目录 `codex.json` | Codex 配置预设，传空串禁用 |
| `-aggregates <path>` | 配置目录 `aggregates.json` | 聚合模型配置，传空串禁用 |
| `-network <path>` | 配置目录 `network.json` | 出口地址配置（NAT 部署下用户填写的出口 IP 与映射端口），传空串禁用 |
| `-log <path>` | 配置目录 `gateway-<时间戳>.log.jsonl` | 请求日志（JSONL），传空串禁用；显式传路径继续追加不截断 |
| `-log-detail <default\|full>` | `default` | 日志完整度：`default` 仅出错时记录完整转发详情，`full` 全部记录 |
| `-log-detail-file <path>` | 配置目录 `logdetail.json` | 日志完整度持久化文件，传空串禁用 |
| `-public-addr <URL>` | — | 手工覆盖对外广告地址（如 `https://gw.example.com` 或 `http://1.2.3.4:443`），最高优先，NAT/反向代理下自动检测不到时使用 |
| `-claude-settings <path>` | `~/.claude/settings.json` | 「应用本地」覆盖 Claude Code 配置的目标路径 |
| `-codex-config <path>` | `~/.codex/config.toml` | 「应用本地」覆盖 Codex config.toml 的目标路径 |
| `-codex-auth <path>` | `~/.codex/auth.json` | 「应用本地」覆盖 Codex auth.json 的目标路径 |
| `-codex-model-catalog <path>` | `~/.codex/bsrouter-models.json` | 「应用本地」覆盖 Codex 模型目录的目标路径 |
| `-codex-models-cache <path>` | `~/.codex/models_cache.json` | 「应用本地」覆盖 Codex 模型缓存的目标路径 |
| `-zcode <path>` | 配置目录 `zcode.json` | zcode 配置预设，传空串禁用 |
| `-zcode-config <path>` | `~/.zcode/v2/config.json` | 「应用本地」覆盖 zcode config.json 的目标路径 |
| `-stream-idle-timeout <duration>` | `0`（禁用） | 上游流式 idle 超时：两字节数据到达间隔超过该值即中止流（思考模型可能长时间无增量，启用建议 ≥120s） |
| `-stream-retries <n>` | `2` | 流开始前失败（5xx/传输错误）每成员重试次数，`0` 关闭 |
| `-faults <path>` | 配置目录 `faults.json` | 故障提示记录文件，传空串禁用 |
| `-fault-mode <user\|dev>` | `user` | 故障捕捉模式：`user` 仅捕捉硬编码特定故障（当前仅余额不足），`dev` 捕捉所有错误（内部 + 上游）；前端不提供切换 |
| `-version` | — | 打印版本并退出 |

> 一键安装的 `bsr` 命令行同样支持透传以上参数，如 `bsr start -addr :9000 -api-key sk-...`。

#### 常用命令

```bash
go build ./...          # 编译(内嵌前端)
go vet ./...            # 静态检查
go test ./... -v        # 全部测试
go test ./... -race     # 竞态检测
```

### 配置文件位置

全部配置（`providers.json` / `groups.json` / `keys.json` / `claude.json` / `codex.json` / `zcode.json` / `aggregates.json` / `network.json` / `logdetail.json` / `faults.json`）与请求日志默认存 **OS 用户配置目录**（跨平台惯例，Go `os.UserConfigDir()`）：

```
Windows: %APPDATA%\BSRouter\
macOS:   ~/Library/Application Support/BSRouter/
Linux:   ~/.config/BSRouter/
```

首次以默认路径启动时，会自动把运行目录下已有的同名配置（providers / groups / keys / claude / codex / zcode / aggregates）**迁移**到该目录（源文件保留，不覆盖已存在目标）。任一配置 flag 显式传路径即覆盖默认（相对当前运行目录解析，适合便携/分发）。请求日志默认以**启动时间戳命名**独立文件（`gateway-<时间戳>.log.jsonl`，每次运行一份），不做迁移。

### 配置

#### 供应商（`providers.json`）

```json
[
  {"kind": "completion", "name": "openai", "base_url": "https://api.openai.com",
   "api_key": "sk-...", "models": [{"name": "gpt-4o"}, {"name": "claude-x", "kinds": ["anthropic"], "context_window": 128}],
   "usage_url": "https://api.openai.com/usage", "models_url": ""}
]
```

- `kind`：供应商**默认**接口格式（`anthropic` / `completion` / `responses`）。
- `models`：模型列表，每个模型可单独用 `kinds`（数组）声明一个或多个支持的接口格式（旧配置可用单值 `kind`，`kinds` 优先），**留空则用供应商默认**。同一供应商的不同模型可拥有不同格式。
- `context_window`：该模型上下文窗口（**k 为单位**，如 `128` 表示 128k；留空/`0` 默认 200k）。Claude Code / Codex 预设据此生成模型名后缀与目录条目窗口。
- `base_path`（可选）：`base_url` 与端点之间的路径段，**留空回退 `/v1`**；`"/"` 表示根路径（端点直接拼到 `base_url` 后）。用于智谱 `/api/paas/v4`、百度千帆 `/v2`、火山方舟 `/api/v3`、Google Gemini `…/v1beta/openai` 等非 `/v1` 厂商。三种格式端点实际请求 `{base_url}{base_path}/chat/completions|messages|responses`，模型列表默认 `{base_url}{base_path}/models`。
- `api_key` 以明文存储；`usage_url`/`models_url` 可选且必须 http(s)，`models_url` 默认 `{base}{base_path}/models`。
- 故障阻塞覆盖（可选，前端编辑表单可配）：`rate_limit_status`（限流错误码，默认 429）、`rate_limit_enabled`（限流阻塞开关，默认启用）、`rate_limit_duration_minutes`（限流阻塞时长分钟，默认 120）、`insufficient_balance_status`（余额不足错误码，默认 402）。除 `rate_limit_enabled` 外：缺省 = 用默认；`0` = 禁用该分类阻塞；其余 400-599 = 该状态码触发对应阻塞（如 5 小时限额提供商 codingplan 用 429 触发限流阻塞并自定义时长）。

> **内置模板库**：前端「供应商管理」页有「模板库」按钮，内置约 25 家主流大模型服务商（OpenAI、Anthropic、Gemini、xAI、Mistral、DeepSeek、Kimi、GLM、千问、文心、豆包、混元、MiniMax、硅基流动、阶跃、百川、OpenRouter、Groq、Together 等）的 base_url / 接口格式 / 模型列表 URL，**不携带硬编码模型列表**（避免模板模型过期或与实际账户权限不符）——补上 api_key 后在表单点「获取模型列表」从服务商 API 实时拉取，即可接入。数据经 `internal/providertemplates/templates.json` 内嵌，接口为 `GET /manage/v1/provider-templates`。**模板较多、无法逐一实测**：各服务商的接口细节（路径、鉴权头、模型列表格式等）可能有出入，接入某家遇到问题请反馈（提 issue 或联系维护者），我们会逐个修正模板。

#### 模型路由

请求体中的模型名：

- `{供应商名}@{模型名}` 合成 id —— 网关按首个 `@` 切分路由到对应供应商，响应模型回填为请求时的完整模型名。
- **聚合裸名**（无 `@`）—— 自动聚合同名模型，按成员**渠道优先级**流转做**故障转移**：成员出错时切换到其余成员，全部失败才返回错误；有成员成功时，失败成员在该聚合下**冷却禁用 10 分钟**（仅内存，重启清除）。**负载均衡为每聚合独立开关、默认关闭**——关闭时按优先级固定流转不轮询，开启时轮询打散请求。聚合在模型列表的 `context_window` 取全部有效成员的最小值（未配置按默认 200k 参与、故障禁用的成员不参与），保证路由到任一成员都不超窗。
- **直通**：模型支持请求的接口格式时，请求/响应**原样转发**（仅改写 model 字段），不经中间层转换；否则经规范化中间类型转换。

#### 模型分组（`groups.json`）

分组是面向下游的**虚拟供应商**，挂载到自己的 `/api` 下 URL：

```json
[
  {"name": "team-a", "kind": "completion", "url": "/api/team-a",
   "models": ["openai@gpt-4o", "anthropic@claude-sonnet-4-5"]}
]
```

客户端以分组的接口格式调用，路由到组内模型对应的真实上游（支持**跨格式**）。`url` 默认 `/api/{name}`，必须位于 `/api` 下且不使用保留的 `/api/v1` 段。

#### 受管 API Key（`keys.json`）

为下游模型请求生成/删除 key（`sk-` + 64 位 a-zA-Z0-9），`/api` 转发端点鉴权额外接受；`/manage` 仍只认网关 key。管理界面中的 Key 展示做**防窥掩码**：仅显示前 5 位与后 5 位（中间以 `****` 隐藏，短 Key 整体隐藏），完整内容不可查看，但每行「复制」按钮仍可复制完整 Key。

#### Claude Code 配置预设（`claude.json`）

预设若干 Claude Code 运行配置（镜像 settings.json 的 `env` 块：`base_url`、`api_key` 与 `auth_token` 二选一、主/子代理/小模型、Fable/Opus/Sonnet/Haiku 四档、`disable_autoupdater`、`extra_env`），一键生成 PowerShell / bash 启动命令（末尾启动 `claude`，并清理未用的鉴权变量），或一键覆盖本地 `~/.claude/settings.json` 的 `env` 块（仅本机访问时可用，保留其余字段）。预设不填密钥时自动注入系统默认 key——优先复用/生成受管 key `claude-default`，未启用受管 key 时回退网关 key。**上下文窗口后缀**：命令与覆盖本地会按各模型字段在供应商模型配置里的 `context_window` 自动追加 `[128k]`/`[1m]` 后缀，让 Claude Code 的上下文预算与上游真实窗口一致。

#### OpenAI Codex 配置预设（`codex.json`）

预设绑定虚拟运营商（统一 API / 分组，`codex` 仅支持 responses），预设直接配置模型列表（`{供应商}@{模型}` 合成 + 聚合裸名，最多 8 个）。一键复制 `codex -c` 覆盖启动命令（**不写任何文件**，首行设密钥环境变量后逐参数覆盖完整定义 provider），或覆盖本地 **~/.codex 四件套**（仅本机访问时可用）：

1. `~/.codex/config.toml` —— 替换/追加单一 `[model_providers.bsrouter]` 块（base_url / wire_api / requires_openai_auth，不含密钥）+ 顶层 `model_provider="bsrouter"`、`model_catalog_json`，行级定向编辑保留其余内容；
2. `~/.codex/auth.json` —— 把密钥写入 `OPENAI_API_KEY` 字段（保留其余字段），**codex 只需 auth.json 有 key 即跳过 ChatGPT 登录**；
3. `~/.codex/bsrouter-models.json` —— 生成 codex **模型目录**（TUI 的模型列表来源）；
4. `~/.codex/models_cache.json` —— 生成 codex **模型缓存**（**桌面 App 的模型列表来源**，`client_version` 自动探测本地 `codex --version`，版本不匹配时桌面端会拒绝缓存）。

**native alias（Codex Desktop 显示自定义模型的关键）**：部分 Codex Desktop 版本在 app-server 加载模型目录后，再用远程 `available_models` allowlist 过滤选择器（上游 [openai/codex#19694](https://github.com/openai/codex/issues/19694)），只保留它认识的**裸原生 OpenAI id**，普通 `{供应商}@{模型}` 路由 id 会被全部剔除。网关为每个配置模型**自动分配**一条裸原生 slug（模型列表排序后依次占用原生 id 池：`gpt-5.6-sol / -terra / -luna / gpt-5.5 / gpt-5.4 / gpt-5.4-mini / gpt-5.3-codex / gpt-5.2`，池用尽即停；slug 与模型对应关系无关紧要，只是通过 allowlist 的「护照」），目录条目 `display_name` 用真实模型 id（桌面显示诚实标签），`context_window` 同步模型配置（未配置默认 200k）。请求 `model=<原生slug>` 时网关在聚合/供应商解析**之前**解析到绑定模型，响应模型回填仍为请求时的 slug（直通/转换/流式/分组路径均生效；只捕获裸 slug，`{供应商}@{模型}` 等普通路由不受影响）。密钥未配置时自动注入系统默认 key。

#### Z.ai zcode 配置预设（`zcode.json`）

zcode（Z.ai 的 opencode 派生编码智能体，桌面 App + CLI）的模型列表**手动配置**在本地 `~/.zcode/v2/config.json`（不自动获取）。预设只配置**模型列表**（`models`，`{供应商}@{模型}` 合成 + 聚合裸名，留空回退网关全部可路由模型）与可选 `api_key`（留空自动注入系统默认 key）。「应用本地」（仅本机访问）把网关覆盖进 config.json 的 `provider` map（保留其余内置/自定义供应商），并**按模型原生接口格式自动分割为多个供应商**：

- completion 模型 → `bsrouter-openai`（`openai-compatible`，`…/api/v1`，zcode 拼 `/chat/completions`）；
- anthropic 模型 → `bsrouter-anthropic`（`anthropic`，`…/api` **不带 `/v1`**，zcode 拼 `/v1/messages`）；
- responses 模型 → `bsrouter-responses`（`openai-compatible` + `wire_api=responses`，`…/api/v1`）。

全部走网关统一 API 入口，让每种格式的模型都走**原生 wire 连接**（不经网关转换）、在 zcode 里便携切换多供应商；每条模型写 `limit.context`（按上下文窗口换算 tokens）。`-zcode-config` 可自定义覆盖目标路径；zcode 无命令端点，apply-local 即覆盖机制。

### 流式输出

`stream:true` 全格式互通，两个路径：

- **格式匹配时直通**：上游 SSE 逐事件透传，仅改写 model 字段（`RewriteSSEModel`）。
- **跨格式时经规范化事件中转**：各格式实现一对 解码器（格式 SSE → 规范化 `StreamEvent`）与编码器（规范化 → 格式 SSE），覆盖 message_start / content_start / content_delta / content_stop / message_delta / message_stop / error。thinking/reasoning 内容在流式经 `content_delta`(thinking) 中转，非流式在 `Message.Reasoning` 上跨格式双向透传（completion `reasoning_content` ↔ responses `reasoning` item ↔ anthropic thinking 扩展块），思考启用参数（`thinking:{budget_tokens}` / `reasoning_effort` / `reasoning:{effort}`）归一化为统一档位并映射回目标格式。

聚合模型流式在**流开始前**同样可故障转移（上游非 2xx / 传输失败）；SSE 头已发出后无法切换（固有边界）。

### API

统一 API（客户端将 base_url 设为 `http://<host>/api`，再调用标准路径）：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/messages` | Anthropic 格式 |
| POST | `/api/v1/chat/completions` | chat.completions 格式 |
| POST | `/api/v1/responses` | responses 格式 |
| GET  | `/api/v1/models` | 聚合所有供应商模型（公开，无需鉴权，返回模型 ID 与 `context_window`(k，未配置省略)） |

管理接口位于 `/manage/v1/*`（受网关 Key 鉴权）：

- **供应商**：增删改查、连通性探测（ping）、同步模型（sync-models）、用量查询（usage）、`fetch-models` 从未注册供应商拉取模型供表单自动填充、`PUT /providers/{name}/models/{model}` 更新单个模型上下文窗口、`GET /provider-templates` 内置供应商模板库。
- **模型分组 / 聚合**：分组虚拟供应商 CRUD；`GET/PUT /aggregates` 查看/设置聚合成员（成员顺序即渠道优先级）与每聚合负载均衡开关。
- **受管 Key**：`/keys` 生成/列表/删除。
- **Claude / Codex / zcode 预设**：CRUD + Claude/Codex `/{name}/command` 一键启动命令（嵌入真实密钥）+ `/{name}/apply-local` 覆盖本地配置（**仅本机访问，否则 403**；zcode 无命令端点，apply-local 即覆盖机制）；`GET /codex-native-slugs` 返回原生 slug 池。
- **日志**：`GET /logs` 最近日志（默认只含 `/api` 转发日志，`?scope=all` 含管理接口）、`GET /logs/file` 当前日志文件路径、`GET/PUT /logs/detail` 日志完整度开关。
- **部署**：`GET/PUT /network` 部署形态与出口地址；`GET /local` 是否本机访问。

模型列表端点（`GET /api/v1/models`、`GET /manage/v1/models` 与各分组 `GET {分组URL}/v1/models`）公开无需鉴权，便于下游免 key 发现可用模型。

### 请求日志

所有 API 请求以 **JSONL** 记录到本地文件（权限 0600）。默认每次运行以启动时间戳命名独立文件（`gateway-<时间戳>.log.jsonl`）；显式 `-log <path>` 尊重路径继续追加（不截断），传空串禁用。字段含 `timestamp` / `request_id` / `method` / `path` / `status` / `duration_ms` / `remote_addr` / `user_agent` / `request_bytes` / `response_bytes` / `model` / `provider` / `kind` / `upstream_status` / `error`，以及转发详情（`forward_url` / `forward_request` / `forward_response`，转换路径另有 `request_body` / `converted_response_body`）。写入前均**抹除 api_key**（含 URL 编码形态，仅替换 ≥8 字符的 key），并按 rune 边界截断至 256 KB；鉴权头不记录。

**日志完整度分级**（`-log-detail default|full`，默认 `default`；管理界面 `GET/PUT /manage/v1/logs/detail` 运行时切换，持久化到 `logdetail.json`）：`default` 仅网关内部出错或供应商返回错误（`status>=400`）才记录完整转发详情；`full` 所有请求都记录。

### 远程部署

网关部署在服务器上时，Claude/Codex 预设若指向 `127.0.0.1`，远端客户端会连到自己回环而失败。网关按 **`-addr` 绑定地址 + 网卡检测**判定部署形态（仅枚举本机接口，**零外部请求**）：

- **local**：绑定回环（默认 `127.0.0.1:18154`）→ 本地部署，不广告、不提醒。
- **direct**：绑定全部接口（`:18154` / `0.0.0.0`）且网卡有**直连公网 IPv4**（或显式绑定公网 IP）→ 自动广告 `http://<公网IP>:<监听端口>`。
- **nat**：绑定非回环但网卡无公网 IP（云 NAT / 路由器后）→ 不做公网出口自动探测（异端口映射无法从网关内部探测），由用户填写出口地址。

出口地址优先级：**`-public-addr <完整URL>`**（如 `https://gw.example.com`，手工权威覆盖，最高优先）> **UI 填写的出口 IP 与映射端口**（NAT 部署下前端醒目横幅引导填写）> **direct 自动广告**。命令端点对**指向回环**的预设 base_url 动态替换为广告地址（不改存储）；NAT 且未配置出口时命令带 `warning` 提醒。

### 内置 Web 界面

`http://127.0.0.1:18154/`，黑白灰扁平管理 UI（无阴影、无 emoji，图标用内联 SVG，控件仅少量圆角），登录后管理全部配置。八个页面：

- **供应商管理**（`Providers.tsx`）：供应商增/改/删、连通性测试、同步模型、用量查询；表单含模型按行编辑（模型名 + 上下文窗口(k) + 支持的格式多选）与「获取模型列表」自动填充。
- **模型管理**（`Models.tsx`）：已接入模型列表（行内编辑上下文窗口）、**聚合模型**（成员 tag 可拖拽调整渠道优先级、剔除/添加回成员、每聚合负载均衡开关）、模型分组 CRUD。
- **日志查看**（`Logs.tsx`）：最近日志（点击行展开转发详情）、5 秒自动刷新、日志完整度下拉。
- **故障提示**（`Faults.tsx`）：展示记录的故障（时间 / 类型 / 故障内容 / 模型 / 供应商 / 状态 / 自动解除时间，最新在前），页头显示当前捕捉模式，每条可删除；限流故障显示自动解除时间（2 小时后）。
- **API Key**（`ApiKeys.tsx`）：生成/列表/删除受管 key（新生成 key 完整展示一次）。
- **Claude 预设**（`ClaudePresets.tsx`）：预设 CRUD + 每行「复制 PS / 复制 Bash」一键启动命令 + 「应用本地」覆盖 `~/.claude/settings.json`（仅本机显示）。
- **Codex 预设**（`CodexPresets.tsx`）：预设 CRUD + 模型列表（最多 8 个）+ 命令复制 + 「应用本地」覆盖 ~/.codex 四件套（仅本机显示）。
- **zcode 预设**（`ZcodePresets.tsx`）：zcode 预设 CRUD（模型列表，留空回退网关全部）+ 「应用本地」覆盖 `~/.zcode/v2/config.json`（按模型原生格式分割为多供应商，仅本机显示）。

远程（non-local）部署且未配置出口地址时，Claude / Codex 预设页顶部显示醒目横幅引导填写出口 IP 与映射端口。

### 鉴权

- 网关 key 优先级：**`-private` > `-api-key` > 不鉴权**。
  - `-private`：网关**自动生成**随机 API Key（64 位 hex）并打印到启动日志，用它登录/调用。
  - `-api-key <key>`（或 `GATEWAY_API_KEY` 环境变量）：使用显式 key。
  - 两者都不给：网关**不鉴权**（所有端点开放，启动时打印警告；适合纯本地使用）。
- **受管 API Key**（`-keys keys.json`）专供下游模型请求：`/api` 转发端点额外接受受管 key；`/manage` 仍只认网关 key。无受管 key 且无网关 key 时 `/api` 保持开放。
- **SPA 静态资源（登录页/JS/CSS）始终可访问**；`/api` 与 `/manage` 在启用鉴权时强制要求 Key，**模型列表端点公开例外**。
- 客户端通过 `Authorization: Bearer <key>`（OpenAI 风格）或 `x-api-key: <key>`（Anthropic 风格）携带 key；缺失/错误返回 401 + `WWW-Authenticate`。比较采用 SHA-256 摘要 + `ConstantTimeCompare`（不泄露密钥长度/存在性）。鉴权在请求体解码**之前**执行。

### 架构

```
internal/gateway/   # 接入层:规范化中间类型 + 三种格式适配器(请求/响应/流式双向转换 + 直通)
internal/provider/  # 供应商:Config(含模型级多接口格式 kinds + context_window + base_path)、按模型 Kind 派发、Manager(JSON 持久化)
internal/providertemplates/ # 内置供应商模板库(go:embed templates.json,不含硬编码模型,前端「模板库」一键接入)
internal/group/     # 模型分组:虚拟供应商 + Manager(URL 冲突校验)
internal/aggregate/ # 聚合模型:成员从供应商派生 + 剔除名单/渠道优先级/负载均衡开关持久化 + 故障转移冷却(仅内存)+ 上下文窗口取成员最小值
internal/apikey/    # 受管 API Key:生成/查询/删除 + JSON 持久化
internal/claude/    # Claude Code 配置预设:Config(镜像 env 块)+ Manager + 命令生成(PS/bash)+ 覆盖本地 settings.json
internal/codex/     # OpenAI Codex 配置预设:Config + Manager + -c 命令生成 + TOML 合并(apply-local)+ native alias 模型目录
internal/zcode/     # Z.ai zcode 配置预设:Config(模型列表 + api_key)+ Manager + 覆盖本地 config.json(按模型原生格式分割多供应商)
internal/network/   # 部署形态检测(仅网卡,零外部请求)+ 出口地址(出口IP+映射端口)JSON 持久化
internal/server/    # 网关 HTTP 服务:转发 + 管理 + 探测 + 分组 + 聚合 + 日志 + 受管 key + 预设 + 部署 + SPA 挂载
internal/logger/    # JSONL 请求日志(写 + 最近 N 条读取 + 完整度分级 + 密钥抹除/截断)
webui/              # 内嵌前端:webui.go(go:embed BSRouterWebUI/dist) + BSRouterWebUI(Vite+React+TS)
cmd/gateway/        # 可运行入口
```

核心设计是**规范化中间类型（canonical）**：三种格式各自实现一对「格式 ↔ 规范化」转换，请求与响应两侧**对称**；流式输出同样经规范化事件互通（跨格式也可），格式匹配时直通透传。另有关键机制：

- **直通**：模型支持请求的接口格式时，请求/响应原样转发（仅改写 model 字段），不经中间层转换。
- **故障转移**：聚合模型按渠道优先级依序尝试成员，成员出错切换到下一个，全部失败返回最后一次错误；有成员成功时失败成员冷却 10 分钟（仅内存）。
- **thinking/reasoning 双向透传**：deepseek 等 thinking 上游的 `reasoning_content` / reasoning item / thinking 块跨格式保留并回传，否则上游会以 400 拒绝多轮对话。
- **孤儿 tool_use 兜底 / 非 function 工具跳过 / 多 tool_result 拆分**：保证转发请求对严格上游始终合法。

### 开发

前端与后端分别构建：

```bash
# 前端构建(产生 dist,供 go:embed 内嵌)
cd webui/BSRouterWebUI && npm run build

# Go 编译 / 测试 / 静态检查(在仓库根目录)
go build ./...
go test ./... -v
go vet ./...
```

> 前端构建产物 `dist/` 与本地工具文件已在 `.gitignore` 中忽略，不入库。

### 许可证

本项目基于 [MIT License](LICENSE) 开源。

```
MIT License

Copyright (c) 2026 TTTTSR

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

---

## English

BSRouter is a Large Language Model (LLM) Gateway that provides a unified access point for **agent users**, connecting models from different providers (OpenAI, Anthropic, Azure OpenAI, domestic vendors, etc.) while shielding clients from each provider's API differences.

- **Three wire formats interoperate**: Anthropic Messages / OpenAI chat.completions / responses. The request/response format is decided by the client; the upstream can be anything. **Streaming interoperates too** — cross-format via canonical events, same-format via direct passthrough.
- **Passthrough + conversion dual path**: when a model natively supports the client's format, the request is forwarded verbatim (only the model field is rewritten, avoiding conversion loss); otherwise it goes through the canonical conversion layer.
- **Aggregate models & failover**: same-named models from different providers auto-aggregate into bare-name aggregates, fail over by channel priority, and failed members get a 10-minute cooldown. **The aggregate's context window is the minimum across its members** (unset members count as the default 200k; fault-blocked members are excluded), so routing to any member never overflows the window.
- **Fault alerts & blocking**: when the upstream returns a specific error (e.g. insufficient balance / rate limit), the gateway records a fault shown on the "Faults" page (each row deletable), and that provider's models become **blocked** (aggregates skip it, standalone requests return 503 with the blocking reason). **Insufficient balance (default 402) blocks persistently** until the fault is manually deleted; **rate limit (default 429, enabled, 120 min) auto-unblocks when the duration elapses** (also manually deletable — fits 5-hour-limit providers like codingplan); the rate-limit **status code / enabled switch / duration** and the insufficient-balance status code are all per-provider configurable in the edit form. Faults are recorded **per member at failure time** — even if a fallback member later succeeds, the failing provider is recorded and blocked immediately.
- **Built-in web UI**: black-white-gray flat management UI for providers, model groups, aggregates, logs, faults, API keys, and Claude Code / Codex / zcode presets.
- **Single-binary distribution**: the frontend is embedded via `go:embed`; one compiled executable runs everything.
- **Zero external dependencies**: pure Go standard library.

### One-Click Install (no compilation needed)

Downloads the gateway binary and the `bsr` CLI for your platform and adds them to PATH:

```bash
# Linux / macOS (no root; installs to ~/.local/bin; new terminals, or paste the
# one-line export printed at the end)
curl -fsSL https://raw.githubusercontent.com/TTTTSR/BSRouter/main/scripts/install.sh | sh

# Linux, usable in the CURRENT terminal right away: installs to the system
# PATH dir /usr/local/bin (requires sudo - note: it's "| sudo sh")
curl -fsSL https://raw.githubusercontent.com/TTTTSR/BSRouter/main/scripts/install.sh | sudo sh

# Windows (PowerShell; usable in the current window immediately)
irm https://raw.githubusercontent.com/TTTTSR/BSRouter/main/scripts/install.ps1 | iex
```

After install, `bsr` lives in `~/.local/bin` (Linux/macOS) or `%LOCALAPPDATA%\BSRouter\bin` (Windows). `bsr start` runs the gateway in the background; `bsr stop` / `restart` / `status` / `log` manage it. See "Quick Start" to build from source or customize.

### Quick Start

#### Prerequisites

- Go 1.25+
- Node.js (only needed to build the frontend)

#### Build

The frontend and backend are compiled separately. The backend embeds the frontend's `dist/` output via `go:embed`, so **the frontend must be built before compiling Go**:

```bash
# 1) Build the frontend (go:embed depends on dist; without it, go build fails)
cd webui/BSRouterWebUI && npm install && npm run build

# 2) Compile Go (frontend embedded, run from the repo root)
cd ../.. && go build ./...
```

#### Run

```bash
# Start the gateway (default listens on 127.0.0.1:18154; config files default to the
# OS user config directory — see "Config File Locations" below)
go run ./cmd/gateway

# Generate a random API key for auth and print it
go run ./cmd/gateway -private

# Use an explicit API key
go run ./cmd/gateway -api-key <key>
```

After startup, open `http://127.0.0.1:18154/` to use the management UI and log in with the gateway API key (with no key configured, the gateway runs unauthenticated — suitable for purely local use).

#### Startup Flags

Complete set of flags for `go run ./cmd/gateway` (or the compiled binary):

| Flag | Default | Description |
|---|---|---|
| `-addr` | `127.0.0.1:18154` | HTTP listen address. **By default binds loopback only**, so the management and forwarding endpoints are not exposed to the LAN; use `:18154` or `0.0.0.0:18154` for LAN/public access (deployment mode and advertised base, see "Remote Deployment") |
| `-private` | — | Auto-generate a random API key (64 hex chars) and print it for login/API calls |
| `-api-key <key>` | `GATEWAY_API_KEY` env var | Use an explicit gateway API key; without `-private`/`-api-key` the gateway runs **unauthenticated** |
| `-config <path>` | `providers.json` in config dir | Provider config (resolved relative to the working directory; passing any config flag explicitly overrides the default and skips run-dir migration) |
| `-groups <path>` | `groups.json` in config dir | Model group config; empty string disables |
| `-keys <path>` | `keys.json` in config dir | Managed API key config; empty string disables |
| `-claude <path>` | `claude.json` in config dir | Claude Code preset config; empty string disables |
| `-codex <path>` | `codex.json` in config dir | Codex preset config; empty string disables |
| `-aggregates <path>` | `aggregates.json` in config dir | Aggregate model config; empty string disables |
| `-network <path>` | `network.json` in config dir | Egress address config (egress IP + mapped port filled in on NAT deployments); empty string disables |
| `-log <path>` | `gateway-<timestamp>.log.jsonl` in config dir | Request log (JSONL); empty string disables; an explicit path appends without truncation |
| `-log-detail <default\|full>` | `default` | Log detail level: `default` records full forward detail only on errors, `full` for every request |
| `-log-detail-file <path>` | `logdetail.json` in config dir | Log-detail-level persistence file; empty string disables |
| `-public-addr <URL>` | — | Manually override the advertised base URL (e.g. `https://gw.example.com` or `http://1.2.3.4:443`); highest priority, for NAT/reverse-proxy deployments that auto-detection cannot find |
| `-claude-settings <path>` | `~/.claude/settings.json` | Target path for "apply-local" to override Claude Code config |
| `-codex-config <path>` | `~/.codex/config.toml` | Target path for "apply-local" to override Codex config.toml |
| `-codex-auth <path>` | `~/.codex/auth.json` | Target path for "apply-local" to override Codex auth.json |
| `-codex-model-catalog <path>` | `~/.codex/bsrouter-models.json` | Target path for "apply-local" to override the Codex model catalog |
| `-codex-models-cache <path>` | `~/.codex/models_cache.json` | Target path for "apply-local" to override the Codex models cache |
| `-zcode <path>` | `zcode.json` in config dir | zcode preset config; empty string disables |
| `-zcode-config <path>` | `~/.zcode/v2/config.json` | Target path for "apply-local" to override the local zcode config.json |
| `-stream-idle-timeout <duration>` | `0` (disabled) | Upstream stream idle timeout: aborts the stream when no data arrives for this long (thinking models may emit nothing for a while; ≥120s recommended when enabled) |
| `-stream-retries <n>` | `2` | Retries per member for pre-stream failures (5xx / transport errors); `0` disables |
| `-faults <path>` | `faults.json` in config dir | Fault-record file; empty string disables |
| `-fault-mode <user\|dev>` | `user` | Fault capture mode: `user` records only hardcoded faults (currently just insufficient balance), `dev` records all errors (internal + upstream); not toggleable from the UI |
| `-version` | — | Print version and exit |

> The `bsr` CLI from the one-click install forwards these flags too, e.g. `bsr start -addr :9000 -api-key sk-...`.

#### Common Commands

```bash
go build ./...          # compile (frontend embedded)
go vet ./...            # static analysis
go test ./... -v        # run all tests
go test ./... -race     # race detector
```

### Config File Locations

All config files (`providers.json` / `groups.json` / `keys.json` / `claude.json` / `codex.json` / `zcode.json` / `aggregates.json` / `network.json` / `logdetail.json`) and the request log default to the **OS user config directory** (platform convention, via Go's `os.UserConfigDir()`):

```
Windows: %APPDATA%\BSRouter\
macOS:   ~/Library/Application Support/BSRouter/
Linux:   ~/.config/BSRouter/
```

On first startup with the default paths, existing same-named config files in the run directory (providers / groups / keys / claude / codex / zcode / aggregates) are **migrated** into that directory (the source file is kept, and an existing target is never overwritten). Passing any config flag explicitly overrides the default (resolved relative to the current working directory — good for portable/distributed setups). The request log defaults to an independent file named by **startup timestamp** (`gateway-<timestamp>.log.jsonl`, one per run) and is not migrated.

### Configuration

#### Providers (`providers.json`)

```json
[
  {"kind": "completion", "name": "openai", "base_url": "https://api.openai.com",
   "api_key": "sk-...", "models": [{"name": "gpt-4o"}, {"name": "claude-x", "kinds": ["anthropic"], "context_window": 128}],
   "usage_url": "https://api.openai.com/usage", "models_url": ""}
]
```

- `kind`: the provider's **default** wire format (`anthropic` / `completion` / `responses`).
- `models`: the model list. Each model can declare one or more supported formats with a `kinds` array (legacy configs may use a single `kind`; `kinds` wins), and **an empty list falls back to the provider default**. Different models of the same provider can have different formats.
- `context_window`: the model's context window (**in units of k**, e.g. `128` means 128k; empty/`0` defaults to 200k). Claude Code / Codex presets use this to derive model-name suffixes and catalog entry windows.
- `api_key` is stored in plaintext; `usage_url` / `models_url` are optional and must be http(s); `models_url` defaults to `{base}/v1/models`.
- Fault-blocking overrides (optional, editable in the provider form): `rate_limit_status` (rate-limit code, default 429), `rate_limit_enabled` (rate-limit block switch, default on), `rate_limit_duration_minutes` (rate-limit block duration in minutes, default 120), `insufficient_balance_status` (insufficient-balance code, default 402). Except `rate_limit_enabled`: absent = default; `0` = disable that block category; any other 400-599 = that status triggers the corresponding block (e.g. 5-hour-limit providers like codingplan use 429 for rate-limit blocking and can set a custom duration).

> **Built-in template library**: the "Providers" page has a "Template Library" button with ~25 mainstream providers (OpenAI, Anthropic, Gemini, xAI, Mistral, DeepSeek, Kimi, GLM, Qwen, Ernie, Doubao, Hunyuan, MiniMax, SiliconFlow, StepFun, Baichuan, OpenRouter, Groq, Together, etc.), pre-filling base_url / wire format / models URL. Templates carry **no hardcoded model list** (so they never go stale or mismatch your account's access) — fill in the API key, then click "Fetch models" in the form to pull the list live from the provider's API. Data is embedded via `internal/providertemplates/templates.json`; endpoint `GET /manage/v1/provider-templates`. **With so many templates, not every one can be fully tested**: per-provider interface details (paths, auth headers, model-list format, etc.) may differ — if connecting a certain provider fails, please report it (open an issue or contact the maintainers) and we will fix that template.

#### Model Routing

The model name in a request body:

- **Composite id** `{provider}@{model}` — the gateway splits on the first `@` and routes to the corresponding provider; the response model is backfilled to the full name from the request.
- **Bare aggregate name** (no `@`) — same-named models auto-aggregate and flow by member **channel priority** with **failover**: when a member errors, the gateway switches to the next; the error is returned only if all members fail. When another member succeeds, the failed member is **cooldown-banned for 10 minutes** within that aggregate (memory-only, cleared on restart). **Load balancing is a per-aggregate switch, off by default** — when off, the fixed priority order is used without rotation; when on, requests are spread by round-robin. The aggregate's `context_window` in the model list is the minimum across its effective members (unset members count as the default 200k; fault-blocked members are excluded), so routing to any member never overflows the window.
- **Passthrough**: when a model supports the request's wire format, the request/response is forwarded **verbatim** (only the `model` field is rewritten), bypassing the conversion layer; otherwise it is converted through the canonical intermediate types.

#### Model Groups (`groups.json`)

A group is a **virtual provider** for downstream clients, mounted at its own URL under `/api`:

```json
[
  {"name": "team-a", "kind": "completion", "url": "/api/team-a",
   "models": ["openai@gpt-4o", "anthropic@claude-sonnet-4-5"]}
]
```

Clients call the group's wire format and it routes to the real upstream of each assigned model (cross-format is supported). `url` defaults to `/api/{name}`, must live under `/api`, and must not use the reserved `/api/v1` segment.

#### Managed API Keys (`keys.json`)

Generate/delete keys (`sk-` + 64 chars of a-zA-Z0-9) for downstream model requests. The `/api` forwarding endpoints additionally accept these keys; `/manage` still only accepts the gateway key. Keys are displayed with **privacy masking** in the management UI: only the first 5 and last 5 characters are shown (the middle hidden as `****`; short keys are hidden entirely). Full content is never viewable, but a per-row "Copy" button still copies the complete key.

#### Claude Code Presets (`claude.json`)

Presets mirror the `env` block of Claude Code's `settings.json` (`base_url`; `api_key` XOR `auth_token`; main/subagent/small-fast models; Fable/Opus/Sonnet/Haiku tiers; `disable_autoupdater`; `extra_env`). They generate one-click PowerShell / bash launch commands (launching `claude` at the end and cleaning up unused auth variables), or overwrite the `env` block of the local `~/.claude/settings.json` (available only when accessed from the local machine; all other fields are preserved). When a preset has no key, the system default key is injected automatically — preferring to reuse/lazily create the managed key `claude-default`, falling back to the gateway key when managed keys are not enabled. **Context-window suffix**: for both `/command` and apply-local, the `[128k]`/`[1m]` suffix is appended to each model field based on the `context_window` in the provider's model config, keeping Claude Code's context budget aligned with the upstream's real window.

#### OpenAI Codex Presets (`codex.json`)

Presets bind to a virtual provider (the unified API or a group; `codex` only supports responses) and directly configure a model list (`{provider}@{model}` composites + bare aggregate names, at most 8). One-click copy of a `codex -c` override launch command (**writes no files**; sets the key env var on the first line, then defines the provider completely via per-parameter overrides), or overwrite the local **~/.codex four-file set** (available only when accessed from the local machine):

1. `~/.codex/config.toml` — replace/append the single `[model_providers.bsrouter]` block (base_url / wire_api / requires_openai_auth, **no key**) plus the top-level `model_provider="bsrouter"` and `model_catalog_json`; line-level targeted editing preserves everything else;
2. `~/.codex/auth.json` — writes the key into the `OPENAI_API_KEY` field (preserving other fields). **Codex skips the ChatGPT login as long as auth.json has a key**;
3. `~/.codex/bsrouter-models.json` — generates the codex **model catalog** (the TUI's model-list source);
4. `~/.codex/models_cache.json` — generates the codex **models cache** (**the Desktop app's model-list source**; `client_version` is auto-detected from the local `codex --version`, and the Desktop app rejects the cache on a version mismatch).

**Native alias (the key to showing custom models in Codex Desktop)**: some Codex Desktop versions filter the model picker against a remote `available_models` allowlist after the app-server loads the model catalog ([openai/codex#19694](https://github.com/openai/codex/issues/19694)), keeping only the **bare native OpenAI ids** it recognizes and dropping ordinary `{provider}@{model}` routing ids. The gateway **auto-assigns** each configured model a bare native slug (the sorted model list occupies the native id pool in order: `gpt-5.6-sol / -terra / -luna / gpt-5.5 / gpt-5.4 / gpt-5.4-mini / gpt-5.3-codex / gpt-5.2`, stopping when the pool is exhausted; the slug↔model correspondence is irrelevant — it is just a "passport" through the allowlist). Catalog entries use the real model id as `display_name` (honest labels on Desktop) and sync `context_window` from the model config (default 200k when unset). When a request sends `model=<native slug>`, the gateway resolves it to the bound model **before** aggregate/provider resolution, and the response model is backfilled with the requested slug (works across passthrough / conversion / streaming / group paths; only bare slugs are captured — ordinary routes like `{provider}@{model}` are unaffected). Keys are auto-injected with the system default when a preset has none.

#### Z.ai zcode Presets (`zcode.json`)

zcode (Z.ai's opencode-derived coding agent, desktop app + CLI) keeps its model list **manually configured** in the local `~/.zcode/v2/config.json` (not auto-fetched). A preset configures only a **model list** (`models`: `{provider}@{model}` composites + bare aggregate names; empty falls back to all routable gateway models) and an optional `api_key` (empty auto-injects the system default key). "Apply to local" (local access only) overlays the gateway into the config.json `provider` map (preserving other built-in/custom providers) and **auto-splits the models by native wire format into multiple providers**:

- completion models → `bsrouter-openai` (`openai-compatible`, `…/api/v1`, zcode appends `/chat/completions`);
- anthropic models → `bsrouter-anthropic` (`anthropic`, `…/api` **without `/v1`**, zcode appends `/v1/messages`);
- responses models → `bsrouter-responses` (`openai-compatible` + `wire_api=responses`, `…/api/v1`).

All requests go through the gateway's unified API, so every format connects over its **native wire** (no gateway conversion) and you can switch between the multiple providers in zcode's UI; each model entry carries `limit.context` (derived from its context window in tokens). `-zcode-config` customizes the override target; zcode has no command endpoint — apply-local is the override mechanism.

### Streaming

`stream:true` interoperates across all formats via two paths:

- **Same-format passthrough**: the upstream SSE stream is forwarded event-by-event with only the `model` field rewritten (`RewriteSSEModel`).
- **Cross-format via canonical events**: each format has a decoder (format SSE → canonical `StreamEvent`) and encoder (canonical → format SSE), covering message_start / content_start / content_delta / content_stop / message_delta / message_stop / error. Thinking/reasoning content streams through `content_delta`(thinking) and, non-streaming, round-trips bidirectionally on `Message.Reasoning` (completion `reasoning_content` ↔ responses `reasoning` item ↔ anthropic thinking extension block). Reasoning-enable parameters (`thinking:{budget_tokens}` / `reasoning_effort` / `reasoning:{effort}`) are normalized to one unified effort level and mapped back to the target format.

Aggregate-model streaming can also fail over **before the stream starts** (upstream non-2xx / transport failure); once SSE headers have been sent, switching is impossible (an inherent boundary).

### API

Unified API (clients set `base_url` to `http://<host>/api`, then call the standard paths):

| Method | Path | Description |
|---|---|---|
| POST | `/api/v1/messages` | Anthropic format |
| POST | `/api/v1/chat/completions` | chat.completions format |
| POST | `/api/v1/responses` | responses format |
| GET  | `/api/v1/models` | All providers' models (public, no auth; returns model IDs and `context_window` in k, omitted when unset) |

The management API lives under `/manage/v1/*` (gated by the gateway key):

- **Providers**: CRUD, connectivity probe (ping), sync-models, usage query (usage), `fetch-models` to pull models from an unregistered provider for form autofill, and `PUT /providers/{name}/models/{model}` to update a single model's context window.
- **Groups / aggregates**: group virtual-provider CRUD; `GET/PUT /aggregates` to view/set aggregate members (member order is channel priority) and the per-aggregate load-balancing switch.
- **Managed keys**: `/keys` create / list / delete.
- **Claude / Codex / zcode presets**: CRUD + Claude/Codex `/{name}/command` one-click launch command (embeds the real key) + `/{name}/apply-local` to overwrite local config (**local access only, 403 otherwise**; zcode has no command endpoint — apply-local is the override mechanism); `GET /codex-native-slugs` returns the native slug pool.
- **Logs**: `GET /logs` recent entries (defaults to `/api` forwarding logs only; `?scope=all` includes management endpoints), `GET /logs/file` the current log file path, `GET/PUT /logs/detail` the log-detail-level switch.
- **Deployment**: `GET/PUT /network` deployment mode and egress address; `GET /local` whether the request is from the local machine.

The model-list endpoints (`GET /api/v1/models`, `GET /manage/v1/models`, and each group's `GET {groupURL}/v1/models`) are public with no auth, letting downstream clients discover available models without a key.

### Request Logs

Every API request is recorded as **JSONL** to a local file (mode 0600). By default each run gets an independent file named by startup timestamp (`gateway-<timestamp>.log.jsonl`); an explicit `-log <path>` appends to that path (never truncated) and an empty string disables logging. Fields include `timestamp` / `request_id` / `method` / `path` / `status` / `duration_ms` / `remote_addr` / `user_agent` / `request_bytes` / `response_bytes` / `model` / `provider` / `kind` / `upstream_status` / `error`, plus forward details (`forward_url` / `forward_request` / `forward_response`, and `request_body` / `converted_response_body` on the conversion path). Before writing, `api_key` is **redacted** (including URL-encoded forms; only keys of ≥8 chars are replaced) and bodies are truncated to 256 KB at rune boundaries; auth headers are never logged.

**Log detail level** (`-log-detail default|full`, default `default`; switchable at runtime via `GET/PUT /manage/v1/logs/detail`, persisted to `logdetail.json`): `default` records full forward detail only when the gateway errors internally or the provider returns an error (`status>=400`); `full` records it for every request.

### Remote Deployment

When the gateway runs on a server, Claude/Codex presets pointing at `127.0.0.1` would make remote clients connect to their own loopback and fail. The gateway determines the deployment mode from **`-addr` + a NIC scan** (only local interfaces are enumerated — **zero external requests**):

- **local**: bound to loopback (default `127.0.0.1:18154`) → local deployment; nothing is advertised, no warning.
- **direct**: bound to all interfaces (`:18154` / `0.0.0.0`) with a **direct public IPv4** on a NIC (or an explicit public-IP bind) → auto-advertises `http://<publicIP>:<listenPort>`.
- **nat**: bound to non-loopback but with no public IP on the NICs (cloud NAT / behind a router) → no egress auto-probing (a mapped external port cannot be detected from inside the gateway); the user fills in the egress address.

Egress-address priority: **`-public-addr <full URL>`** (e.g. `https://gw.example.com`; manual authoritative override, highest priority) > **egress IP + mapped port filled in the UI** (a prominent banner guides this on NAT deployments) > **direct auto-advertise**. Command endpoints dynamically replace loopback-pointing preset `base_url`s with the advertised base (storage untouched); on NAT without egress config, commands carry a `warning`.

### Built-in Web UI

`http://127.0.0.1:18154/` — a black-white-gray flat management UI (no shadows, no emoji, inline SVG icons, slight corner radius on controls). After login you can manage everything. Eight pages:

- **Providers** (`Providers.tsx`): provider add/edit/delete, connectivity test, sync-models, usage query; the form edits models row-by-row (model name + context window(k) + supported formats) and offers "fetch models" autofill.
- **Models** (`Models.tsx`): the connected-model list (inline context-window editing), **aggregate models** (drag-and-drop member tags for channel priority, remove/re-add members, per-aggregate load-balancing switch), and group CRUD.
- **Logs** (`Logs.tsx`): recent logs (click a row to expand forward details), 5-second auto-refresh, log-detail-level dropdown.
- **Faults** (`Faults.tsx`): recorded faults (time / category / message / model / provider / status / auto-unblock time, newest first), current capture mode in the header, each row deletable; rate-limit faults show their 2-hour auto-unblock time.
- **API Keys** (`ApiKeys.tsx`): create/list/delete managed keys (a newly generated key is shown in full exactly once).
- **Claude Presets** (`ClaudePresets.tsx`): preset CRUD + per-row "Copy PS / Copy Bash" one-click launch commands + "Apply to local" to overwrite `~/.claude/settings.json` (shown only when local).
- **Codex Presets** (`CodexPresets.tsx`): preset CRUD + model list (max 8) + command copy + "Apply to local" to overwrite the ~/.codex four-file set (shown only when local).
- **zcode Presets** (`ZcodePresets.tsx`): zcode preset CRUD (model list; empty falls back to all gateway models) + "Apply to local" to overwrite `~/.zcode/v2/config.json` (auto-splits into multiple providers by native format; shown only when local).

On remote (non-local) deployments without an egress address, the Claude / Codex preset pages show a prominent banner at the top guiding the egress IP and mapped port.

### Authentication

- Gateway key priority: **`-private` > `-api-key` > unauthenticated**.
  - `-private`: the gateway **auto-generates** a random API key (64 hex chars) and prints it at startup; use it to log in / call the API.
  - `-api-key <key>` (or the `GATEWAY_API_KEY` env var): use an explicit key.
  - Neither: the gateway runs **unauthenticated** (all endpoints open; a warning is printed at startup; suitable for purely local use).
- **Managed API keys** (`-keys keys.json`) are for downstream model requests: `/api` forwarding endpoints additionally accept managed keys; `/manage` still only accepts the gateway key. With neither managed keys nor a gateway key, `/api` stays open.
- **SPA static assets (login page/JS/CSS) are always accessible**; `/api` and `/manage` require a key when auth is enabled, **with the model-list endpoints as a public exception**.
- Clients carry the key via `Authorization: Bearer <key>` (OpenAI style) or `x-api-key: <key>` (Anthropic style); missing/invalid returns 401 + `WWW-Authenticate`. Comparison uses SHA-256 digests + `ConstantTimeCompare` (no key length/existence leak). Auth runs **before** the request body is decoded.

### Architecture

```
internal/gateway/   # Adapter layer: canonical intermediate types + three format adapters (bidirectional request/response/stream conversion + passthrough)
internal/provider/  # Providers: Config (per-model multi-format kinds + context_window), dispatch by model Kind, Manager (JSON persistence)
internal/providertemplates/ # Built-in provider template library (go:embed templates.json, no hardcoded models, one-click connect via the UI template picker)
internal/group/     # Model groups: virtual provider + Manager (URL collision validation)
internal/aggregate/ # Aggregate models: members derived from providers + exclusion list/channel priority/load-balance switch persistence + failover cooldown (memory-only) + context window = min over members
internal/apikey/    # Managed API keys: generate/query/delete + JSON persistence
internal/claude/    # Claude Code presets: Config (mirrors env block) + Manager + command generation (PS/bash) + apply-local settings.json
internal/codex/     # OpenAI Codex presets: Config + Manager + -c command generation + TOML merge (apply-local) + native-alias model catalog
internal/zcode/     # Z.ai zcode presets: Config (model list + api_key) + Manager + override local config.json (split into multiple providers by native format)
internal/network/   # Deployment-mode detection (NIC only, zero external requests) + egress address (IP + mapped port) JSON persistence
internal/server/    # Gateway HTTP service: forwarding + management + probes + groups + aggregates + logs + managed keys + presets + deployment + SPA mount
internal/logger/    # JSONL request logs (write + recent-N read + detail levels + key redaction/truncation)
webui/              # Embedded frontend: webui.go (go:embed BSRouterWebUI/dist) + BSRouterWebUI (Vite+React+TS)
cmd/gateway/        # Runnable entrypoint
```

The core design is the **canonical intermediate type**: each format implements a pair of "format ↔ canonical" conversions that are **symmetric** on both request and response sides; streaming interoperates through canonical events too (cross-format included), with direct passthrough when formats match. Key mechanisms:

- **Passthrough**: when a model supports the request's wire format, the request/response is forwarded verbatim (only the `model` field is rewritten), bypassing the conversion layer.
- **Failover**: aggregate models try members in channel-priority order; on member error it switches to the next, returning the last error only if all fail; failed members are cooldown-banned for 10 minutes (memory-only) when another member succeeds.
- **Thinking/reasoning round-trip**: `reasoning_content` / reasoning items / thinking blocks from thinking upstreams like deepseek are preserved and echoed back across formats; otherwise the upstream rejects multi-turn conversations with a 400.
- **Orphan tool_use repair / non-function tool skipping / multi-tool_result splitting**: keeps forwarded requests always legal for strict upstreams.

### Development

The frontend and backend are built separately:

```bash
# Frontend build (produces dist, embedded via go:embed)
cd webui/BSRouterWebUI && npm run build

# Go compile / test / vet (run from the repo root)
go build ./...
go test ./... -v
go vet ./...
```

> The frontend build output `dist/` and local tool files are ignored in `.gitignore` and not committed.

### License

Licensed under the [MIT License](LICENSE).

```
MIT License

Copyright (c) 2026 TTTTSR

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
