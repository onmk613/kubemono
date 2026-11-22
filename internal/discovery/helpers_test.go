package discovery

import (
	"testing"
	"time"

	"kubemono/internal/pki"
)

// pemCert 创建一个自签名的 CA 证书（PEM 形式）。
func pemCert(t *testing.T) []byte {
	t.Helper()
	ca, err := pki.NewCA("kubernetes-ca", "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return pki.EncodeCertPEM(ca.Cert)
}
