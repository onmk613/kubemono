// Package pki 提供 kubemono 自研引导机制所需的 CA 与叶子证书管理能力。
// 设计要点：
// - 只依赖标准库 crypto/x509：本包不引入任何 k8s.io 依赖，便于被 phase 复用；
// - 私钥只存在于返回值与落盘文件中，永远不会出现在错误信息、日志或诊断字符串里；
// - 落盘一律使用「同目录临时文件 + rename」的原子写法，证书 0644、私钥 0600，
// 避免组件读到半成品文件；
// - 所有接受 PEM 的函数都对输入做校验（签名、公钥匹配、IsCA），
// 拒绝被篡改或与私钥不匹配的材料。
package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// KeyAlgorithm 是证书私钥的生成算法。
// 取值与 kubemono/internal/config 中的 KeyAlgorithmRSA2048 / KeyAlgorithmECDSAP256
// 完全一致（由 pki 的单元测试锁定），这样 API 配置可以直接喂给本包。
type KeyAlgorithm string

const (
	// RSA2048 是默认算法，对应 clusterconfig.KeyAlgorithmRSA2048。
	RSA2048 KeyAlgorithm = "rsa2048"
	// ECDSAP256 对应 clusterconfig.KeyAlgorithmECDSAP256。
	ECDSAP256 KeyAlgorithm = "ecdsa-p256"
	// DefaultKeyAlgorithm 是未显式配置时使用的算法。
	DefaultKeyAlgorithm = RSA2048
)

// PEM 块类型。
const (
	pemTypeCertificate = "CERTIFICATE"
	pemTypePrivateKey  = "PRIVATE KEY"
	pemTypeRSAPrivate  = "RSA PRIVATE KEY"
	pemTypeECPrivate   = "EC PRIVATE KEY"
	pemTypePublicKey   = "PUBLIC KEY"
)

// String 实现 fmt.Stringer。
func (a KeyAlgorithm) String() string { return string(a) }

// Valid 报告算法是否被本包支持。
func (a KeyAlgorithm) Valid() bool {
	switch a {
	case RSA2048, ECDSAP256:
		return true
	default:
		return false
	}
}

// NewPrivateKey 按算法生成一个新的私钥。
func (a KeyAlgorithm) NewPrivateKey() (crypto.Signer, error) {
	switch a {
	case RSA2048:
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("生成 RSA-2048 私钥失败: %w", err)
		}
		return key, nil
	case ECDSAP256:
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("生成 ECDSA-P256 私钥失败: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("不支持的密钥算法 %q：可选 %s、%s", string(a), RSA2048, ECDSAP256)
	}
}

// KeyAlgorithmOf 返回私钥对应的算法，未知类型返回错误。
func KeyAlgorithmOf(key crypto.Signer) (KeyAlgorithm, error) {
	switch key.(type) {
	case *rsa.PrivateKey:
		return RSA2048, nil
	case *ecdsa.PrivateKey:
		return ECDSAP256, nil
	default:
		return "", fmt.Errorf("无法从 %T 推断密钥算法", key)
	}
}

// EncodeCertPEM 把证书编码成 PEM。cert 为 nil 或缺少 DER 数据时返回 nil。
func EncodeCertPEM(cert *x509.Certificate) []byte {
	if cert == nil || len(cert.Raw) == 0 {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: cert.Raw})
}

// EncodeKeyPEM 把私钥编码成 PKCS#8 PEM（RSA 与 ECDSA 统一格式）。
// 返回值只应写入 0600 的文件；调用方不得把它放进日志或错误信息。
func EncodeKeyPEM(key crypto.Signer) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("私钥为空")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("编码 PKCS#8 私钥失败: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypePrivateKey, Bytes: der}), nil
}

