package certs

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// KeySize 是 certificateKey 的字节数（256 位）。
const KeySize = 32

// aad 把密文绑定到协议版本，避免跨版本重放。
var aad = []byte("kubemono-certs/v1")

// GenerateKey 生成一个新的 certificateKey，返回裸字节与 base64 编码。
// 裸字节只用于加密；base64 是 JoinSecret.CertificateKey 的内部编码。
func GenerateKey() ([]byte, string, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, "", fmt.Errorf("join-certs: 生成 certificateKey 失败: %w", err)
	}
	return key, base64.StdEncoding.EncodeToString(key), nil
}

// DecodeKey 解码 JoinSecret.CertificateKey 的 base64 形式。
func DecodeKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("join-certs: certificateKey 不是合法的 base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("join-certs: certificateKey 解码后为 %d 字节，要求 %d 字节", len(key), KeySize)
	}
	return key, nil
}

// KeyFingerprint 返回 certificateKey 的 SHA-256 指纹（十六进制），用于判断
// 集群里的证书包是否由当前 key 加密，而不泄露 key 本身。
func KeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

// Encrypt 用 certificateKey 加密证书包（AES-256-GCM），返回 nonce||密文。
func Encrypt(bundle *Bundle, key []byte) ([]byte, error) {
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("join-certs: certificateKey 长度为 %d 字节，要求 %d", len(key), KeySize)
	}
	plaintext, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("join-certs: 序列化证书包失败: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("join-certs: 生成 nonce 失败: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// Decrypt 用 certificateKey 解密 nonce||密文。
func Decrypt(data, key []byte) (*Bundle, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("join-certs: certificateKey 长度为 %d 字节，要求 %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, fmt.Errorf("%w: 密文过短", ErrMalformed)
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("join-certs: 解密失败（certificateKey 不匹配或密文被篡改）: %w", err)
	}
	var bundle Bundle
	if err := json.Unmarshal(plaintext, &bundle); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return &bundle, nil
}
