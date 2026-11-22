package pki

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadAndWriteCAChain 固定 external CA 证书链：LoadCA 保留中间 CA + 根，
// WriteToDisk 原样写回整条链。
func TestLoadAndWriteCAChain(t *testing.T) {
	root, err := NewCA("corp-root", ECDSAP256, 0)
	if err != nil {
		t.Fatalf("NewCA(root): %v", err)
	}
	key, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "corp-intermediate"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, root.Cert, key.Public(), root.Key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	intermediate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	var pemData []byte
	for _, cert := range []*x509.Certificate{intermediate, root.Cert} {
		pemData = append(pemData, EncodeCertPEM(cert)...)
	}
	if err := os.WriteFile(certPath, pemData, 0o644); err != nil {
		t.Fatal(err)
	}
	keyPEM, err := EncodeKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	ca, err := LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if ca.Cert.Subject.CommonName != "corp-intermediate" {
		t.Fatalf("签名 CA = %q", ca.Cert.Subject.CommonName)
	}
	if len(ca.Chain) != 2 || ca.Chain[1].Subject.CommonName != "corp-root" {
		t.Fatalf("chain = %+v", ca.Chain)
	}
	leaf, leafKey, err := ca.Sign(CertConfig{
		CommonName: "kube-apiserver",
		Usages:     UsageServer,
		Validity:   time.Hour,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if leafKey == nil {
		t.Fatal("Sign 应返回新私钥")
	}
	if err := leaf.CheckSignatureFrom(intermediate); err != nil {
		t.Fatalf("leaf 不是中间 CA 签发: %v", err)
	}
	if err := intermediate.CheckSignatureFrom(root.Cert); err != nil {
		t.Fatalf("中间 CA 不是根签发: %v", err)
	}

	outDir := t.TempDir()
	outCert := filepath.Join(outDir, "ca.crt")
	outKey := filepath.Join(outDir, "ca.key")
	if err := ca.WriteToDisk(outCert, outKey); err != nil {
		t.Fatalf("WriteToDisk: %v", err)
	}
	data, err := os.ReadFile(outCert)
	if err != nil {
		t.Fatal(err)
	}
	certs, err := ParseCertsPEM(data)
	if err != nil {
		t.Fatalf("ParseCertsPEM: %v", err)
	}
	if len(certs) != 2 || certs[1].Subject.CommonName != "corp-root" {
		t.Fatalf("写回的 chain = %+v", certs)
	}
}
