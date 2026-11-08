package service

import (
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
)

// newCfgTransfer 只装配配置依赖,用于验证「转存参数全部来自 transfer 配置段」。
func newCfgTransfer(cfg *config.Config) *Transfer {
	p := &config.Provider{}
	p.Set(cfg)
	return &Transfer{cfg: p}
}

func TestTransferPolicyComesFromConfig(t *testing.T) {
	cfg := config.Default()
	tr := newCfgTransfer(cfg)

	if got := tr.autoStreamBytes(); got != 8<<30 {
		t.Fatalf("默认 auto_stream_bytes=%d", got)
	}
	if n, d := tr.keepPolicy(); n != 200 || d != 24*time.Hour {
		t.Fatalf("默认保留策略=%d/%v", n, d)
	}

	cfg.Transfer.AutoStreamBytes = 1 << 20
	cfg.Transfer.KeepTasks = 7
	cfg.Transfer.KeepTaskAfter = time.Hour
	tr = newCfgTransfer(cfg)

	if got := tr.autoStreamBytes(); got != 1<<20 {
		t.Fatalf("配置 auto_stream_bytes 未生效:%d", got)
	}
	if n, d := tr.keepPolicy(); n != 7 || d != time.Hour {
		t.Fatalf("配置保留策略未生效:%d/%v", n, d)
	}

	cfg.Transfer.AutoStreamBytes = -1
	tr = newCfgTransfer(cfg)
	if got := tr.autoStreamBytes(); got != 0 {
		t.Fatalf("负阈值应视为 0(全部 offline):%d", got)
	}
}

func TestTransferDestDefaultIsEndpointRootUnlessConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Transfer.DestDir = ""
	tr := newCfgTransfer(cfg)
	if got := tr.destDefault(); got != "/" {
		t.Fatalf("未配置 dest_dir 应为端点根目录:%q", got)
	}

	cfg.Transfer.DestDir = "/dav//inbox/"
	tr = newCfgTransfer(cfg)
	if got := tr.destDefault(); got != "/dav/inbox" {
		t.Fatalf("dest_dir 应归一化:%q", got)
	}
}

func TestTransferPublicBaseResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Transfer.PublicBase = ""
	tr := newCfgTransfer(cfg)
	if got := tr.PublicBase(); got != "https://drive.example.com" {
		t.Fatalf("无 public_base 时应回退 domain_routes:%q", got)
	}

	cfg.Transfer.PublicBase = "pool.example.net/"
	tr = newCfgTransfer(cfg)
	if got := tr.PublicBase(); got != "https://pool.example.net" {
		t.Fatalf("public_base 覆盖未生效:%q", got)
	}

	cfg.Transfer.PublicBase = ""
	cfg.DomainRoutes = nil
	tr = newCfgTransfer(cfg)
	if got := tr.PublicBase(); got != "" {
		t.Fatalf("无任何域名时应为空:%q", got)
	}
}
