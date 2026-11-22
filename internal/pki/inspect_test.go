package pki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInspectCertificate 固定证书解析字段：Subject/Issuer/SAN/EKU/指纹/链。
func TestInspectCertificate(t *testing.T) {
	ca, err := NewCA("inspect-ca", ECDSAP256, 0)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	cert, key, err := ca.Sign(CertConfig{
		CommonName:  "etcd-1",
		DNSNames:    []string{"etcd-1.example.com"},
		IPAddresses: nil,
		Usages:      UsageServerClient,
		Validity:    time.Hour,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "etcd-1.crt")
	keyPath := filepath.Join(dir, "etcd-1.key")
	if err := WriteCertAndKey(certPath, keyPath, cert, key); err != nil {
		t.Fatalf("WriteCertAndKey: %v", err)
	}
	// 把 CA 追加到文件末尾，验证链解析。
	data, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, EncodeCertPEM(ca.Cert)...)
	if err := os.WriteFile(certPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	infos, err := Inspect(certPath)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("certificates = %d，期望 2", len(infos))
	}
	leaf := infos[0]
	if leaf.Index != 0 || leaf.Subject == "" || leaf.Issuer == "" {
		t.Fatalf("leaf info = %+v", leaf)
	}
	if !strings.Contains(leaf.Subject, "etcd-1") || !strings.Contains(leaf.Issuer, "inspect-ca") {
		t.Fatalf("subject/issuer = %q/%q", leaf.Subject, leaf.Issuer)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "etcd-1.example.com" {
		t.Fatalf("DNS = %v", leaf.DNSNames)
	}
	if !containsString(leaf.ExtKeyUsage, "ServerAuth") || !containsString(leaf.ExtKeyUsage, "ClientAuth") {
		t.Fatalf("EKU = %v", leaf.ExtKeyUsage)
	}
	if leaf.SelfSigned || !strings.HasPrefix(leaf.Fingerprint, "sha256:") {
		t.Fatalf("selfSigned/fingerprint = %v/%q", leaf.SelfSigned, leaf.Fingerprint)
	}
	if !infos[1].IsCA || !infos[1].SelfSigned {
		t.Fatalf("CA info = %+v", infos[1])
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
