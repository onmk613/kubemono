package pki

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// CertInfo 是一张证书的可读信息。
type CertInfo struct {
	Path        string    `json:"path"`
	Index       int       `json:"index"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	SelfSigned  bool      `json:"selfSigned"`
	Serial      string    `json:"serial"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	IsCA        bool      `json:"isCA"`
	PathLen     int       `json:"pathLen"`
	PathLenZero bool      `json:"pathLenZero"`
	KeyUsage    []string  `json:"keyUsage,omitempty"`
	ExtKeyUsage []string  `json:"extKeyUsage,omitempty"`
	DNSNames    []string  `json:"dnsNames,omitempty"`
	IPs         []string  `json:"ips,omitempty"`
	URIs        []string  `json:"uris,omitempty"`
	Emails      []string  `json:"emails,omitempty"`
	Signature   string    `json:"signature"`
	PublicKey   string    `json:"publicKey"`
	Fingerprint string    `json:"fingerprint"`
}

// Inspect 解析 PEM 证书文件，返回其中每一张证书的信息。
// 文件可以包含证书链；Index 从 0 开始，0 通常是叶子或签发 CA。
func Inspect(path string) ([]CertInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取证书 %s 失败: %w", path, err)
	}
	return InspectPEM(data, path)
}

// InspectPEM 解析 PEM 证书字节并返回每一张证书的信息。
func InspectPEM(data []byte, path string) ([]CertInfo, error) {
	certs, err := ParseCertsPEM(data)
	if err != nil {
		return nil, fmt.Errorf("解析证书 %s 失败: %w", path, err)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("文件 %s 中没有证书", path)
	}
	out := make([]CertInfo, 0, len(certs))
	for i, cert := range certs {
		out = append(out, certInfo(path, i, cert))
	}
	return out, nil
}

func certInfo(path string, index int, cert *x509.Certificate) CertInfo {
	return CertInfo{
		Path:        path,
		Index:       index,
		Subject:     cert.Subject.String(),
		Issuer:      cert.Issuer.String(),
		SelfSigned:  cert.CheckSignatureFrom(cert) == nil,
		Serial:      fmt.Sprintf("%x", cert.SerialNumber),
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
		IsCA:        cert.IsCA,
		PathLen:     cert.MaxPathLen,
		PathLenZero: cert.MaxPathLenZero,
		KeyUsage:    keyUsageNames(cert.KeyUsage),
		ExtKeyUsage: extKeyUsageNames(cert.ExtKeyUsage),
		DNSNames:    append([]string(nil), cert.DNSNames...),
		IPs:         IPStrings(cert.IPAddresses),
		URIs:        uriStrings(cert.URIs),
		Emails:      append([]string(nil), cert.EmailAddresses...),
		Signature:   cert.SignatureAlgorithm.String(),
		PublicKey:   publicKeyName(cert.PublicKey),
		Fingerprint: fingerprintDER(cert.Raw),
	}
}

func fingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Fingerprint 返回证书 DER 内容的 "sha256:<hex>" 指纹。
// nil 证书返回空串：调用方普遍以空串表示“证书不可用”，便于比较与报告。
// 收敛点：certrenew 与本包共用同一算法，逐字节一致才能对账。
func Fingerprint(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	return fingerprintDER(cert.Raw)
}

// FingerprintOfPEMFile 返回 PEM 证书文件中第一张证书的指纹。
// 读盘或解析失败时返回错误；吞错还是上报由调用方决定，本包只提供原语。
func FingerprintOfPEMFile(path string) (string, error) {
	cert, err := LoadCertificate(path)
	if err != nil {
		return "", err
	}
	return Fingerprint(cert), nil
}

func keyUsageNames(ku x509.KeyUsage) []string {
	bits := []struct {
		bit  x509.KeyUsage
		name string
	}{
		{x509.KeyUsageDigitalSignature, "DigitalSignature"},
		{x509.KeyUsageContentCommitment, "ContentCommitment"},
		{x509.KeyUsageKeyEncipherment, "KeyEncipherment"},
		{x509.KeyUsageDataEncipherment, "DataEncipherment"},
		{x509.KeyUsageKeyAgreement, "KeyAgreement"},
		{x509.KeyUsageCertSign, "CertSign"},
		{x509.KeyUsageCRLSign, "CRLSign"},
		{x509.KeyUsageEncipherOnly, "EncipherOnly"},
		{x509.KeyUsageDecipherOnly, "DecipherOnly"},
	}
	var out []string
	for _, b := range bits {
		if ku&b.bit != 0 {
			out = append(out, b.name)
		}
	}
	return out
}

var extKeyUsageName = map[x509.ExtKeyUsage]string{
	x509.ExtKeyUsageAny:             "Any",
	x509.ExtKeyUsageServerAuth:      "ServerAuth",
	x509.ExtKeyUsageClientAuth:      "ClientAuth",
	x509.ExtKeyUsageCodeSigning:     "CodeSigning",
	x509.ExtKeyUsageEmailProtection: "EmailProtection",
	x509.ExtKeyUsageTimeStamping:    "TimeStamping",
	x509.ExtKeyUsageOCSPSigning:     "OCSPSigning",
}

func extKeyUsageNames(ekus []x509.ExtKeyUsage) []string {
	out := make([]string, 0, len(ekus))
	for _, e := range ekus {
		if n, ok := extKeyUsageName[e]; ok {
			out = append(out, n)
		} else {
			out = append(out, fmt.Sprintf("ExtKeyUsage(%d)", e))
		}
	}
	return out
}

func uriStrings(uris []*url.URL) []string {
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		out = append(out, u.String())
	}
	return out
}

// IPStrings 把 IP 列表渲染为字符串（证书信息与 CLI 输出共用）。
func IPStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func publicKeyName(pub any) string {
	switch p := pub.(type) {
	case *ecdsa.PublicKey:
		return "ECDSA " + p.Curve.Params().Name
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d bit", p.N.BitLen())
	case ed25519.PublicKey:
		return "Ed25519"
	default:
		return strings.TrimSpace(fmt.Sprintf("%T", pub))
	}
}
