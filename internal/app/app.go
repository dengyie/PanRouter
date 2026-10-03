// Package app 负责依赖装配:main 与端到端测试共用同一套组装逻辑,
// 避免"测试装配与生产装配漂移"这类问题(历史上曾因此出现双 metrics 实例)。
package app

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/driver/lanzou"
	"github.com/dengyie/panrouter/internal/driver/quark"
	"github.com/dengyie/panrouter/internal/pkg/breaker"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"github.com/dengyie/panrouter/internal/pkg/limiter"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/pkg/sign"
	"github.com/dengyie/panrouter/internal/repo"
	"github.com/dengyie/panrouter/internal/service"
)

// driverFactories:网盘 ID → 构造函数。新增网盘 = 新 driver 包 + 此处一个条目 + config 一个键。
var driverFactories = map[string]func(*httpx.Client) driver.Driver{
	"quark":  func(c *httpx.Client) driver.Driver { return quark.New(c, "", "") },
	"lanzou": func(c *httpx.Client) driver.Driver { return lanzou.New(c) },
}

type Options struct {
	Cfg       *config.Config
	Logger    *zap.SugaredLogger
	MasterKey string // 凭据加密主密钥(main 从环境变量解析;测试显式传入)
	WebFS     fs.FS  // 前端静态资源,可为 nil
	Version   string
	// AllowPrivateClients 仅供测试:客户端允许直连私网地址(e2e 的 mock 上游在 127.0.0.1)。
	// 生产必须为 false,SSRF 防护依赖该默认值。
	AllowPrivateClients bool
	// ExtraDrivers:调用方注入的额外 driver(测试的 fake driver;或未来不随内核发布的插件),
	// 不经过 driverFactories,也不分配内置 HTTP 客户端。
	ExtraDrivers []ExtraDriver
}

// ExtraDriver 是注入式 driver 及其(可选的)独立客户端;
// 客户端为 nil 表示该 driver 不需要对应能力(如纯解析的 fake)。
type ExtraDriver struct {
	Driver       driver.Driver
	APIClient    *httpx.Client
	StreamClient *httpx.Client
}

// App 是装配完成的全部运行时依赖。
type App struct {
	Cfg      *config.Provider
	Store    *repo.Store
	Registry *driver.Registry
	Resolver *service.Resolver
	Accounts *service.AccountService
	Relay    *service.Relay
	Aria2    *service.Aria2
	AES      *crypto.AES
	Signer   *sign.Signer
	Met      *metrics.Registry
	Limiter  *limiter.Limiter
	Log      *zap.SugaredLogger

	apiClients    map[string]*httpx.Client
	streamClients map[string]*httpx.Client
}

// Build 按 config 装配全部依赖。中途失败时负责释放已建资源。
func Build(o Options) (*App, error) {
	cfg := o.Cfg
	aes := crypto.New(o.MasterKey)

	if err := os.MkdirAll(cfg.Server.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	store, err := repo.Open(filepath.Join(cfg.Server.DataDir, "panrouter.db"))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	lim := limiter.New()
	brs := breaker.NewRegistry(5, 10*time.Minute)
	signer := sign.New(cfg.Auth.JWTSecret + "|panrouter-sign")
	met := metrics.New()

	newClient := func(c config.DriverCommon, stream bool) (*httpx.Client, error) {
		return httpx.New(httpx.Options{
			Timeout: 30 * time.Second, NoBodyTimeout: stream,
			Proxy: c.Proxy, RedirectAllow: c.RedirectAllow,
			AllowPrivate: o.AllowPrivateClients,
		})
	}

	// 每个 driver 两个客户端:API 调用受总超时约束;中转流不限体传输时长(设计文档 §7)
	apiClients := map[string]*httpx.Client{}
	streamClients := map[string]*httpx.Client{}
	var driversList []driver.Driver
	ids := make([]string, 0, len(cfg.Drivers))
	for id := range cfg.Drivers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		dc := cfg.Drivers[id]
		if !dc.Enabled {
			continue
		}
		factory, ok := driverFactories[id]
		if !ok {
			o.Logger.Warnf("config 中存在未知网盘 %q,已跳过", id)
			continue
		}
		apiCl, err := newClient(dc, false)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("http client %s: %w", id, err)
		}
		streamCl, err := newClient(dc, true)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("stream client %s: %w", id, err)
		}
		apiClients[id], streamClients[id] = apiCl, streamCl
		driversList = append(driversList, factory(apiCl))
	}
	for _, ex := range o.ExtraDrivers {
		id := ex.Driver.ID()
		driversList = append(driversList, ex.Driver)
		if ex.APIClient != nil {
			apiClients[id] = ex.APIClient
		}
		if ex.StreamClient != nil {
			streamClients[id] = ex.StreamClient
		}
	}
	reg := driver.NewRegistry(driversList, cfg.DomainRoutes)

	cfgp := &config.Provider{}
	cfgp.Set(cfg)

	accSvc := service.NewAccountService(store, aes, o.Logger)
	resolver := service.NewResolver(cfgp, reg, store, aes, accSvc, lim, brs, signer, met, o.Logger)
	relay := service.NewRelay(resolver, streamClients, store, aes, o.Logger)
	aria2, err := service.NewAria2(cfgp, signer, resolver, store, aes, met, o.Logger)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("aria2 client: %w", err)
	}

	return &App{
		Cfg: cfgp, Store: store, Registry: reg, Resolver: resolver, Accounts: accSvc,
		Relay: relay, Aria2: aria2, AES: aes, Signer: signer, Met: met, Limiter: lim,
		Log: o.Logger, apiClients: apiClients, streamClients: streamClients,
	}, nil
}

// ApplyConfig 应用热加载的新配置:限频、部署画像、域名路由即时生效;
// 代理与重定向白名单在客户端构建时固化,变更需重启(设计文档 §7.11)。
func (a *App) ApplyConfig(nc *config.Config) {
	a.Cfg.Set(nc)
	for id, dc := range nc.Drivers {
		a.Limiter.SetRate(id, dc.LimitQPS)
	}
	a.Registry.UpdateRoutes(nc.DomainRoutes)
}

// Close 释放资源(测试与优雅停机)。
func (a *App) Close() error { return a.Store.Close() }
