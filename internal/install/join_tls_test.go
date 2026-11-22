package install

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"kubemono/internal/bootstrap"
	"kubemono/internal/discovery"
	"kubemono/internal/pki"
)

// testTLSMaterial 生成一张自签 CA 与一张由它签发的服务端叶子证书。
// 只把叶子放进 tls.Certificate（不发送 CA）：这正是 kube-apiserver 的行为，
// 因此 CA pin 不能直接当作服务端证书 pin 使用。
func testTLSMaterial(t *testing.T) (caPEM []byte, serverCert tls.Certificate) {
	t.Helper()
	ca, err := pki.NewCA("kubemono-test-ca", "", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, signer, err := ca.Sign(pki.CertConfig{
		CommonName:  "kube-apiserver",
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Usages:      []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		Validity:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pki.EncodeCertPEM(ca.Cert), tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: signer}
}

func TestJoinRESTConfigRequiresCA(t *testing.T) {
	if _, err := joinRESTConfig(nil, bootstrap.Token{}); err == nil {
		t.Fatal("nil info 必须报错")
	}
	if _, err := joinRESTConfig(&discovery.Info{Endpoint: "https://127.0.0.1:6443"}, bootstrap.Token{}); err == nil {
		t.Fatal("缺少 CA 必须报错")
	}
}

func TestJoinRESTConfigVerifiesServerWithDiscoveredCA(t *testing.T) {
	caPEM, cert := testTLSMaterial(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	cfg, err := joinRESTConfig(&discovery.Info{Endpoint: srv.URL, CA: caPEM},
		bootstrap.Token{ID: "abcdef", Secret: "0123456789abcdef"})
	if err != nil {
		t.Fatalf("joinRESTConfig: %v", err)
	}
	if cfg.Insecure || len(cfg.CAData) == 0 {
		t.Fatalf("必须用 CA 做正常校验而不是 Insecure: insecure=%v caData=%d", cfg.Insecure, len(cfg.CAData))
	}
	tr, err := rest.TransportFor(cfg)
	if err != nil {
		t.Fatalf("TransportFor: %v", err)
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("用 cluster-info 的 CA 应能校验服务端: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// 错误的 CA 必须被拒绝（否则等于没有校验）。
	wrongCA, _ := testTLSMaterial(t)
	badCfg, err := joinRESTConfig(&discovery.Info{Endpoint: srv.URL, CA: wrongCA},
		bootstrap.Token{ID: "abcdef", Secret: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	badTr, err := rest.TransportFor(badCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&http.Client{Transport: badTr}).Get(srv.URL); err == nil {
		t.Fatal("错误的 CA 必须导致 TLS 校验失败")
	}
}
