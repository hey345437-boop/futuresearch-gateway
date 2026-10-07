// Command futuresearch-gateway 把 FutureSearch 的异步任务 API 反代成
// OpenAI 兼容端点 + 管理面板 + MCP 工具。
//
// 一个二进制、一个配置文件、一个 data 目录 —— 这就是全部。
//
//	futuresearch-gateway                 起网关（默认 http://127.0.0.1:7868）
//	futuresearch-gateway --mcp-stdio     当 MCP server 用（给本地 AI 客户端 spawn）
//	futuresearch-gateway --version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hey345437-boop/futuresearch-gateway/internal/config"
	"github.com/hey345437-boop/futuresearch-gateway/internal/fsapi"
	"github.com/hey345437-boop/futuresearch-gateway/internal/localfs"
	"github.com/hey345437-boop/futuresearch-gateway/internal/mcp"
	"github.com/hey345437-boop/futuresearch-gateway/internal/panel"
	"github.com/hey345437-boop/futuresearch-gateway/internal/pool"
	"github.com/hey345437-boop/futuresearch-gateway/internal/server"
	"github.com/hey345437-boop/futuresearch-gateway/internal/tenant"
)

const version = "1.0.0"

func main() {
	var (
		cfgPath  = flag.String("config", "config.json", "配置文件路径")
		mcpStdio = flag.Bool("mcp-stdio", false, "以 MCP server 模式跑（stdio 传输，供本地 AI 客户端 spawn）")
		showVer  = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("futuresearch-gateway", version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败：%v", err)
	}
	cfg.ApplyEnv() // 容器部署用：环境变量覆盖（docker run 一行起）
	// 首次启动（或 env 覆盖过）时把生效值落盘，面板上看到的和实际跑的一致。
	if err := config.Save(cfg, *cfgPath); err != nil {
		log.Printf("写回配置失败（不影响运行）：%v", err)
	}

	up := fsapi.NewWithProxy(cfg.Upstream.Proxy)
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	up.SetPrompts(cfg.Prompts)

	keysFile := filepath.Join(cfg.DataDir, "keys.json")
	p := pool.New(keysFile)

	// 租户层（给别人发 key + 额度）。文件不存在 = 空表 + 未启用。
	tenants := tenant.New(filepath.Join(cfg.DataDir, "tenants.json"))

	gw := server.New(server.Options{
		Config: cfg, Pool: p, Up: up, Panel: panel.Handler(), Tenants: tenants,
	})
	gw.SetConfigPath(*cfgPath)

	// MCP：研究工具挂 /mcp，本地项目工具挂 /mcp/local（**分开挂** —— 让人一眼看清
	// 哪些工具会碰本地磁盘）。
	researchMCP := mcp.New(mcp.NewResearch(gw))
	gw.AddMCPEndpoint(cfg.MCP.Path, researchMCP.HTTPHandler())

	var localFS *localfs.FS
	if cfg.LocalFS.Enabled {
		f, err := localfs.New(localfs.Config{
			Enabled: cfg.LocalFS.Enabled, Root: cfg.LocalFS.Root,
			AllowWrite: cfg.LocalFS.AllowWrite, AllowExec: cfg.LocalFS.AllowExec,
			MaxReadKB: cfg.LocalFS.MaxReadKB, MaxOutKB: cfg.LocalFS.MaxOutKB,
			TimeoutSec: cfg.LocalFS.TimeoutSec,
		})
		if err != nil {
			log.Fatalf("本地工具启用失败：%v", err)
		}
		localFS = f
		localMCP := mcp.New(mcp.NewLocal(f))
		gw.AddMCPEndpoint(cfg.LocalFS.Path, localMCP.HTTPHandler())
	}

	// stdio 模式：只带研究工具（本地工具本来就跑在本机，用 HTTP 挂更合适）
	if *mcpStdio {
		if err := mcp.New(mcp.NewResearch(gw)).ServeStdio(); err != nil {
			log.Fatalf("MCP stdio 退出：%v", err)
		}
		return
	}

	// 远程暴露时的安全闸：非环回监听必须同时设 api_key 与 admin_password
	if !cfg.Listen.IsLoopback() {
		if cfg.APIKey == "" || cfg.AdminPass == "" {
			log.Fatalf("监听 %s 已对外暴露，但 config.json 里 %s。\n"+
				"请设置 api_key（客户端鉴权）与 admin_password（面板鉴权）后重启；\n"+
				"若只想本机使用，把 listen.host 改回 127.0.0.1。",
				cfg.Listen.Addr(),
				missingBoth(cfg.APIKey, cfg.AdminPass))
		}
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	gw.StartBalanceLoop(ctx, 10*time.Minute)

	hs := &http.Server{Addr: cfg.Listen.Addr(), Handler: gw.Handler(), ReadHeaderTimeout: 30 * time.Second}
	ln, err := listen(cfg.Listen.Addr())
	if err != nil {
		log.Fatalf("监听 %s 失败：%v", cfg.Listen.Addr(), err)
	}

	gw.Logf("账号 %d 个，余额合计 $%.2f", p.Len(), float64(p.TotalBalanceCents())/100)
	if tenants.Enabled() {
		gw.Logf("租户层已启用，%d 把 key", len(tenants.List()))
	}
	fmt.Printf("\n  FutureSearch Gateway %s\n", version)
	fmt.Printf("  OpenAI 端点   http://%s/v1\n", cfg.Listen.Addr())
	fmt.Printf("  管理面板      http://%s/\n", cfg.Listen.Addr())
	if cfg.MCP.Enabled {
		fmt.Printf("  MCP(研究)     http://%s%s\n", cfg.Listen.Addr(), cfg.MCP.Path)
	}
	if localFS != nil {
		fmt.Printf("  MCP(本地项目) http://%s%s  →  root=%s\n", cfg.Listen.Addr(), cfg.LocalFS.Path, localFS.Root())
	}
	fmt.Printf("  数据目录      %s\n", cfg.DataDir)
	if p := strings.TrimSpace(cfg.Upstream.Proxy); p != "" {
		fmt.Printf("  上游代理      %s\n", p)
	} else {
		fmt.Printf("  上游代理      直连\n")
	}
	fmt.Println()

	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	// os.Interrupt 在 Windows 上是 Ctrl+C（那边没有 SIGTERM）
	signal.Notify(sig, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\n退出中…")
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutCtx)
}

func missingBoth(k, a string) string {
	switch {
	case k == "" && a == "":
		return "api_key 与 admin_password 都是空的"
	case k == "":
		return "api_key 是空的"
	default:
		return "admin_password 是空的"
	}
}
