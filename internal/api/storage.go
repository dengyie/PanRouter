package api

import "net/http"

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
	maxBytes := d.Cfg.Get().Transfer.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultStorageMaxBytes
	}
	out["enabled"] = true
	out["public_base"] = base
	out["dest_dir"] = d.Transfer.DefaultDest()
	out["max_bytes"] = maxBytes
	writeJSON(w, http.StatusOK, out)
}

// defaultStorageMaxBytes 与 service 层的转存上限默认值保持一致(64GiB)。
const defaultStorageMaxBytes int64 = 64 << 30
