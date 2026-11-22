package certrenew

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/pki"
	"kubemono/internal/util/fsutil"
	"kubemono/internal/util/output"
)

// caFixture 是一个带三套 CA 和若干叶子证书的节点。
type caFixture struct {
	paths constants.Paths
	cas   map[string]*pki.CA
}

// newCAFixture 在临时根目录下创建 master 的三套 CA。
func newCAFixture(t *testing.T) *caFixture {
	t.Helper()
	paths := constants.NewPaths(t.TempDir())
	f := &caFixture{paths: paths, cas: map[string]*pki.CA{}}
	mk := func(name, cert, key string) {
		ca, err := pki.NewCA(name, pki.ECDSAP256, 0)
		if err != nil {
			t.Fatalf("NewCA(%s): %v", name, err)
		}
		if err := ca.WriteToDisk(cert, key); err != nil {
			t.Fatalf("WriteToDisk(%s): %v", name, err)
		}
		f.cas[name] = ca
	}
	mk("kubernetes-ca", paths.CertPath(constants.CACertName), paths.CertPath(constants.CAKeyName))
	mk("front-proxy-ca", paths.CertPath(constants.FrontProxyCACertName), paths.CertPath(constants.FrontProxyCAKeyName))
	mk("etcd-ca", paths.EtcdCertPath(constants.CACertName), paths.EtcdCertPath(constants.CAKeyName))
	if err := pki.GenerateServiceAccountKeyPair(paths.ServiceAccountPub(), paths.ServiceAccountKey()); err != nil {
		t.Fatalf("GenerateServiceAccountKeyPair: %v", err)
	}
	return f
}

// writeLeaf 签发并写入一张叶子证书。
func (f *caFixture) writeLeaf(t *testing.T, caName string, cfg pki.CertConfig, certPath, keyPath string) *x509.Certificate {
	t.Helper()
	ca, ok := f.cas[caName]
	if !ok {
		t.Fatalf("unknown CA %s", caName)
	}
	cert, key, err := ca.Sign(cfg)
	if err != nil {
		t.Fatalf("Sign(%s): %v", cfg.CommonName, err)
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(certPath, pki.EncodeCertPEM(cert), 0o644); err != nil {
		t.Fatal(err)
	}
	keyPEM, err := pki.EncodeKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return cert
}

// testCluster 是 certrenew 测试使用的单 master 集群。
func testCluster(t *testing.T) *clusterconfig.Cluster {
	t.Helper()
	cluster := &clusterconfig.Cluster{
		Cluster: clusterconfig.ClusterSpec{
			Hosts: clusterconfig.Hosts{
				{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
				{Name: "m2", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
			},
		},
	}
	clusterconfig.SetDefaults(cluster)
	return cluster
}

// testOptions 是重定向根目录、使用真实时钟的选项集合。
// 证书由 pki 用真实时钟签发，所以默认也用真实时钟；需要"已过期"场景的测试
// 自行覆盖 Now。
func testOptions(t *testing.T, paths constants.Paths, cluster *clusterconfig.Cluster) Options {
	t.Helper()
	return Options{
		Paths:      paths,
		Cluster:    cluster,
		Host:       &clusterconfig.Host{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		Report:     output.Discard(),
		WarnBefore: 30 * 24 * time.Hour,
		Now:        time.Now,
	}
}

// fakeRestarter 记录被重启的组件。
type fakeRestarter struct {
	components []string
	err        error
	calls      int
}

// Restart 实现 Restarter。
func (f *fakeRestarter) Restart(_ context.Context, components []string) error {
	f.calls++
	f.components = append([]string(nil), components...)
	return f.err
}

// fingerprintOfFile 返回文件里第一张证书的 sha256。
func fingerprintOfFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	certs, err := pki.ParseCertsPEM(data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return pki.Fingerprint(certs[0])
}

// writeKubeconfig 写入引用（或内嵌）证书的 kubeconfig。
func writeKubeconfig(t *testing.T, path string, caPEM []byte, clientCertPEM []byte, certFile string) {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["kubernetes"] = &clientcmdapi.Cluster{
		Server:                   "https://127.0.0.1:6443",
		CertificateAuthorityData: caPEM,
	}
	auth := &clientcmdapi.AuthInfo{}
	if certFile != "" {
		auth.ClientCertificate = certFile
	} else {
		auth.ClientCertificateData = clientCertPEM
	}
	cfg.AuthInfos["admin"] = auth
	cfg.Contexts["kubernetes"] = &clientcmdapi.Context{Cluster: "kubernetes", AuthInfo: "admin"}
	cfg.CurrentContext = "kubernetes"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
}

// writeFileRefKubeconfig 写入引用证书文件的 kubeconfig（kubemono 的真实形态：
// 客户端证书与 CA 都是文件路径，不是内嵌 data）。
func writeFileRefKubeconfig(t *testing.T, path, caFile, clientCertFile string) {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["kubernetes"] = &clientcmdapi.Cluster{
		Server:               "https://127.0.0.1:6443",
		CertificateAuthority: caFile,
	}
	auth := &clientcmdapi.AuthInfo{}
	if clientCertFile != "" {
		auth.ClientCertificate = clientCertFile
	}
	cfg.AuthInfos["admin"] = auth
	cfg.Contexts["kubernetes"] = &clientcmdapi.Context{Cluster: "kubernetes", AuthInfo: "admin"}
	cfg.CurrentContext = "kubernetes"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
}

// selfSignedCert 构造任意证书（用于已过期、未知签发者等边界场景）。
func selfSignedCert(t *testing.T, notBefore, notAfter time.Time, cn string, isCA bool) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  isCA,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// writeFile 写入测试文件。
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readFile 读取测试文件。
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

var _ Restarter = (*fakeRestarter)(nil)
