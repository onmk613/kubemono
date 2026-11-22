package cert_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"kubemono/internal/cli/clitest"
	"kubemono/internal/pki"
)

// TestCertShowJSON 覆盖只读证书查看的 JSON 输出与链解析。
func TestCertShowJSON(t *testing.T) {
	ca, err := pki.NewCA("show-ca", pki.ECDSAP256, 0)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	cert, key, err := ca.Sign(pki.CertConfig{
		CommonName: "etcd-1",
		DNSNames:   []string{"etcd-1.example.com"},
		Usages:     pki.UsageServerClient,
		Validity:   time.Hour,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "etcd-1.crt")
	keyPath := filepath.Join(dir, "etcd-1.key")
	if err := pki.WriteCertAndKey(certPath, keyPath, cert, key); err != nil {
		t.Fatalf("WriteCertAndKey: %v", err)
	}
	out, err := clitest.ExecuteRoot(t, "cert", "show", certPath, "--output", "json")
	if err != nil {
		t.Fatalf("cert show: %v\n%s", err, out)
	}
	var infos []pki.CertInfo
	if err := json.Unmarshal([]byte(out), &infos); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out)
	}
	if len(infos) != 1 {
		t.Fatalf("certificates = %d", len(infos))
	}
	if infos[0].Subject == "" || len(infos[0].DNSNames) != 1 || infos[0].Fingerprint == "" {
		t.Fatalf("info = %+v", infos[0])
	}
}
