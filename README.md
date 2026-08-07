# BSRouter

大模型网关（LLM Gateway）——为 **agent 使用者**提供统一接入入口，连接来自不同供应商的大模型服务（OpenAI、Anthropic、Azure OpenAI、国内厂商等），屏蔽各供应商 API 的差异。

- **三种接口格式互通**：Anthropic Messages / OpenAI chat.completions / responses，请求响应格式由客户端决定，上游任意。
- **内置 Web 界面**：黑白灰扁平管理 UI，管理供应商、模型分组、日志、API Key、Claude Code 预设。
- **单二进制分发**：前端经 `go:embed` 内嵌，`make build` 后一个可执行文件即可运行。
- **零外部依赖**：纯 Go 标准库实现。

## 快速开始

### 前置

- Go 1.25+
- Node.js（仅构建前端时需要）

### 构建

```bash
# 一键构建:先 npm run build(前端)再 go build ./...(内嵌前端)
make build

# 仅构建前端(go:embed 依赖 dist,若缺失 go build 会失败)
cd webui/BSRouterWebUI && npm install && npm run build
```

### 运行

```bash
# 启动网关(默认监听 127.0.0.1:18154,配置文件默认存 OS 用户配置目录,见下方「配置文件位置」)
go run ./cmd/gateway

# 自动生成随机 API Key 并打印
go run ./cmd/gateway -private

# 显式指定 API Key
go run ./cmd/gateway -api-key <key>
```

启动后访问 `http://127.0.0.1:18154/` 打开管理界面，输入网关 API Key 登录（未配置 key 时网关不鉴权，适合纯本地使用）。

### 常用命令

```bash
go build ./...          # 编译(内嵌前端)
go vet ./...            # 静态检查
go test ./... -v        # 全部测试
go test ./... -race     # 竞态检测
```

## 配置文件位置

全部配置（`providers.json` / `groups.json` / `keys.json` / `claude.json` / `aggregates.json`）与请求日志 `gateway.log.jsonl` 默认存 **OS 用户配置目录**（跨平台惯例，Go `os.UserConfigDir()`）：

```
Windows: %APPDATA%\BSRouter\
macOS:   ~/Library/Application Support/BSRouter/
Linux:   ~/.config/BSRouter/
```

首次以默认路径启动时，会自动把运行目录下已有的同名配置**迁移**到该目录（源文件保留，不覆盖已存在目标）。任一配置 flag 显式传路径即覆盖默认（相对当前运行目录解析，适合便携/分发）。

## 配置

### 供应商（`providers.json`）

```json
[
  {"kind": "completion", "name": "openai", "base_url": "https://api.openai.com",
   "api_key": "sk-...", "models": [{"name": "gpt-4o"}, {"name": "claude-x", "kind": "anthropic"}],
   "usage_url": "https://api.openai.com/usage", "models_url": ""}
]
```

- `kind`：供应商**默认**接口格式（`anthropic` / `completion` / `responses`）。
- `models`：模型列表，每个模型可单独用 `kind` 覆盖接口格式，留空用供应商默认。
- 同一供应商的不同模型可拥有不同格式。

### 模型路由

请求体中的模型名：

- `{供应商名}@{模型名}` 合成 id —— 网关按首个 `@` 切分路由到对应供应商。
- **聚合裸名**（无 `@`）—— 自动聚合同名模型，网关**轮询**负载均衡 + **故障转移**：成员返回错误时自动切换到其余成员，全部失败才返回错误；有成员成功时，失败成员在该聚合下**冷却禁用 10 分钟**（仅内存）。

### 模型分组

分组是面向下游的**虚拟供应商**，挂载到自己的 `/api` 下 URL：

```json
[
  {"name": "team-a", "kind": "completion", "url": "/api/team-a",
   "models": ["openai@gpt-4o", "anthropic@claude-sonnet-4-5"]}
]
```

客户端以分组的接口格式调用，路由到组内模型对应的真实上游（支持**跨格式**）。

### 受管 API Key（`keys.json`）

为下游模型请求生成/删除 key（`sk-` + 64 位 a-zA-Z0-9），`/api` 转发端点鉴权额外接受；`/manage` 仍只认网关 key。

### Claude Code 配置预设（`claude.json`）

预设若干 Claude Code 运行配置（镜像 settings.json 的 `env` 块），一键生成 PowerShell / bash 启动命令，实现多终端环境分隔。预设不填密钥时，命令端点自动注入系统默认 key。

## API

统一 API（客户端将 base_url 设为 `http://<host>/api`，再调用标准路径）：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/messages` | Anthropic 格式 |
| POST | `/api/v1/chat/completions` | chat.completions 格式 |
| POST | `/api/v1/responses` | responses 格式 |
| GET  | `/api/v1/models` | 聚合所有供应商模型 |

管理接口位于 `/manage/v1/*`（供应商增删改查、连通性探测、分组、日志、受管 key、Claude 预设、聚合模型、部署状态）。

## 远程部署

按 `-addr` 绑定地址 + 网卡检测判定部署形态：

- **local**：绑定回环（默认）→ 本地部署。
- **direct**：绑定全部接口且网卡有直连公网 IP → 自动广告 `http://<公网IP>:<端口>`。
- **nat**：云 NAT / 路由器后 → 不做自动探测（异端口映射无法从内部探测），前端醒目提醒填写出口 IP 与映射端口。

出口地址优先级：`-public-addr <URL>`（手工覆盖）> UI 填写的出口 > direct 自动广告。Claude 预设命令在远程部署下把指向本机的 base_url 动态替换为对外地址（不改存储）。

## 架构

```
internal/gateway/   # 接入层:规范化中间类型 + 三种格式适配器
internal/provider/  # 供应商:Config、按模型 Kind 派发转发、Manager(JSON 持久化)
internal/group/     # 模型分组:虚拟供应商 + Manager
internal/aggregate/ # 聚合模型:成员派生 + 剔除名单 + 轮询 + 故障转移冷却
internal/apikey/    # 受管 API Key
internal/claude/    # Claude Code 配置预设 + 命令生成
internal/network/   # 部署形态检测(仅网卡)+ 出口地址持久化
internal/server/    # 网关 HTTP 服务:转发 + 管理 + 探测 + 分组 + 日志 + SPA 挂载
internal/logger/    # JSONL 请求日志
webui/              # 内嵌前端(go:embed)
cmd/gateway/        # 可运行入口
```

核心设计是**规范化中间类型（canonical）**：三种格式各自实现一对「格式 → 规范化」转换，请求与响应两侧对称，流式输出同样经规范化事件互通（跨格式也可）。

## 开发

```bash
make build      # 一键构建(前端 + Go)
make test       # go test ./...
make vet        # go vet ./...
make clean      # 清理前端构建产物
```

> 仓库通过 Makefile 管理构建；`makefile`/`Makefile` 均已在 `.gitignore` 中忽略（按需重新加入版本控制）。

## 许可证

项目尚未选择开源许可证，保留所有权利。
