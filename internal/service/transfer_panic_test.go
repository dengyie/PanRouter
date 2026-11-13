package service

import (
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/repo"
	"go.uber.org/zap"
)

// P1 契约:Submit 的后台 goroutine 不在 HTTP handler 栈内,api 层 recoverer
// 保护不到——任务执行链(resolver/driver)panic 不许带崩整个进程,必须收口成
// failed 并落日志。用 resolver=nil 的最小装配让 Run 必然 panic(nil 指针解引用)。
func TestTransferSubmitRecoversPanic(t *testing.T) {
	store, err := repo.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	cfg.Transfer.Endpoint = "http://127.0.0.1:1"
	cfg.Transfer.Token = "tok"
	cfgp := &config.Provider{}
	cfgp.Set(cfg)
	tr, err := NewTransfer(cfgp, nil, nil, store, crypto.New("test-key"), metrics.New(), zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	task, err := tr.Submit(TransferRequest{SourceURL: "http://panic.example/s/x"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snap := tr.Get(task.ID); snap != nil && snap.State == stateFailed {
			if snap.Error == "" {
				t.Fatal("panic must be recorded in task error")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach failed within deadline, got %+v", task.ID, tr.Get(task.ID))
}
