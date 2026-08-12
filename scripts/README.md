# BSRouter `bsr` 命令行与一键安装

`bsr` 是 BSRouter 网关的**进程管理命令行**（start / stop / restart / status 等），`install.sh` / `install.ps1` 是一键安装器。

| 平台 | 脚本 | 安装器 |
|---|---|---|
| Linux / macOS | `bsr`（POSIX sh） | `install.sh` |
| Windows | `bsr.ps1` + `bsr.cmd`（PS 5.1 兼容） | `install.ps1` + `install.bat` |

## `bsr` 用法

```
bsr <command> [网关参数...]

start    后台启动网关
stop     停止网关(Unix 先 SIGTERM 优雅退出,超 10s 强杀;Windows 直接 Stop-Process)
restart  先停后启(有参数用新参数,无参数复用上次 start 的参数)
status   查看运行状态(运行中退出码 0,未运行退出码 3)
run      前台运行网关(Ctrl+C 优雅退出,用于诊断)
log      打印包装器日志最近 50 行;`log tail` 实时跟随
version  打印 bsr 版本 + 网关路径 + 网关版本
help     用法
```

示例：

```bash
bsr start -private                     # 自动生成 key 并后台运行
bsr start -addr :9000 -api-key sk-...  # 指定端口与 key
bsr restart                            # 复用上次参数重启
bsr log tail                           # 实时看包装器日志
```

### 状态文件（默认 OS 用户配置目录，与网关一致）

- **Linux** `~/.config/BSRouter` / **macOS** `~/Library/Application Support/BSRouter` / **Windows** `%APPDATA%\BSRouter`
- `bsr.pid` — 网关进程 PID；`bsr.args` — 上次 `start` 的网关参数（`restart` 复用）；`bsr.log` / `bsr.stdout.log`+`bsr.stderr.log` — **包装器日志**（当前运行期的网关 stdout/stderr，每次 `start` 截断）。
- 网关自身的**请求日志**是另一份 `gateway-<时间戳>.log.jsonl`，互不影响。

### 环境变量

| 变量 | 作用 |
|---|---|
| `BSR_GATEWAY` | 指定网关二进制路径（覆盖自动探测） |
| `BSR_CONFIG_DIR` | 指定状态目录（覆盖默认，便于测试/便携） |

网关二进制查找顺序：`$BSR_GATEWAY` → 脚本同目录的 `gateway`/`gateway.exe` → 上级目录（仓库根跑 `scripts/bsr` 时命中）→ `$PATH`。

### 已知局限

- **Windows 停止是强杀**：后台进程收不到 SIGTERM，网关的优雅退出（等最多 10s）在 Windows 上不执行。
- **`restart` 参数复用按空格切分**：含空格/通配符的参数请显式重传给 `restart`/`start`。
- 若网关由其他方式启动（非 `bsr`），`bsr` 看不到它；再次 `start` 会因端口占用启动失败并打印日志尾部。

## 一键安装

### Linux / macOS

```bash
curl -fsSL https://raw.githubusercontent.com/TTTTSR/BSRouter/main/scripts/install.sh | sh
# 或下载后本地执行:
./install.sh
```

选项：`--version <ver>`（默认 `latest`）、`--base-url <url>`、`--prefix <dir>`（默认 `~/.local`，装到 `<prefix>/bin`）、`--local <构建目录>`（从本地构建安装，跳过下载）、`--no-path`（不改 PATH）。

安装后 `bsr` 与 `gateway` 位于 `~/.local/bin`，脚本自动把该目录加入 `PATH`（写入 `~/.bashrc` / `~/.zshrc` / `~/.profile`，按 `$SHELL` 判定）。

### Windows

**一键安装**（`irm` 拉取并直接执行脚本，无需下载到本地）：

```powershell
irm https://raw.githubusercontent.com/TTTTSR/BSRouter/main/scripts/install.ps1 | iex
```

或下载后本地执行（**可传自定义参数**）：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1
# 或双击 / cmd:
install.bat
```

选项：`-Version`、`-BaseUrl`、`-Prefix`（默认 `%LOCALAPPDATA%\BSRouter`，装到 `<prefix>\bin`）、`-Local -LocalDir <dir>`、`-NoPath`。安装后自动把 `<prefix>\bin` 追加到**用户级 PATH**（新终端生效）。

> 注意：`irm ... | iex` 一键命令使用**默认参数**（`-Version latest`、装到 `%LOCALAPPDATA%\BSRouter\bin`、自动改用户 PATH），且无法透传自定义参数；需要 `-Version`/`-Prefix`/`-NoPath` 等选项时，用「下载后本地执行」的方式调用。

### 从本地构建安装（无需发布，立即可用）

```bash
# Linux/macOS
cd BSRouter && go build -o gateway ./cmd/gateway
bash scripts/install.sh --local . --prefix ~/.local

# Windows
cd BSRouter && go build -o gateway.exe ./cmd/gateway
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/install.ps1 -Local -LocalDir .
```

## 发布资产命名约定（打 Release 时遵循）

安装脚本按以下约定从 `<base-url>/<version>/` 下载。`-Version latest`（默认）会先经 GitHub API `releases/latest` 解析为**最新 release 的真实标签名**（如 `v0.2.0`），再据此拼资产 URL——资产按版本命名，无需发布 `latest` 字面名的文件：

| 资产 | 内容 |
|---|---|
| `bsr-<version>-linux-amd64.tar.gz` / `-arm64` | `gateway` + `bsr` |
| `bsr-<version>-darwin-amd64.tar.gz` / `-arm64` | `gateway` + `bsr` |
| `bsr-<version>-windows-amd64.zip` / `-arm64` | `gateway.exe` + `bsr.ps1` + `bsr.cmd` |

打包示例（GitHub Actions 或手动）：

```bash
# linux/darwin
tar -czf bsr-$V-$OS-$ARCH.tar.gz -C <stage> gateway bsr
# windows
powershell -Command "Compress-Archive -Path <stage>\gateway.exe,<stage>\bsr.ps1,<stage>\bsr.cmd -DestinationPath bsr-$V-windows-$ARCH.zip"
```

配合网关 `-version` flag（`go build -ldflags "-X main.version=$V" ./cmd/gateway`）即可让 `bsr version` 输出真实版本。
