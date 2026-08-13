// Command gateway 启动大模型网关:启动时从本地 JSON 读取供应商配置,
// 并对外提供统一 API(/api)、管理接口(/manage)与内置 Web 界面。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"BSRouter/internal/aggregate"
	"BSRouter/internal/apikey"
	"BSRouter/internal/claude"
	"BSRouter/internal/codex"
	"BSRouter/internal/dsh"
	"BSRouter/internal/fault"
	"BSRouter/internal/group"
	"BSRouter/internal/logger"
	"BSRouter/internal/network"
	"BSRouter/internal/provider"
	"BSRouter/internal/server"
	"BSRouter/internal/zcode"
	"BSRouter/webui"
)

// shutdownTimeout 是优雅退出时等待在途请求完成的最长时间。
const shutdownTimeout = 10 * time.Second

// version 是构建时注入的版本号(经 -ldflags "-X main.version=..." 覆盖);默认 dev。
var version = "dev"

// defaultConfigDir 返回按平台惯例的 BSRouter 用户配置目录(Go os.UserConfigDir 映射:
// Windows %APPDATA%\BSRouter / macOS ~/Library/Application Support/BSRouter /
// Linux $XDG_CONFIG_HOME/BSRouter 或 ~/.config/BSRouter)。系统无法确定该目录
// (如 HOME / APPDATA 未设置)时返回空串,调用方据此回退到当前运行目录。
func defaultConfigDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		log.Printf("warning: cannot determine OS config dir (%v); using the current directory", err)
		return ""
	}
	return filepath.Join(dir, "BSRouter")
}

// resolveDeployment 按监听地址与网卡检测判定部署形态:
//   - 绑定回环 → local(本地部署);
//   - 绑定全部接口(:port / 0.0.0.0)且网卡有直连公网 IPv4,或显式绑定公网 IP → direct;
//   - 其余(绑定非回环但无直连公网,如云 NAT/路由器后)→ nat。
//
// nat 形态下出口地址(公网 IP + 映射端口)无法从网关内部自动探测,交由用户通过
// -public-addr 或管理界面填写。PublicAddr 为 -public-addr 手工覆盖(最高优先)。
// detect 参数便于测试注入。
func resolveDeployment(addr, publicAddr string, det network.Detection) *server.Deployment {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || port == "0" {
		port = "80"
	}
	dep := &server.Deployment{
		Mode:       server.ModeNAT,
		ListenPort: port,
		PublicAddr: strings.TrimSpace(publicAddr),
	}
	if host != "" && network.IsLoopbackHost(host) {
		dep.Mode = server.ModeLocal
		return dep
	}
	if ip := net.ParseIP(host); ip != nil && network.IsPublicIP(ip) {
		// 显式绑定公网 IP:直接广告该 IP。
		dep.Mode = server.ModeDirect
		dep.DirectPublicIP = ip.String()
		return dep
	}
	if (host == "" || host == "0.0.0.0") && det.DirectPublicIP != "" {
		// 绑定全部接口且网卡直连公网(无 NAT)。
		dep.Mode = server.ModeDirect
		dep.DirectPublicIP = det.DirectPublicIP
		return dep
	}
	return dep
}

// normalizePublicAddr 校验并规范化 -public-addr 为完整 base URL(去掉尾部 "/")。
func normalizePublicAddr(addr string) string {
	s := strings.TrimSpace(addr)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		log.Printf("warning: -public-addr %q should be a full URL (e.g. https://gw.example.com or http://1.2.3.4:443)", s)
	}
	if u, err := url.Parse(s); err != nil || u.Scheme == "" || u.Host == "" {
		log.Printf("warning: -public-addr %q is not a valid URL, ignoring", s)
		return ""
	}
	return strings.TrimRight(s, "/")
}

// configPath 把文件名挂到配置目录下;目录为空(无法确定)时原样返回,回退当前运行目录。
func configPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return filepath.Join(dir, name)
}

// defaultLogName 返回默认请求日志文件名:每次运行以启动时间戳命名(独立文件,
// 天然截断——新文件从空开始,不累积旧运行内容)。显式传 -log 时尊重用户路径。
func defaultLogName() string {
	return "gateway-" + time.Now().Format("20060102-150405") + ".log.jsonl"
}

