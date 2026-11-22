package pki

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"kubemono/internal/util/stringsx"
	"math/big"
	"net"
	"sort"
	"strings"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// clockSkew 是签发证书时向前回溯的时钟偏移，避免刚签发的证书被
// 时钟略快的节点判为「尚未生效」。
const clockSkew = 5 * time.Minute

// 证书用途预设。未显式指定时使用 UsageServerClient，覆盖多数控制面证书的握手需求。
var (
	// UsageServer 是服务端证书用途。
	UsageServer = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	// UsageClient 是客户端证书用途。
	UsageClient = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	// UsageServerClient 同时具备服务端与客户端用途。
	UsageServerClient = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
)

// CertConfig 描述一张待签发叶子证书。
// 零值下：PublicKey 为空表示由 Sign 现场生成新私钥；Validity 为空取
// constants.DefaultCertTTL；Usages 为空取 UsageServerClient。
type CertConfig struct {
	// CommonName 是证书 CN，必填。
	CommonName string
	// Organizations 是证书 O，对应 k8s 的 group（如 kubemono:cluster-admins）。
	Organizations []string
	// DNSNames 和 IPAddresses 是 SAN；渲染前会去重排序。
	DNSNames    []string
	IPAddresses []net.IP
	// Usages 是扩展密钥用途。
	Usages []x509.ExtKeyUsage
	// Validity 是有效期，从签发时刻算起。
	Validity time.Duration
	// PublicKey 非空时用它签证书（调用方自己保管私钥，此时 Sign 返回的
	// crypto.Signer 为 nil）；为空时由 Sign 生成新私钥并返回。
	PublicKey crypto.PublicKey
}

// normalizeDNSNames 去重（大小写不敏感，保留首次出现的写法）并排序。
func normalizeDNSNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i]), strings.ToLower(out[j])
		if li == lj {
			return out[i] < out[j]
		}
		return li < lj
	})
	return out
}

// normalizeIPs 归一化（IPv4 收敛成 4 字节）、去重并排序。
func normalizeIPs(ips []net.IP) []net.IP {
	seen := make(map[string]net.IP, len(ips))
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		norm := ip.To4()
		if norm == nil {
			norm = ip.To16()
		}
		if norm == nil {
			continue
		}
		seen[norm.String()] = norm
	}
	out := make([]net.IP, 0, len(seen))
	for _, ip := range seen {
		out = append(out, ip)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// CA 是自签的证书颁发机构。
type CA struct {
	// Cert 是 CA 证书，IsCA 为 true。
	Cert *x509.Certificate
	// Key 是 CA 私钥；不要在错误信息或日志中输出它。
	Key crypto.Signer
	// Chain 是证书链：Chain[0] 是签发 CA，后续是中间/根证书（可选）。
	Chain []*x509.Certificate
}

// NewCA 生成一个自签 CA。alg 为空时用默认算法，validity 为空时取
// constants.DefaultCATTL（10 年）。
func NewCA(cn string, alg KeyAlgorithm, validity time.Duration) (*CA, error) {
	if strings.TrimSpace(cn) == "" {
		return nil, errEmptyCommonName()
	}
	if alg == "" {
		alg = DefaultKeyAlgorithm
	}
	if !alg.Valid() {
		return nil, errUnsupportedAlgorithm(string(alg))
	}
	if validity <= 0 {
		validity = constants.DefaultCATTL
	}
	key, err := alg.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}
	now := time.Now().Truncate(time.Second)
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, errSignFailed(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errSignFailed(err)
	}
	return &CA{Cert: cert, Key: key, Chain: []*x509.Certificate{cert}}, nil
}

// LoadCA 从磁盘读取 CA 证书与私钥，并校验：私钥与证书公钥匹配、证书是 CA。
// certPath 可以包含证书链：第一张是签发 CA，后续是中间/根证书；整条链保存在
// CA.Chain 中，WriteToDisk 会原样写回。
// CA 已过期时返回错误：继续用它签发的叶子证书会被组件拒绝。
func LoadCA(certPath, keyPath string) (*CA, error) {
	certs, err := readCertsFile(certPath)
	if err != nil {
		return nil, err
	}
	key, err := readKeyFile(keyPath)
	if err != nil {
		return nil, err
	}
	return newCAFromParts(certs, key)
}

// LoadCAIfExists 加载已有 CA，**不应用叶子证书的续期窗口**（叶子证书的 30 天
// 阈值不适用于 CA）。
// 返回值语义：
// - 两个文件都不存在：(nil, false, nil)，调用方可以生成新 CA；
// - 只存在一个，或内容不可用/已过期：(nil, true, err)，调用方必须报错，
// **绝不能**因此重建 CA——那会让既有证书与信任链全部失效；
// - 可用：(ca, true, nil)。
func LoadCAIfExists(certPath, keyPath string) (*CA, bool, error) {
	certExists := FileExists(certPath)
	keyExists := FileExists(keyPath)
	if !certExists && !keyExists {
		return nil, false, nil
	}
	if !certExists || !keyExists {
		return nil, true, fmt.Errorf(
			"CA 材料不完整：%s 存在=%t，%s 存在=%t；为避免破坏既有信任链，kubemono 不会自动重建 CA",
			certPath, certExists, keyPath, keyExists)
	}
	ca, err := LoadCA(certPath, keyPath)
	if err != nil {
		return nil, true, fmt.Errorf("已有 CA 不可用（拒绝自动重建，避免破坏集群信任链）: %w", err)
	}
	return ca, true, nil
}

