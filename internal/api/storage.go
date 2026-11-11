package api

import (
	"math"
	"net/http"
)

// GET /api/v1/storage 返回存储落点(OpenList 兼容端点)的接入信息。
//
// 客户端(桌面端)不内置任何部署域名:它从这里拿到 public_base,再拼成
// `<public_base>/<存储路径>` 交给 /api/v1/resolve 解析。换部署只改服务端配置。
// 未配置端点时回 200 + enabled=false,客户端据此隐藏入口而不是报错。
func (d *Deps) handleStorageInfo(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"code": 200, "driver": "mangodrive", "enabled": false}
	base := ""
	if d.Transfer != nil {
		base = d.Transfer.PublicBase()
	}
	if base == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["enabled"] = true
	out["public_base"] = base
	out["dest_dir"] = d.Transfer.DefaultDest()
	out["max_bytes"] = storageMaxBytes(d.Cfg.Get().Transfer.MaxBytes)
	// limit_qps 下发 mangodrive 驱动限速(取整,0=未配置):客户端文件夹批量
	// 提链的并发度据此自适应(取 1/3 并留裕度),服务端调 QPS 客户端无需改版。
	//
	// ★ 截断方向:必须**向上**取整(ceil)。原实现 int(x) 截断向下:
	//   limit_qps=2.5 → 下发 2 → 客户端算 2/3=0 → 钳到 1 → 保守(安全);
	//   但 limit_qps=0.5 → 下发 0 → 客户端视为「未配置」→ 用默认并发 3,
	//   而服务端实际只允许 0.5 QPS → **客户端并发远超服务端限速**,
	//   表现为批量提链被服务端限流拒绝、大文件夹反复失败。
	//   向上取整保证「下发值 >= 实际限速」,客户端据此只会更保守,不会更激进。
	//   另:ceil 后必须 >= 1,否则同样退化成「未配置」。
	if lc := d.Cfg.Get().Drivers["mangodrive"]; lc.LimitQPS > 0 {
		qps := int(math.Ceil(lc.LimitQPS))
		if qps < 1 {
			qps = 1
		}
		out["limit_qps"] = qps
	}
	writeJSON(w, http.StatusOK, out)
}

// storageMaxBytes 把 transfer.max_bytes 映射成对外契约语义:
// 正值原样;0 = 默认上限;负值(服务端不限)对客户端报 0,表示「不限」。
//
// 不能直接透传负值:客户端(桌面端 StorageInfo)按 `0 = 不限` 理解,
// 负值会被当成具体上限比对,反而把任意文件都判超限 —— 契约两端口径必须一致。
func storageMaxBytes(cfgMax int64) int64 {
	if cfgMax < 0 {
		return 0
	}
	if cfgMax == 0 {
		return defaultStorageMaxBytes
	}
	return cfgMax
}

// defaultStorageMaxBytes 与 service 层的转存上限默认值保持一致(64GiB)。
const defaultStorageMaxBytes int64 = 64 << 30
