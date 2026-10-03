// Package sign 提供下载链接的 HMAC 签名(带过期时间)。
// 签名只校验"建立连接"时是否有效,Range 续传复用同一 URL,不中断长下载。
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid signature")

type Signer struct {
	key []byte
}

func New(key string) *Signer {
	return &Signer{key: []byte(key)}
}

func (s *Signer) mac(payload string, exp int64) []byte {
	h := hmac.New(sha256.New, s.key)
	fmt.Fprintf(h, "%s|%d", payload, exp)
	return h.Sum(nil)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Sign 返回 "exp.sig" 形式的 token。
func (s *Signer) Sign(payload string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	return b64url([]byte(strconv.FormatInt(exp, 10))) + "." + b64url(s.mac(payload, exp))
}

// Verify 校验 payload 的签名与有效期。
func (s *Signer) Verify(payload, token string) error {
	expB64, sigB64, ok := strings.Cut(token, ".")
	if !ok || expB64 == "" || sigB64 == "" {
		return ErrInvalid
	}
	expBytes, err := base64.RawURLEncoding.DecodeString(expB64)
	if err != nil {
		return ErrInvalid
	}
	exp, err := strconv.ParseInt(string(expBytes), 10, 64)
	if err != nil {
		return ErrInvalid
	}
	if time.Now().Unix() > exp {
		return fmt.Errorf("%w: expired", ErrInvalid)
	}
	want := b64url(s.mac(payload, exp))
	if !hmac.Equal([]byte(want), []byte(sigB64)) {
		return ErrInvalid
	}
	return nil
}
