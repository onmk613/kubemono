package pki

import (
	"crypto/x509"
	"kubemono/internal/util/stringsx"
	"os"
	"time"

	"kubemono/internal/constants"
)

// CertificateExists 判断一份证书/私钥对是否可以继续使用。
// 条件：两个文件都存在、证书可解析、私钥可解析且与证书公钥匹配、
// 证书在 constants.DefaultCertWarnBefore（30 天）的续期阈值之外。
// 任意一项不满足都返回 false，调用方据此重新签发。
func CertificateExists(certPath, keyPath string) bool {
	certs, err := readCertsFile(certPath)
	if err != nil {
		return false
	}
	key, err := readKeyFile(keyPath)
	if err != nil {
		return false
	}
	if !publicKeysEqual(certs[0].PublicKey, key.Public()) {
		return false
	}
	return !expiresWithin(certs, constants.DefaultCertWarnBefore, time.Now())
}

// CertificateMatchesConfig 报告已有证书是否按给定的期望身份/SAN/用途签发，
// 它有意忽略有效期时长；调用方需自行结合 CertificateExists 使用。
func CertificateMatchesConfig(certPath string, cfg CertConfig) (bool, error) {
	certs, err := readCertsFile(certPath)
	if err != nil {
		return false, err
	}
	return CertMatchesConfig(certs[0], cfg), nil
}

// CertMatchesConfig 把已解析的证书与期望的 CertConfig 比较。
func CertMatchesConfig(cert *x509.Certificate, cfg CertConfig) bool {
	if cert == nil {
		return false
	}
	if cert.Subject.CommonName != cfg.CommonName {
		return false
	}
	if !stringsx.SetEqual(cert.Subject.Organization, stringsx.DedupSorted(cfg.Organizations)) {
		return false
	}
	if !stringsx.SetEqual(cert.DNSNames, normalizeDNSNames(cfg.DNSNames)) {
		return false
	}
	ips := make([]string, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		ips = append(ips, ip.String())
	}
	desiredIPs := make([]string, 0, len(cfg.IPAddresses))
	for _, ip := range normalizeIPs(cfg.IPAddresses) {
		desiredIPs = append(desiredIPs, ip.String())
	}
	if !stringsx.SetEqual(ips, desiredIPs) {
		return false
	}
	usages := cfg.Usages
	if len(usages) == 0 {
		usages = UsageServerClient
	}
	return ekuSetEqual(cert.ExtKeyUsage, usages)
}

func ekuSetEqual(a, b []x509.ExtKeyUsage) bool {
	if len(a) != len(b) {
		return false
	}
	left := make(map[x509.ExtKeyUsage]int, len(a))
	for _, u := range a {
		left[u]++
	}
	for _, u := range b {
		left[u]--
		if left[u] < 0 {
			return false
		}
	}
	return true
}

// FileExists 报告路径存在且是普通文件。
func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// earliestExpiry 返回一组证书中最早的 NotAfter。
func earliestExpiry(certs []*x509.Certificate) time.Time {
	earliest := certs[0].NotAfter
	for _, cert := range certs[1:] {
		if cert.NotAfter.Before(earliest) {
			earliest = cert.NotAfter
		}
	}
	return earliest
}

// expiresWithin 报告证书是否会在 warnBefore 之内到期（含已过期）。
func expiresWithin(certs []*x509.Certificate, warnBefore time.Duration, now time.Time) bool {
	return !now.Add(warnBefore).Before(earliestExpiry(certs))
}
