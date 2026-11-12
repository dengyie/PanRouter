package app

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/repo"
)

// 回归:Build 必须立刻初始化 account_status gauge。
// 否则进程重启后要等看门狗(默认 30min)或首个账号事件,/metrics 里
// 账号状态序列缺失——对监控来说「没有序列」与「全部为 0」不可区分。
func TestBuildInitializesAccountStatusGauge(t *testing.T) {
	dir := t.TempDir()
	aes := crypto.New("k")
	st, err := repo.Open(filepath.Join(dir, "panrouter.db"))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := aes.EncryptBytes([]byte("c=1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAccount(&repo.Account{
		PanType: "quark", Name: "preset", CredEnc: enc, Status: "ok", CredVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Server.DataDir = dir
	cfg.Auth.JWTSecret = "test-secret"
	a, err := Build(Options{Cfg: cfg, Logger: zap.NewNop().Sugar(), MasterKey: "k"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() { _ = a.Close() }()

	rr := httptest.NewRecorder()
	a.Met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `panrouter_account_status{pan="quark",status="ok"} 1`) {
		t.Fatalf("account_status gauge 未在 Build 时初始化:\n%s", body)
	}
}
