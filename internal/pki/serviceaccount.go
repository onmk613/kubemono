package pki

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"fmt"

	"kubemono/internal/util/fsutil"
)

// ServiceAccountKeyBits 是 ServiceAccount 签名密钥的固定长度（2048 位）。
// 不开放配置：所有 master 必须共用同一套 SA 密钥（公钥跨节点共享），
// 改长度只会制造分叉，没有收益。
const ServiceAccountKeyBits = 2048

// GenerateServiceAccountKeyPair 生成 apiserver 使用的 ServiceAccount 密钥对。
// 私钥以 PKCS#8 PEM 写入 keyPath（0600），公钥以 PKIX PEM 写入 pubPath（0644），
// 均为原子写入。两个文件都不参与 TLS，仅用于 SA token 的签名与校验。
func GenerateServiceAccountKeyPair(pubPath, keyPath string) error {
	key, err := rsa.GenerateKey(rand.Reader, ServiceAccountKeyBits)
	if err != nil {
		return fmt.Errorf("生成 ServiceAccount 私钥失败: %w", err)
	}
	keyPEM, err := EncodeKeyPEM(key)
	if err != nil {
		return err
	}
	pubPEM, err := encodePublicKeyPEM(key.Public())
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(keyPath); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(pubPath, pubPEM, 0o644)
}

// LoadServiceAccountKeyPair 读取并校验 ServiceAccount 密钥对，返回私钥。
// 校验：私钥必须是 RSA（apiserver 不接受其他类型的 SA 签名密钥）、
// 公钥文件必须与私钥匹配。错误信息只包含路径，不含密钥内容。
func LoadServiceAccountKeyPair(pubPath, keyPath string) (crypto.Signer, error) {
	signer, err := readKeyFile(keyPath)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := signer.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ServiceAccount 私钥 %s 必须是 RSA（当前为 %T）", keyPath, signer)
	}
	pubData, err := readPublicKeyFile(pubPath)
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(rsaKey.Public(), pubData) {
		return nil, errKeyMismatch("service-account")
	}
	return rsaKey, nil
}
