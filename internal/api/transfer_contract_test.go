package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/app"
	"github.com/dengyie/panrouter/internal/config"
)

// transferEnv 起一个真实 App(含 transfer 编排),并把上游指派给假的网盘驱动。
func newTransferEnv(t *testing.T, openlistURL string) (*httptest.Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.APIToken = "tok"
	cfg.Auth.JWTSecret = "jwt"
	cfg.Server.DataDir = t.TempDir()
	cfg.Transfer.Endpoint = openlistURL
	cfg.Transfer.Token = "ol-token"

	a, err := app.Build(app.Options{Cfg: cfg, Logger: zap.NewNop().Sugar(), MasterKey: "k", AllowPrivateClients: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	ts := httptest.NewServer(Router(Deps{
		Version: "t", Cfg: a.Cfg, Resolver: a.Resolver, Transfer: a.Transfer, Store: a.Store,
		AES: a.AES, Signer: a.Signer, Log: a.Log, Met: a.Met,
	}))
	t.Cleanup(ts.Close)
	return ts, "tok"
}

func TestTransferContract(t *testing.T) {
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer ol.Close()

	ts, token := newTransferEnv(t, ol.URL)

	// 未知通道 → 400(unsupported)
	st, _ := doRaw(t, ts, token, "POST", "/api/v1/transfer/mangodrive",
		`{"source_url":"https://fake.example/s/x","channel":"bogus"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("bogus channel: status=%d", st)
	}
	// 缺 source_url → 404(not_found)
	st, _ = doRaw(t, ts, token, "POST", "/api/v1/transfer/mangodrive", `{}`)
	if st != http.StatusNotFound {
		t.Fatalf("empty source: status=%d", st)
	}
	// 非 http 链接 → 400(unsupported)
	st, _ = doRaw(t, ts, token, "POST", "/api/v1/transfer/mangodrive", `{"source_url":"quark://x"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("bad scheme: status=%d", st)
	}
	// 无 token → 401
	st, _ = doRawNoAuth(t, ts, "POST", "/api/v1/transfer/mangodrive", `{}`)
	if st != http.StatusUnauthorized {
		t.Fatalf("anon: status=%d", st)
	}
}

func doRaw(t *testing.T, ts *httptest.Server, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	return rawReq(t, ts, token, method, path, body)
}

func doRawNoAuth(t *testing.T, ts *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	return rawReq(t, ts, "", method, path, body)
}
func rawReq(t *testing.T, ts *httptest.Server, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}