// resolveLogDetail 解析日志完整度:显式传 -log-detail 用它;否则读持久化文件;
// 都无则 default。返回 (detail, 是否合法)。
func resolveLogDetail(explicit bool, flagVal, filePath string) string {
	if explicit {
		if flagVal == server.LogDetailFull {
			return server.LogDetailFull
		}
		if flagVal == server.LogDetailDefault {
			return server.LogDetailDefault
		}
		if flagVal != "" {
			log.Printf("warning: invalid -log-detail %q, using default", flagVal)
		}
		return server.LogDetailDefault
	}
	if filePath != "" {
		if data, err := os.ReadFile(filePath); err == nil {
			var cfg struct {
				Detail string `json:"detail"`
			}
			if json.Unmarshal(data, &cfg) == nil && (cfg.Detail == server.LogDetailFull || cfg.Detail == server.LogDetailDefault) {
				return cfg.Detail
			}
		}
	}
	return server.LogDetailDefault
}

// migrateFile 把 src 复制到 dst(仅当 dst 不存在时),用于把旧版散落在运行目录的配置
// 一次性迁移到 OS 用户配置目录。源文件保留不动,由用户决定是否清理;目录不存在则创建。
func migrateFile(dst, src string) {
	if dst == "" || dst == src {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		return // 目标已存在(旧版本已迁移或用户自建),绝不覆盖
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return // 运行目录没有该文件,无需迁移
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		log.Printf("warning: cannot create config dir %s: %v", filepath.Dir(dst), err)
		return
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		log.Printf("warning: migrate %s -> %s: %v", src, dst, err)
		return
	}
	log.Printf("migrated %s -> %s (old copy kept)", src, dst)
}

