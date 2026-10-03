package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dengyie/panrouter/internal/driver"
)

// M1 自实现的 HS256 JWT(header.payload.signature),足够单管理员会话使用。

type jwtClaims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mintJWT(secret, sub string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	head := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(jwtClaims{Sub: sub, Exp: exp})
	payload := head + "." + b64url(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return payload + "." + b64url(mac.Sum(nil))
}

func verifyJWT(secret, token string) (*jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("bad token shape")
	}
	payload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	want := b64url(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return nil, errors.New("bad signature")
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c jwtClaims
	if err := json.Unmarshal(claimsBytes, &c); err != nil {
		return nil, err
	}
	if time.Now().Unix() > c.Exp {
		return nil, fmt.Errorf("token expired at %d", c.Exp)
	}
	return &c, nil
}

// handleLogin POST /api/v1/auth/login(公开端点)。
func (d *Deps) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "请求格式错误", err))
		return
	}
	cfg := d.Cfg.Get()
	passOK := false
	if strings.HasPrefix(cfg.Auth.PasswordBcrypt, "$2") {
		passOK = bcrypt.CompareHashAndPassword([]byte(cfg.Auth.PasswordBcrypt), []byte(req.Password)) == nil
	} else if cfg.Auth.PasswordBcrypt != "" {
		// 兼容明文配置(自用便利);每次登录打警告提醒改用 bcrypt
		passOK = req.Password == cfg.Auth.PasswordBcrypt
		d.Log.Warn("auth.password_bcrypt 不是 bcrypt 哈希,当前按明文比对——请尽快更换")
	}
	if req.Username != cfg.Auth.Username || !passOK {
		d.Store.AddAudit("login_failed", "username="+req.Username)
		writeErr(w, driver.NewErr(driver.KindAuthExpired, "用户名或密码错误", nil))
		return
	}
	ttl := 7 * 24 * time.Hour
	token := mintJWT(cfg.Auth.JWTSecret, req.Username, ttl)
	d.Store.AddAudit("login_ok", "username="+req.Username)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_in": int(ttl.Seconds())})
}