// CAExists 报告是否存在可用的 CA：两个文件都存在、私钥与证书匹配、证书是 CA
// 且尚未过期。它刻意不应用叶子证书的续期窗口。
func CAExists(certPath, keyPath string) bool {
	ca, present, err := LoadCAIfExists(certPath, keyPath)
	return present && err == nil && ca != nil
}

// LoadCertificate 从 PEM 文件读取第一张证书。
// 用于复用既有叶子证书前的身份/签发者校验；只返回第一张证书。
func LoadCertificate(path string) (*x509.Certificate, error) {
	certs, err := readCertsFile(path)
	if err != nil {
		return nil, err
	}
	return certs[0], nil
}

func newCAFromParts(certs []*x509.Certificate, key crypto.Signer) (*CA, error) {
	if len(certs) == 0 || certs[0] == nil {
		return nil, ErrNoCertificate
	}
	cert := certs[0]
	if key == nil {
		return nil, ErrNoPrivateKey
	}
	if !cert.IsCA {
		return nil, errNotCA(cert.Subject.CommonName)
	}
	if !publicKeysEqual(cert.PublicKey, key.Public()) {
		return nil, errKeyMismatch(cert.Subject.CommonName)
	}
	if !time.Now().Before(cert.NotAfter) {
		return nil, errCAExpired(cert.Subject.CommonName, cert.NotAfter)
	}
	return &CA{Cert: cert, Key: key, Chain: certs}, nil
}

// WriteToDisk 原子地把 CA 证书（0644）与私钥（0600）写入磁盘。
// 父目录按 0755 创建；即使目标文件已存在也不会出现半截内容。
func (ca *CA) WriteToDisk(certPath, keyPath string) error {
	if ca == nil {
		return ErrNoCA
	}
	if ca.Cert == nil {
		return ErrNoCertificate
	}
	if ca.Key == nil {
		return ErrNoPrivateKey
	}
	chain := ca.Chain
	if len(chain) == 0 {
		chain = []*x509.Certificate{ca.Cert}
	}
	var certPEM []byte
	for _, cert := range chain {
		pem := EncodeCertPEM(cert)
		if pem == nil {
			return ErrNoCertificate
		}
		certPEM = append(certPEM, pem...)
	}
	if len(certPEM) == 0 {
		return ErrNoCertificate
	}
	keyPEM, err := EncodeKeyPEM(ca.Key)
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(certPath); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(certPath, certPEM, 0o644); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

// Sign 用 CA 签发一张叶子证书。
// cfg.PublicKey 为空时现场生成新私钥并作为第二个返回值返回；
// cfg.PublicKey 非空时第二个返回值为 nil（私钥由调用方保管）。
// 除 CA 校验外，本函数不接受任何外部提供的 x509 模板，因此不可能签出 CA 证书。
func (ca *CA) Sign(cfg CertConfig) (*x509.Certificate, crypto.Signer, error) {
	if ca == nil || ca.Cert == nil || ca.Key == nil {
		return nil, nil, ErrNoCA
	}
	var (
		signer crypto.Signer
		pub    = cfg.PublicKey
	)
	if pub == nil {
		alg, err := ca.leafKeyAlgorithm()
		if err != nil {
			return nil, nil, err
		}
		key, err := alg.NewPrivateKey()
		if err != nil {
			return nil, nil, err
		}
		signer, pub = key, key.Public()
	}
	cert, err := ca.issue(cfg, pub)
	if err != nil {
		return nil, nil, err
	}
	return cert, signer, nil
}

// issue 组装模板并签名。CSR 的 extension 一律不复制，只取其公钥。
func (ca *CA) issue(cfg CertConfig, pub crypto.PublicKey) (*x509.Certificate, error) {
	if strings.TrimSpace(cfg.CommonName) == "" {
		return nil, errEmptyCommonName()
	}
	if pub == nil {
		return nil, ErrNoPublicKey
	}
	validity := cfg.Validity
	if validity <= 0 {
		validity = constants.DefaultCertTTL
	}
	usages := cfg.Usages
	if len(usages) == 0 {
		usages = UsageServerClient
	}
	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}
	keyUsage := x509.KeyUsageDigitalSignature
	if _, ok := pub.(*rsa.PublicKey); ok {
		keyUsage |= x509.KeyUsageKeyEncipherment
	}
	now := time.Now().Truncate(time.Second)
	notAfter := now.Add(validity)
	if notAfter.After(ca.Cert.NotAfter) {
		return nil, fmt.Errorf(
			"签发 %s 失败：有效期至 %s 超过 CA 到期时间 %s；请缩短 certValidity 或先轮换 CA",
			cfg.CommonName, notAfter.UTC().Format(time.RFC3339), ca.Cert.NotAfter.UTC().Format(time.RFC3339))
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cfg.CommonName, Organization: stringsx.DedupSorted(cfg.Organizations)},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              notAfter,
		KeyUsage:              keyUsage,
		ExtKeyUsage:           append([]x509.ExtKeyUsage(nil), usages...),
		DNSNames:              normalizeDNSNames(cfg.DNSNames),
		IPAddresses:           normalizeIPs(cfg.IPAddresses),
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, errSignFailed(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errSignFailed(err)
	}
	return cert, nil
}

// leafKeyAlgorithm 返回 CA 私钥对应的算法，用于给新叶子生成同族密钥。
func (ca *CA) leafKeyAlgorithm() (KeyAlgorithm, error) {
	alg, err := KeyAlgorithmOf(ca.Key)
	if err != nil {
		return "", fmt.Errorf("无法确定 CA 私钥算法: %w", err)
	}
	return alg, nil
}

// newSerialNumber 生成 128 位随机序列号。
func newSerialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, errSerialFailed(err)
	}
	if serial.Sign() == 0 {
		serial = big.NewInt(1)
	}
	return serial, nil
}
