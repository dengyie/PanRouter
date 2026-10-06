// 生命周期:后台任务(凭据看门狗、配置热加载)归 App 编排,main 只传参;
// Shutdown 按序收口——停后台并等待协程退出,再关 Store,杜绝"协程用库时库已关"。
package app

import (
	"context"
	"sync"
	"time"

	"github.com/dengyie/panrouter/internal/config"
)

// 后台任务节奏。看门狗 30min 与热加载 5s 为生产值;测试经 StartBackground 的
// 选项覆盖,不睡眠等待。
const (
	defaultWatchdogInterval = 30 * time.Minute
	defaultReloadInterval   = 5 * time.Second
)

// BackgroundOptions 允许测试收紧节奏并注入热加载的配置文件路径(空 = 不热加载)。
type BackgroundOptions struct {
	WatchdogInterval time.Duration // ≤0 取默认 30min
	ReloadInterval   time.Duration // ≤0 取默认 5s
	ConfigPath       string
}

// StartBackground 启动凭据看门狗与配置热加载协程;返回的 stop 取消两者并
// 等待其退出(不关 Store,资源关闭由 Shutdown 统一排序)。重复调用安全,
// 并记录到 App 供 Shutdown 兜底(main 忘调 stop 时仍能优雅收口)。
func (a *App) StartBackground(opts BackgroundOptions) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wd := opts.WatchdogInterval
	if wd <= 0 {
		wd = defaultWatchdogInterval
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.watchdogLoop(ctx, wd)
	}()

	ri := opts.ReloadInterval
	if ri <= 0 {
		ri = defaultReloadInterval
	}
	if opts.ConfigPath != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.reloadLoop(ctx, ri, opts.ConfigPath)
		}()
	}

	once := sync.Once{}
	stopFn := func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
	a.stopBg = stopFn
	return stopFn
}

// watchdogLoop 周期校验全部账号凭据;单账号失败不中断整轮。
func (a *App) watchdogLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			accounts, err := a.Store.ListAccounts("")
			if err != nil {
				a.Log.Warnf("watchdog list accounts: %v", err)
				continue
			}
			for i := range accounts {
				if ctx.Err() != nil {
					return
				}
				drv, ok := a.Registry.Get(accounts[i].PanType)
				if !ok {
					continue
				}
				if _, err := a.Accounts.Check(ctx, accounts[i].ID, drv); err != nil {
					a.Log.Warnf("watchdog check account=%d: %v", accounts[i].ID, err)
				}
			}
		}
	}
}

// reloadLoop 周期热加载配置;失败保留旧配置(单点日志,不打断循环)。
func (a *App) reloadLoop(ctx context.Context, interval time.Duration, path string) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			nc, _, err := config.LoadReload(path, a.Cfg.Get())
			if err != nil {
				a.Log.Warnf("config 热加载失败(保留旧配置): %v", err)
				continue
			}
			a.ApplyConfig(nc)
		}
	}
}

// Shutdown 优雅停机:停后台任务并等待退出,再关闭 Store。
// ctx 仅约束等待上限——超时仍会继续关闭 Store,避免进程挂死。
func (a *App) Shutdown(ctx context.Context) error {
	if a.stopBg != nil {
		a.stopBg()
	}
	return a.Store.Close()
}
