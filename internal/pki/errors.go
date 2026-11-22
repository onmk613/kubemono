package pki

import (
	"errors"
	"fmt"
	"time"
)

// 可判定的哨兵错误。调用方用 errors.Is 判断，不需要解析错误文本。
var (
	// ErrNoCA 表示 CA 对象不完整（缺少证书或私钥）。
	ErrNoCA = errors.New("CA 材料不完整")
	// ErrNoCertificate 表示缺少证书。
	ErrNoCertificate = errors.New("缺少证书")
	// ErrNoPrivateKey 表示缺少私钥。
	ErrNoPrivateKey = errors.New("缺少私钥")
	// ErrNoPublicKey 表示缺少公钥。
	ErrNoPublicKey = errors.New("缺少公钥")
	// ErrKeyMismatch 表示证书公钥与私钥不匹配。
	ErrKeyMismatch = errors.New("证书与私钥不匹配")
	// ErrNotCA 表示证书不是 CA。
	ErrNotCA = errors.New("证书不是 CA")
	// ErrCertExpired 表示证书已过期。
	ErrCertExpired = errors.New("证书已过期")
	// ErrUnknownAlgorithm 表示配置了不支持的密钥算法。
	ErrUnknownAlgorithm = errors.New("不支持的密钥算法")
	// ErrSigningFailed 表示 x509 签名失败。
	ErrSigningFailed = errors.New("签发证书失败")
)

// 以下函数把上下文拼进哨兵错误。所有错误只包含路径、CN 与算法名，
// 绝不包含私钥字节或 PEM 内容。

func errEmptyCommonName() error {
	return errors.New("证书 CN 不能为空")
}

func errUnsupportedAlgorithm(alg string) error {
	return fmt.Errorf("%w: %q", ErrUnknownAlgorithm, alg)
}

func errSignFailed(err error) error {
	return fmt.Errorf("%w: %v", ErrSigningFailed, err)
}

func errSerialFailed(err error) error {
	return fmt.Errorf("生成证书序列号失败: %w", err)
}

func errNotCA(cn string) error {
	return fmt.Errorf("%w: %q", ErrNotCA, cn)
}

func errKeyMismatch(cn string) error {
	return fmt.Errorf("%w: %q 的证书与私钥不是一对", ErrKeyMismatch, cn)
}

func errCAExpired(cn string, notAfter time.Time) error {
	return fmt.Errorf("%w: CA %q 已于 %s 过期", ErrCertExpired, cn, notAfter.Format(time.RFC3339))
}
