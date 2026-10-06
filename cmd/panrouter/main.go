// PanRouter — 网盘直链聚合与加速下载服务(自用)。
// 依赖装配在 internal/app,本文件只负责:配置加载、安全守卫、看门狗、热加载、优雅停机。
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dengyie/panrouter/internal/api"
	"github.com/dengyie/panrouter/internal/app"
	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/pkg/log"
	"github.com/dengyie/panrouter/web"
)

// 默认开发版本;CI 用 -ldflags "-X main.version=<sha>" 覆盖。
var version = "0.1.0-m1"

func main() {
	cfgPath := flag.String("config", "", "配置文件路径(默认 ./config.yaml)")
	flag.Parse()

	cfg, usedPath, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	logger, err := log.New(cfg.Log.Level)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	// 安全守卫:对外监听时拒绝默认密钥(防止误发布)
	if !isLoopbackListen(cfg.Server.Listen) &&
		(cfg.Auth.APIToken == config.DefaultAPIToken || cfg.Auth.JWTSecret == config.DefaultJWTSecret) {
		logger.Fatal("refusing to start: 监听地址非 loopback 且 api_token/jwt_secret 仍为默认值,请先修改 config")
	}

	// 主密钥:优先环境变量,回落 JWTSecret(自用可接受,README 有说明)
	masterKey := os.Getenv("PANROUTER_MASTER_KEY")
	if masterKey == "" {
		masterKey = cfg.Auth.JWTSecret
		logger.Warn("PANROUTER_MASTER_KEY 未设置,凭据加密主密钥回落为 jwt_secret")
	}

	webFS, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		logger.Fatalf("embed web: %v", err)
	}

	a, err := app.Build(app.Options{
		Cfg: cfg, Logger: logger, MasterKey: masterKey,
	})
	if err != nil {
		logger.Fatalf("build app: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	deps := api.Deps{
		Version: version, Cfg: a.Cfg, Resolver: a.Resolver, Relay: a.Relay, Aria2: a.Aria2,
		Accounts: a.Accounts, Store: a.Store, AES: a.AES, Signer: a.Signer,
		Log: a.Log, Met: a.Met, WebFS: webFS,
	}
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           api.Router(deps),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 后台任务(凭据看门狗 30min、配置热加载 5s)由 App 编排;
	// stop 停止协程并等待退出,Shutdown 时兜底再调(幂等)。
	a.StartBackground(app.BackgroundOptions{ConfigPath: usedPath})

	go func() {
		logger.Infof("panrouter %s listening on %s (profile=%s, drivers=%v, config=%s)",
			version, cfg.Server.Listen, cfg.Server.DeployProfile, a.Registry.IDs(), usedPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warnf("http shutdown: %v", err)
	}
	logger.Info("bye")
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