// ParseCertsPEM 解析 PEM 中的所有证书，至少一个才算成功。
// 解析失败时返回的错误只包含 PEM 块类型与下标，不会回显输入内容。
func ParseCertsPEM(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != pemTypeCertificate {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("解析第 %d 个证书失败: %w", len(certs)+1, err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("未找到 %s PEM 块", pemTypeCertificate)
	}
	return certs, nil
}

// ParsePrivateKeyPEM 解析 PEM 中的私钥，支持 PKCS#1、SEC1(EC) 与 PKCS#8。
// 文件里混有证书等其他块时会被跳过；找到第一个可解析的私钥即返回。
func ParsePrivateKeyPEM(data []byte) (crypto.Signer, error) {
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case pemTypeRSAPrivate:
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("解析 PKCS#1 RSA 私钥失败: %w", err)
			}
			return key, nil
		case pemTypeECPrivate:
			key, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("解析 SEC1 EC 私钥失败: %w", err)
			}
			return key, nil
		case pemTypePrivateKey:
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("解析 PKCS#8 私钥失败: %w", err)
			}
			signer, ok := key.(crypto.Signer)
			if !ok {
				return nil, fmt.Errorf("PKCS#8 私钥类型 %T 不支持签名", key)
			}
			switch signer.(type) {
			case *rsa.PrivateKey, *ecdsa.PrivateKey, ed25519.PrivateKey:
				return signer, nil
			default:
				return nil, fmt.Errorf("不支持的私钥类型 %T", key)
			}
		default:
			continue
		}
	}
	return nil, fmt.Errorf("未找到可用的私钥 PEM 块（支持 %s、%s、%s）",
		pemTypePrivateKey, pemTypeRSAPrivate, pemTypeECPrivate)
}

// encodePublicKeyPEM 把公钥编码成 PKIX PEM。
func encodePublicKeyPEM(pub crypto.PublicKey) ([]byte, error) {
	if pub == nil {
		return nil, fmt.Errorf("公钥为空")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("编码公钥失败: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypePublicKey, Bytes: der}), nil
}

// parsePublicKeyPEM 解析 PKIX（"PUBLIC KEY"）或 PKCS#1（"RSA PUBLIC KEY"）公钥。
func parsePublicKeyPEM(data []byte) (crypto.PublicKey, error) {
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case pemTypePublicKey:
			pub, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("解析 PKIX 公钥失败: %w", err)
			}
			return pub, nil
		case "RSA PUBLIC KEY":
			pub, err := x509.ParsePKCS1PublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("解析 PKCS#1 公钥失败: %w", err)
			}
			return pub, nil
		default:
			continue
		}
	}
	return nil, fmt.Errorf("未找到可用的公钥 PEM 块（支持 %s、RSA PUBLIC KEY）", pemTypePublicKey)
}

// publicKeysEqual 比较两个公钥是否相同。
// 优先使用标准库的 Equal 方法（常量时间、区分曲线），
// 类型不支持时退化为 PKIX 编码的字节比较。
func publicKeysEqual(a, b crypto.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}
	type equaler interface {
		Equal(crypto.PublicKey) bool
	}
	if eq, ok := a.(equaler); ok {
		return eq.Equal(b)
	}
	ab, errA := x509.MarshalPKIXPublicKey(a)
	bb, errB := x509.MarshalPKIXPublicKey(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// readParse 读取文件并解析为 T；统一把"读取/解析"两段错误带上路径与中文类别。
func readParse[T any](path, label string, parse func([]byte) (T, error)) (T, error) {
	var zero T
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, fmt.Errorf("读取%s %s 失败: %w", label, path, err)
	}
	v, err := parse(data)
	if err != nil {
		return zero, fmt.Errorf("解析%s %s 失败: %w", label, path, err)
	}
	return v, nil
}

// readCertsFile 读取并解析证书文件。
func readCertsFile(path string) ([]*x509.Certificate, error) {
	return readParse(path, "证书", ParseCertsPEM)
}

// readKeyFile 读取并解析私钥文件。
func readKeyFile(path string) (crypto.Signer, error) {
	return readParse(path, "私钥", ParsePrivateKeyPEM)
}

// readPublicKeyFile 读取并解析公钥文件。
func readPublicKeyFile(path string) (crypto.PublicKey, error) {
	return readParse(path, "公钥", parsePublicKeyPEM)
}