func main() {
	// 配置默认存 OS 用户配置目录(跨平台惯例),显式传路径则按用户原意解析(相对当前目录)。
	cfgDir := defaultConfigDir()
	var (
		// 默认仅绑定本机回环地址,避免管理端点与转发端点直接暴露到局域网;
		// 如需局域网访问可显式传入 -addr :18154。
		addr   = flag.String("addr", "127.0.0.1:18154", "HTTP listen address")
		config = flag.String("config", configPath(cfgDir, "providers.json"), "provider config JSON file path")
		// 网关 API Key,所有端点统一鉴权(Authorization: Bearer 或 x-api-key)。
		apiKey = flag.String("api-key", os.Getenv("GATEWAY_API_KEY"), "gateway api key (all endpoints require it)")
		// 私密模式:自动生成随机 API Key 用于鉴权并打印;不加该参数且未提供 -api-key 时,网关不鉴权。
		private = flag.Bool("private", false, "generate a random api key for auth and print it; without it (and no -api-key) the gateway runs unauthenticated")
		// 请求日志文件(JSONL);传空字符串禁用。默认以启动时间戳命名每次运行的独立
		// 文件(gateway-<时间戳>.log.jsonl);显式传路径则按用户原意(追加,不截断)。
		logPath = flag.String("log", configPath(cfgDir, defaultLogName()), "request log file path (JSONL, empty disables; default: gateway-<timestamp>.log.jsonl in config dir)")
		// 日志完整度:default 仅出错时记录完整转发详情;full 全部记录。
		// 显式传该 flag 用其值;否则读持久化文件;都无则 default。
		logDetail = flag.String("log-detail", "", "log detail level (default|full; default: only errors record full forward detail, full: record all)")
		// 日志完整度持久化文件(管理界面开关写回,下次启动读取);传空字符串禁用持久化。
		logDetailFile = flag.String("log-detail-file", configPath(cfgDir, "logdetail.json"), "log detail level persistence file (empty disables)")
		// 模型分组配置文件;传空字符串禁用分组功能。
		groupsPath = flag.String("groups", configPath(cfgDir, "groups.json"), "model group config JSON file path (empty disables)")
		// 受管 API Key 文件(供下游模型请求鉴权);传空字符串禁用。
		keysPath = flag.String("keys", configPath(cfgDir, "keys.json"), "managed api key config JSON file path (empty disables)")
		// Claude Code 配置预设文件;传空字符串禁用。
		claudePath = flag.String("claude", configPath(cfgDir, "claude.json"), "claude code preset config JSON file path (empty disables)")
		// OpenAI Codex 配置预设文件;传空字符串禁用。
		codexPath = flag.String("codex", configPath(cfgDir, "codex.json"), "codex preset config JSON file path (empty disables)")
		// Z.ai zcode 配置预设文件;传空字符串禁用。
		zcodePath = flag.String("zcode", configPath(cfgDir, "zcode.json"), "zcode preset config JSON file path (empty disables)")
		// DeepSeek Harness (dsh) 配置预设文件;传空字符串禁用。
		dshPath = flag.String("dsh", configPath(cfgDir, "dsh.json"), "deepseek harness (dsh) preset config JSON file path (empty disables)")
		// 聚合模型配置(剔除名单)文件;传空字符串禁用。
		aggregatesPath = flag.String("aggregates", configPath(cfgDir, "aggregates.json"), "aggregate model config JSON file path (empty disables)")
		// 出口地址配置(NAT 部署下用户填写的出口 IP 与映射端口)文件;传空字符串禁用。
		networkPath = flag.String("network", configPath(cfgDir, "network.json"), "egress address config JSON file path (empty disables)")
		// 手工覆盖的对外广告地址(完整 base URL,如 https://gw.example.com 或 http://1.2.3.4:443);
		// 优先级最高,用于 NAT/反向代理下自动检测不到的部署。留空时按部署形态自动推导。
		publicAddr = flag.String("public-addr", "", "advertised base URL for remote access (e.g. https://gw.example.com or http://1.2.3.4:443); overrides auto-detection")
		// 覆盖本地 Claude Code 配置的目标 settings.json 路径;留空默认 ~/.claude/settings.json。
		claudeSettings = flag.String("claude-settings", "", "path to local Claude Code settings.json to override (default ~/.claude/settings.json)")
		// 覆盖本地 Codex 配置的目标 config.toml 路径;留空默认 ~/.codex/config.toml。
		codexConfig = flag.String("codex-config", "", "path to local codex config.toml to override (default ~/.codex/config.toml)")
		// 覆盖本地 Codex 鉴权的目标 auth.json 路径;留空默认 ~/.codex/auth.json。
		codexAuth = flag.String("codex-auth", "", "path to local codex auth.json to override (default ~/.codex/auth.json)")
		// 覆盖本地 Codex 模型目录的目标文件路径;留空默认 ~/.codex/bsrouter-models.json。
		codexModelCatalog = flag.String("codex-model-catalog", "", "path to local codex model catalog json to override (default ~/.codex/bsrouter-models.json)")
		// 覆盖本地 Codex 模型缓存的目标文件路径;留空默认 ~/.codex/models_cache.json(桌面 app 读)。
		codexModelsCache = flag.String("codex-models-cache", "", "path to local codex models cache json to override (default ~/.codex/models_cache.json)")
		// 覆盖本地 zcode 配置的目标 config.json 路径;留空默认 ~/.zcode/v2/config.json。
		zcodeConfig = flag.String("zcode-config", "", "path to local zcode config.json to override (default ~/.zcode/v2/config.json)")
		dshConfig   = flag.String("dsh-config", "", "path to local dsh settings.yaml to override (default ~/.dsh/settings.yaml)")
		// 上游流式响应体 idle 超时:两字节数据到达间隔超过该值即中止流(上游挂起/断流时
		// 避免无限等待并让错误可被记录)。默认 0 禁用(思考模型可能长时间无增量,启用时
		// 建议 ≥120s)。
		streamIdleTimeout = flag.Duration("stream-idle-timeout", 0, "upstream stream idle timeout (0 disables; abort when no stream data for this long, e.g. 120s)")
		// 流开始前失败(请求发送错误 / 上游非 2xx)的每成员重试次数;默认 2(共尝试 3 次)。
		streamRetries = flag.Int("stream-retries", 2, "retries per member for pre-stream failures (5xx / transport; 0 disables)")
		faultsPath    = flag.String("faults", configPath(cfgDir, "faults.json"), "fault records JSON file path (empty disables)")
		// 故障捕捉模式:user 仅捕捉硬编码特定故障(当前仅余额不足);dev 捕捉所有错误(内部+上游)。
		// 模式仅由启动参数指定,前端不提供切换。
		faultMode = flag.String("fault-mode", string(fault.ModeUser), "fault capture mode (user: only hardcoded faults e.g. insufficient balance; dev: all errors)")
		ver       = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *ver {
		fmt.Printf("BSRouter gateway %s\n", version)
		os.Exit(0)
	}

	// 日志完整度:显式 -log-detail 优先,否则读持久化文件,都无则 default。
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	logDetailLevel := resolveLogDetail(explicit["log-detail"], *logDetail, *logDetailFile)
	if logDetailLevel != server.LogDetailDefault {
		log.Printf("log detail level: %s", logDetailLevel)
	}

	// 配置目录就绪 + 一次性迁移:首次以默认路径启动时,把运行目录下已有的同名配置
	// 复制到 OS 用户目录(仅目标不存在时,源文件保留)。显式传路径(便携式/自定义)不迁移。
	if cfgDir != "" {
		if err := os.MkdirAll(cfgDir, 0o755); err != nil {
			log.Printf("warning: cannot create config dir %s: %v", cfgDir, err)
		}
		if !explicit["config"] {
			migrateFile(*config, "providers.json")
		}
		if !explicit["groups"] {
			migrateFile(*groupsPath, "groups.json")
		}
		if !explicit["keys"] {
			migrateFile(*keysPath, "keys.json")
		}
		if !explicit["claude"] {
			migrateFile(*claudePath, "claude.json")
		}
		if !explicit["codex"] {
			migrateFile(*codexPath, "codex.json")
		}
		if !explicit["zcode"] {
			migrateFile(*zcodePath, "zcode.json")
		}
		if !explicit["dsh"] {
			migrateFile(*dshPath, "dsh.json")
		}
		if !explicit["aggregates"] {
			migrateFile(*aggregatesPath, "aggregates.json")
		}
		// 请求日志不做运行目录迁移:默认名含启动时间戳(gateway-<时间戳>.log.jsonl),
		// 每次运行是独立新文件,迁移旧 gateway.log.jsonl 无意义。
	}

	// 鉴权 key 优先级:-private > -api-key > 无鉴权。
	key := strings.TrimSpace(*apiKey)
	if *private {
		if key != "" {
			log.Printf("warning: -private set, ignoring -api-key")
		}
		key = generateAPIKey()
		log.Printf("api-key auth enabled, generated key: %s", key)
	}

	mgr, err := provider.NewManager(*config)
	if err != nil {
		log.Fatalf("load provider config: %v", err)
	}
	log.Printf("loaded %d provider(s) from %s", len(mgr.List()), *config)

	var gm *group.Manager
	if *groupsPath != "" {
		gm, err = group.NewManager(*groupsPath)
		if err != nil {
			log.Fatalf("load group config: %v", err)
		}
		log.Printf("loaded %d group(s) from %s", len(gm.List()), *groupsPath)
	}

	var lg *logger.Logger
	if *logPath != "" {
		lg, err = logger.New(*logPath)
		if err != nil {
			log.Fatalf("open request log: %v", err)
		}
	}

	var km *apikey.Manager
	if *keysPath != "" {
		km, err = apikey.NewManager(*keysPath)
		if err != nil {
			log.Fatalf("load api key config: %v", err)
		}
		log.Printf("loaded %d managed api key(s) from %s", km.Count(), *keysPath)
	}

	var cm *claude.Manager
	if *claudePath != "" {
		cm, err = claude.NewManager(*claudePath)
		if err != nil {
			log.Fatalf("load claude preset config: %v", err)
		}
		log.Printf("loaded %d claude preset(s) from %s", cm.Count(), *claudePath)
	}

	var xm *codex.Manager
	if *codexPath != "" {
		xm, err = codex.NewManager(*codexPath)
		if err != nil {
			log.Fatalf("load codex preset config: %v", err)
		}
		log.Printf("loaded %d codex preset(s) from %s", xm.Count(), *codexPath)
	}

	var zm *zcode.Manager
	if *zcodePath != "" {
		zm, err = zcode.NewManager(*zcodePath)
		if err != nil {
			log.Fatalf("load zcode preset config: %v", err)
		}
		log.Printf("loaded %d zcode preset(s) from %s", zm.Count(), *zcodePath)
	}

	var dm *dsh.Manager
	if *dshPath != "" {
		dm, err = dsh.NewManager(*dshPath)
		if err != nil {
			log.Fatalf("load dsh preset config: %v", err)
		}
		log.Printf("loaded %d dsh preset(s) from %s", dm.Count(), *dshPath)
	}

	var am *aggregate.Manager
	if *aggregatesPath != "" {
		am, err = aggregate.NewManager(*aggregatesPath, mgr)
		if err != nil {
			log.Fatalf("load aggregate config: %v", err)
		}
		log.Printf("loaded %d aggregate model(s) from %s", len(am.Models()), *aggregatesPath)
	}

	var nm *network.Manager
	if *networkPath != "" {
		nm, err = network.NewManager(*networkPath)
		if err != nil {
			log.Fatalf("load network config: %v", err)
		}
	}

	var fm *fault.Manager
	if *faultsPath != "" {
		mode := fault.Mode(*faultMode)
		switch mode {
		case fault.ModeUser, fault.ModeDev:
		default:
			log.Printf("warning: invalid -fault-mode %q, using user", *faultMode)
			mode = fault.ModeUser
		}
		fm, err = fault.NewManager(*faultsPath, mode)
		if err != nil {
			log.Fatalf("load fault records: %v", err)
		}
		log.Printf("fault capture: mode=%s, loaded %d fault(s) from %s", mode, fm.Count(), *faultsPath)
	}

	// 部署形态判定:按 -addr 绑定地址 + 网卡直连公网 IP 区分 local/direct/nat。
	dep := resolveDeployment(*addr, normalizePublicAddr(*publicAddr), network.Detect())
	switch dep.Mode {
	case server.ModeNAT:
		log.Printf("deployment: nat (listening on %s, no direct public IP; egress address must be set via -public-addr or the web UI)", *addr)
	case server.ModeDirect:
		log.Printf("deployment: direct public IP %s, advertising base http://%s:%s", dep.DirectPublicIP, dep.DirectPublicIP, dep.ListenPort)
	default:
		log.Printf("deployment: local (listening on %s)", *addr)
	}
	if *publicAddr != "" {
		log.Printf("deployment: -public-addr set, advertising base %s", strings.TrimRight(*publicAddr, "/"))
	}

	srv := server.New(mgr).WithAPIKey(key).WithLogger(lg).WithGroups(gm).WithWebUI(webui.Handler()).WithAPIKeys(km).WithClaudePresets(cm).WithCodexPresets(xm).WithZcodePresets(zm).WithDshPresets(dm).WithAggregates(am).WithFaults(fm).WithDeployment(dep).WithNetworkManager(nm).WithStreamIdleTimeout(*streamIdleTimeout).WithStreamRetries(*streamRetries)
	if *logDetailFile != "" {
		srv = srv.WithLogDetailPath(*logDetailFile)
	}
	srv = srv.WithLogDetail(logDetailLevel)
	if *claudeSettings != "" {
		srv = srv.WithClaudeSettingsPath(*claudeSettings)
	}
	if *codexConfig != "" {
		srv = srv.WithCodexConfigPath(*codexConfig)
	}
	if *codexAuth != "" {
		srv = srv.WithCodexAuthPath(*codexAuth)
	}
	if *codexModelCatalog != "" {
		srv = srv.WithCodexModelCatalogPath(*codexModelCatalog)
	}
	if *codexModelsCache != "" {
		srv = srv.WithCodexModelsCachePath(*codexModelsCache)
	}
	if *zcodeConfig != "" {
		srv = srv.WithZcodeConfigPath(*zcodeConfig)
	}
	if *dshConfig != "" {
		srv = srv.WithDshSettingsPath(*dshConfig)
	}

	if key == "" {
		log.Printf("warning: no api key configured; all endpoints are UNAUTHENTICATED (use -private to generate a key)")
		log.Printf("gateway listening on %s (no auth, request log: %s)", *addr, *logPath)
	} else {
		log.Printf("gateway listening on %s (api-key auth enabled, request log: %s)", *addr, *logPath)
	}

	// 优雅退出:监听中断信号(SIGINT / SIGTERM),触发后停止接收新连接、
	// 等待在途请求完成(超时强制关闭)、最后关闭日志文件。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runServer(ctx, *addr, srv.Handler(), lg); err != nil {
		log.Fatal(err)
	}
}

// runServer 启动 HTTP 服务并阻塞,直到 ctx 被取消(如收到中断信号)后优雅退出;
// 退出前关闭日志,确保在途请求的日志已写入。返回 nil 表示正常退出。
func runServer(ctx context.Context, addr string, handler http.Handler, lg *logger.Logger) error {
	httpSrv := &http.Server{Addr: addr, Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Printf("shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("forced shutdown: %v", err)
		}
		// 服务器已停止,再关闭日志,确保在途请求日志已写入。
		if lg != nil {
			if err := lg.Close(); err != nil {
				log.Printf("close request log: %v", err)
			}
		}
		log.Printf("shutdown complete")
		return nil
	}
}

// generateAPIKey 生成 32 字节(64 位十六进制)的随机 API Key。
func generateAPIKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("generate api key: %v", err)
	}
	return hex.EncodeToString(b)
}
