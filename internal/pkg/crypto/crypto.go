// Package crypto 提供凭据的 AES-GCM 加密落盘能力。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

type AES struct {
	key []byte
}

// New 用任意长度的主密钥派生 32 字节 AES-256 密钥。
func New(masterKey string) *AES {
	k := sha256.Sum256([]byte(masterKey))
	return &AES{key: k[:]}
}

func (a *AES) encrypt(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(a.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func (a *AES) decrypt(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(a.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
}

// Encrypt 返回 Base64 编码的密文。
func (a *AES) Encrypt(plain string) (string, error) {
	b, err := a.encrypt([]byte(plain))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func (a *AES) Decrypt(encoded string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("bad ciphertext encoding: %w", err)
	}
	plain, err := a.decrypt(b)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// EncryptBytes / DecryptBytes 用于 BLOB 字段。
func (a *AES) EncryptBytes(plain []byte) ([]byte, error) { return a.encrypt(plain) }
func (a *AES) DecryptBytes(data []byte) ([]byte, error)  { return a.decrypt(data) }
