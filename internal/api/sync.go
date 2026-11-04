package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/repo"
)

// ---- 设备历史同步(M3) ----
//
// 桌面端把收藏/解析历史/下载记录推到服务端做多设备双向同步:
// payload 是客户端语义的不透明 JSON(服务端不解析,两仓零共享代码边界),
// 删除以墓碑(deleted=true)传播,LWW 按客户端时间戳仲裁(见 repo.UpsertSyncItem)。
//
// 协议:
//   POST /api/v1/sync {device_id, items:[{kind,uid,deleted,updated_at,payload}]} → {accepted, skipped, server_time}
//   GET  /api/v1/sync?since=<seq>&limit=500 → {items, last_seq}   (since=0 全量,按 id 升序翻页)

const (
	// SyncMaxBatch 单次推送条目上限(与 resolve/batch 预算同思路:防单请求打爆)
	SyncMaxBatch = 500
	// SyncDefaultPullSize 默认 pull 每页条数
	SyncDefaultPullSize = 500
	// SyncPayloadMaxBytes 单条 payload 上限:收藏/历史/下载记录都是小对象,防异常客户端塞大内容
	SyncPayloadMaxBytes = 16 * 1024
)

// 合法 kind:服务端只认这三种;payload 内容本身不校验(不透明)。
var syncKinds = map[string]bool{"bookmark": true, "link_history": true, "download_record": true}

// maxAPIBodyBytes 写端点请求体上限:sync 合法上限 ≈500 条×16KB ≈ 8MB,留余量;
// 解码是全量缓冲,不给失控/恶意客户端无上限灌内存的口子。
const maxAPIBodyBytes = 10 << 20

// decodeBoundedBody 施加字节上限后解码 JSON 请求体。
// 超限按内容拒绝(KindUnsupported→400,客户端识别为毒丸批可丢弃收敛);
// 其余解码失败保持「请求格式错误」原语义。返回 false 时响应已写出。
func decodeBoundedBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, driver.NewErr(driver.KindUnsupported, "请求体超限", err))
		} else {
			writeErr(w, driver.NewErr(driver.KindNotFound, "请求格式错误", err))
		}
		return false
	}
	return true
}

type syncPushReq struct {
	DeviceID string      `json:"device_id"`
	Items    []syncEntry `json:"items"`
}

type syncEntry struct {
	Kind      string          `json:"kind"`
	UID       string          `json:"uid"`
	Deleted   bool            `json:"deleted"`
	UpdatedAt int64           `json:"updated_at"` // 客户端时钟毫秒(LWW 仲裁键)
	Payload   json.RawMessage `json:"payload"`
}

type syncItemView struct {
	ID              uint      `json:"id"`
	Kind            string    `json:"kind"`
	UID             string    `json:"uid"`
	DeviceID        string    `json:"device_id"`
	Payload         string    `json:"payload"`
	Deleted         bool      `json:"deleted"`
	ClientUpdatedAt int64     `json:"client_updated_at"`
	ServerUpdatedAt time.Time `json:"server_updated_at"`
}

// handlePushSync POST /api/v1/sync
func (d *Deps) handlePushSync(w http.ResponseWriter, r *http.Request) {
	var q syncPushReq
	if !decodeBoundedBody(w, r, &q) {
		return
	}
	if q.DeviceID == "" || len(q.Items) == 0 {
		writeErr(w, driver.NewErr(driver.KindUnsupported, "device_id 与 items 不能为空", nil))
		return
	}
	if len(q.Items) > SyncMaxBatch {
		writeErr(w, driver.NewErr(driver.KindUnsupported, "items 数量须在 1-"+strconv.Itoa(SyncMaxBatch), nil))
		return
	}
	accepted, skipped := 0, 0
	for i := range q.Items {
		e := &q.Items[i]
		if !syncKinds[e.Kind] || e.UID == "" {
			writeErr(w, driver.NewErr(driver.KindUnsupported, "items["+strconv.Itoa(i)+"] kind/uid 非法", nil))
			return
		}
		if len(e.Payload) > SyncPayloadMaxBytes {
			writeErr(w, driver.NewErr(driver.KindUnsupported, "items["+strconv.Itoa(i)+"] payload 超限", nil))
			return
		}
		payload := string(e.Payload)
		if payload == "null" {
			payload = ""
		}
		item := repo.SyncItem{
			Kind: e.Kind, UID: e.UID, DeviceID: q.DeviceID,
			Payload: payload, Deleted: e.Deleted, ClientUpdatedAt: e.UpdatedAt,
		}
		applied, err := d.Store.UpsertSyncItem(&item)
		if err != nil {
			writeErr(w, driver.NewErr(driver.KindUpstream, "同步落库失败", err))
			return
		}
		if applied {
			accepted++
		} else {
			skipped++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": accepted, "skipped": skipped, "server_time": time.Now().UnixMilli(),
	})
}

// handlePullSync GET /api/v1/sync?since=&limit=
func (d *Deps) handlePullSync(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := strconv.ParseUint(q.Get("since"), 10, 64)
	if err != nil {
		writeErr(w, driver.NewErr(driver.KindUnsupported, "since 参数非法", nil))
		return
	}
	limit := SyncDefaultPullSize
	if v := q.Get("limit"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n <= 0 || n > SyncMaxBatch {
			writeErr(w, driver.NewErr(driver.KindUnsupported, "limit 参数非法(1-"+strconv.Itoa(SyncMaxBatch)+")", nil))
			return
		}
		limit = n
	}
	rows, err := d.Store.ListSyncItemsAfter(uint(since), limit)
	if err != nil {
		writeErr(w, driver.NewErr(driver.KindUpstream, "查询同步数据失败", err))
		return
	}
	items := make([]syncItemView, 0, len(rows))
	lastSeq := since
	for i := range rows {
		items = append(items, syncItemView{
			ID: rows[i].ID, Kind: rows[i].Kind, UID: rows[i].UID, DeviceID: rows[i].DeviceID,
			Payload: rows[i].Payload, Deleted: rows[i].Deleted,
			ClientUpdatedAt: rows[i].ClientUpdatedAt, ServerUpdatedAt: rows[i].ServerUpdatedAt,
		})
		if uint64(rows[i].ID) > lastSeq {
			lastSeq = uint64(rows[i].ID)
		}
	}
	// 空页仍回报库内真实最大序号:库从备份回档后,客户端游标可能停在比真实最大值更靠前的位置,
	// 若此处只回显 since,该设备会认为「已追平」而永久脱离同步(只能清设置恢复)。
	if len(rows) == 0 && lastSeq == since {
		if maxID, e := d.Store.MaxSyncItemID(); e != nil {
			d.Log.Warnw("同步空页回退查询失败,客户端游标保持不变", "err", e)
		} else if uint64(maxID) < since {
			lastSeq = uint64(maxID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "last_seq": lastSeq})
}
