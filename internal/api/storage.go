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
	out["enabled"] = true
	out["public_base"] = base
	out["dest_dir"] = d.Transfer.DefaultDest()
	out["max_bytes"] = storageMaxBytes(d.Cfg.Get().Transfer.MaxBytes)
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
