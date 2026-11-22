package certs

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/phase"
	"kubemono/internal/pki"
	"kubemono/internal/util/output"
)

func testCluster() (*clusterconfig.Cluster, *clusterconfig.Host) {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "m2", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	c.ControlPlane.CertSANs = []string{"api.internal.example.com"}
	c.ControlPlane.ExternalEndpoints = []string{"k8s.example.com:6443"}
	clusterconfig.SetDefaults(c)
	host, _ := c.Cluster.Hosts.FindHost("m1")
	return c, host
}

func testContext(t *testing.T, cluster *clusterconfig.Cluster, host *clusterconfig.Host) *phase.Context {
	t.Helper()
	return &phase.Context{
		Cluster:    cluster,
		ClusterID:  "cid-1",
		Host:       host,
		IsInitNode: true,
		Paths:      constants.NewPaths(t.TempDir()),
		Release:    "v1.31.4-km.1",
		Report:     output.Discard(),
		Now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func TestAppliesOnlyOnCAPossessingNodes(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	if !(&Phase{}).Applies(c) {
		t.Fatal("master must run the certs phase")
	}
	worker, _ := cluster.Cluster.Hosts.FindHost("w1")
	c.Host = worker
	if (&Phase{}).Applies(c) {
		t.Fatal("worker must not run the certs phase (its CA comes from the join exchange)")
	}
}

func TestRunCreatesFullPKI(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	ctx := context.Background()

	done, err := (&Phase{}).Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("fresh node must not report the phase as done")
	}
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	done, err = (&Phase{}).Check(ctx, c)
	if err != nil || !done {
		t.Fatalf("Check after Run: done=%v err=%v", done, err)
	}

	// 每个 CA 的证书与私钥都在，且权限正确。
	for _, ca := range caPairs(c) {
		certPath, keyPath := ca.Paths(c.Paths)
		assertFile(t, certPath, 0o644)
		assertFile(t, keyPath, 0o600)
	}
	assertFile(t, c.Paths.ServiceAccountPub(), 0o644)
	assertFile(t, c.Paths.ServiceAccountKey(), 0o600)
	assertFile(t, c.Paths.CertPath(constants.APIServerCertName), 0o644)
	assertFile(t, c.Paths.CertPath(constants.APIServerKeyName), 0o600)
	assertFile(t, c.Paths.CertPath(constants.APIServerKubeletClientCertName), 0o644)
	assertFile(t, c.Paths.CertPath(constants.FrontProxyClientCertName), 0o644)
	assertFile(t, c.Paths.EtcdCertPath("server.crt"), 0o644)
	assertFile(t, c.Paths.EtcdCertPath("peer.crt"), 0o644)
	assertFile(t, c.Paths.CertPath(constants.APIServerEtcdClientCertName), 0o644)
	assertFile(t, c.Paths.EtcdCertPath(constants.EtcdKubemonoClientCertName), 0o644)

	// apiserver 证书的 SAN 只覆盖本节点、Service 地址、外部端点与 certSANs。
	// 其它 master 的名字/地址不得出现：证书只断言持有者实际能服务的端点。
	cert := parseCert(t, c.Paths.CertPath(constants.APIServerCertName))
	if cert.Subject.CommonName != constants.APIServerCertCN {
		t.Fatalf("apiserver CN = %s", cert.Subject.CommonName)
	}
	wantDNS := []string{
		"kubernetes.default.svc." + cluster.Network.DNSDomain,
		"m1", "api.internal.example.com", "k8s.example.com",
	}
	for _, dns := range wantDNS {
		if !contains(cert.DNSNames, dns) {
			t.Errorf("apiserver SAN 缺少 DNS %q（现有 %v）", dns, cert.DNSNames)
		}
	}
	for _, dns := range []string{"m2", "w1"} {
		if contains(cert.DNSNames, dns) {
			t.Errorf("apiserver SAN 不应包含其它节点的 DNS %q（现有 %v）", dns, cert.DNSNames)
		}
	}
	wantIPs := []string{"10.0.0.1", "10.96.0.1", "127.0.0.1"}
	for _, ip := range wantIPs {
		if !containsIP(cert.IPAddresses, ip) {
			t.Errorf("apiserver SAN 缺少 IP %s（现有 %v）", ip, cert.IPAddresses)
		}
	}
	for _, ip := range []string{"10.0.0.2", "10.0.0.11"} {
		if containsIP(cert.IPAddresses, ip) {
			t.Errorf("apiserver SAN 不应包含其它节点的 IP %s（现有 %v）", ip, cert.IPAddresses)
		}
	}

	// apiserver-kubelet-client 绑定 system:kubelet-api-admin。
	kubeletClient := parseCert(t, c.Paths.CertPath(constants.APIServerKubeletClientCertName))
	if got := kubeletClient.Subject.Organization; len(got) != 1 || got[0] != "system:kubelet-api-admin" {
		t.Fatalf("apiserver-kubelet-client O = %v", got)
	}
	if kubeletClient.Subject.CommonName != constants.UserAPIServerKubeletClient {
		t.Fatalf("apiserver-kubelet-client CN = %s", kubeletClient.Subject.CommonName)
	}

	// 叶子证书必须由对应的 CA 签发（而不是自签）。
	ca := parseCert(t, c.Paths.CertPath(constants.CACertName))
	if err := cert.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("apiserver 证书不是由 kubernetes-ca 签发的: %v", err)
	}
	frontProxyCA := parseCert(t, c.Paths.CertPath(constants.FrontProxyCACertName))
	frontProxyClient := parseCert(t, c.Paths.CertPath(constants.FrontProxyClientCertName))
	if err := frontProxyClient.CheckSignatureFrom(frontProxyCA); err != nil {
		t.Fatalf("front-proxy-client 不是由 front-proxy-ca 签发的: %v", err)
	}
	etcdCA := parseCert(t, c.Paths.EtcdCertPath(constants.CACertName))
	etcdServer := parseCert(t, c.Paths.EtcdCertPath("server.crt"))
	if err := etcdServer.CheckSignatureFrom(etcdCA); err != nil {
		t.Fatalf("etcd server 证书不是由 etcd-ca 签发的: %v", err)
	}
}

func TestRunIsIdempotentAndNeverReissuesTheCA(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	ctx := context.Background()
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatal(err)
	}
	caBefore, err := os.ReadFile(c.Paths.CertPath(constants.CACertName))
	if err != nil {
		t.Fatal(err)
	}
	leafBefore, err := os.ReadFile(c.Paths.CertPath(constants.APIServerCertName))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	caAfter, _ := os.ReadFile(c.Paths.CertPath(constants.CACertName))
	if string(caBefore) != string(caAfter) {
		t.Fatal("重复执行 certs 不允许重签 CA")
	}
	leafAfter, _ := os.ReadFile(c.Paths.CertPath(constants.APIServerCertName))
	if string(leafBefore) != string(leafAfter) {
		t.Fatal("已存在且有效的叶子证书不应被重签")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	c.DryRun = true
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if _, err := os.Stat(c.Paths.CertPath(constants.CACertName)); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不能写文件: %v", err)
	}
}

func TestStackedVsExternalEtcdTargets(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	if !runsEtcd(c) {
		t.Fatal("stacked 模式下 master 运行 etcd")
	}
	// external + unmanaged：master 上不需要 etcd 证书。
	cluster.ControlPlane.Etcd.External = clusterconfig.ExternalEtcdSpec{Endpoints: []string{"https://127.0.0.1:2379"}}
	targets, err := desiredTargets(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		if tg.leaf.CA == pki.CAEtcd {
			t.Fatalf("外部 etcd 时 master 不应生成 %s", tg.leaf.CertFile)
		}
	}
}

func TestKeyAlgorithmFromConfig(t *testing.T) {
	if _, err := toKeyAlgorithm("bogus"); err == nil {
		t.Fatal("未知算法必须报错")
	}
	if alg, err := toKeyAlgorithm(clusterconfig.KeyAlgorithmECDSAP256); err != nil || alg != pki.ECDSAP256 {
		t.Fatalf("ecdsa-p256 映射错误: %v %v", alg, err)
	}
	if alg, err := toKeyAlgorithm(""); err != nil || alg != pki.RSA2048 {
		t.Fatalf("空值应使用 rsa2048: %v %v", alg, err)
	}
}

// --- 辅助函数 ---
func assertFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("文件不存在 %s: %v", path, err)
	}
	if info.Mode().Perm() != mode {
		t.Errorf("%s 权限为 %04o，期望 %04o", filepath.Base(path), info.Mode().Perm(), mode)
	}
}

func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("%s 不是 PEM", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	return cert
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

func containsIP(list []net.IP, want string) bool {
	for _, v := range list {
		if v.String() == want {
			return true
		}
	}
	return false
}

// TestAPIServerSANScopedToNode 固定"证书只断言本节点身份"：在 m2 上签发的
// apiserver 证书不得包含 m1 / w1 的名字与地址。
func TestAPIServerSANScopedToNode(t *testing.T) {
	cluster, _ := testCluster()
	m2, ok := cluster.Cluster.Hosts.FindHost("m2")
	if !ok {
		t.Fatal("fixture 缺少 m2")
	}
	c := testContext(t, cluster, m2)
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cert := parseCert(t, c.Paths.CertPath(constants.APIServerCertName))
	if !contains(cert.DNSNames, "m2") {
		t.Errorf("m2 的 apiserver SAN 缺少本机名字（现有 %v）", cert.DNSNames)
	}
	if !containsIP(cert.IPAddresses, "10.0.0.2") {
		t.Errorf("m2 的 apiserver SAN 缺少本机地址（现有 %v）", cert.IPAddresses)
	}
	for _, dns := range []string{"m1", "w1"} {
		if contains(cert.DNSNames, dns) {
			t.Errorf("m2 的 apiserver SAN 不应包含 %q（现有 %v）", dns, cert.DNSNames)
		}
	}
	for _, ip := range []string{"10.0.0.1", "10.0.0.11"} {
		if containsIP(cert.IPAddresses, ip) {
			t.Errorf("m2 的 apiserver SAN 不应包含 %s（现有 %v）", ip, cert.IPAddresses)
		}
	}
}

// TestAddingMasterKeepsExistingAPIServerCert 固定 SAN 收窄带来的运维性质：
// 集群新增一台 master 不会改变存量 master 的 desired SAN，因此 node repair
// 不会把它们的 apiserver 证书判成漂移（否则每次扩容都要全量重签 + 重启）。
func TestAddingMasterKeepsExistingAPIServerCert(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	ctx := context.Background()
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cluster.Cluster.Hosts = append(cluster.Cluster.Hosts, clusterconfig.Host{
		Name: "m3", Address: "10.0.0.3", Roles: []clusterconfig.Role{clusterconfig.RoleMaster},
	})
	done, err := (&Phase{}).Check(ctx, c)
	if err != nil {
		t.Fatalf("Check after adding master: %v", err)
	}
	if !done {
		t.Fatal("新增 master 不应让存量 master 的证书判成漂移")
	}
}

// TestAPIServerSANIncludesExternalEndpoint 断言 externalEndpoints 自动进入
// apiserver SAN，worker kubelet 可直接指向它。
func TestAPIServerSANIncludesExternalEndpoint(t *testing.T) {
	cluster, host := testCluster()
	cluster.ControlPlane.ExternalEndpoints = []string{"10.0.0.200:6443"}
	c := testContext(t, cluster, host)
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cert := parseCert(t, c.Paths.CertPath(constants.APIServerCertName))
	if !containsIP(cert.IPAddresses, "10.0.0.200") {
		t.Fatalf("apiserver SAN 缺少 externalEndpoints 的 IP（现有 %v）", cert.IPAddresses)
	}
}

func TestRunImportsExternalCA(t *testing.T) {
	externalDir := t.TempDir()
	external, err := pki.NewCA("enterprise-ca", pki.ECDSAP256, 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	externalCert := filepath.Join(externalDir, "ca.crt")
	externalKey := filepath.Join(externalDir, "ca.key")
	if err := external.WriteToDisk(externalCert, externalKey); err != nil {
		t.Fatalf("WriteToDisk: %v", err)
	}

	cluster, host := testCluster()
	cluster.ControlPlane.PKI.ExternalCA = clusterconfig.ExternalCASpec{CertFile: externalCert, KeyFile: externalKey}
	c := testContext(t, cluster, host)
	ctx := context.Background()
	done, err := (&Phase{}).Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("尚未导入 externalCA 时 Check 必须为 false")
	}
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := pki.LoadCA(c.Paths.CertPath(constants.CACertName), c.Paths.CertPath(constants.CAKeyName))
	if err != nil {
		t.Fatalf("加载导入后的 CA: %v", err)
	}
	if !bytes.Equal(got.Cert.Raw, external.Cert.Raw) {
		t.Fatal("集群 CA 不是 externalCA 指定的企业 CA")
	}
	done, err = (&Phase{}).Check(ctx, c)
	if err != nil || !done {
		t.Fatalf("导入后 Check = %v/%v", done, err)
	}
}

func TestSANChangeReissuesAPIServerCertificate(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	ctx := context.Background()
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	before, err := os.ReadFile(c.Paths.CertPath(constants.APIServerCertName))
	if err != nil {
		t.Fatal(err)
	}
	// 新增一个 apiserver SAN：必须重签并重启组件。
	cluster.ControlPlane.CertSANs = append(cluster.ControlPlane.CertSANs, "api2.internal.example.com")
	done, err := (&Phase{}).Check(ctx, c)
	if err != nil {
		t.Fatalf("Check after SAN change: %v", err)
	}
	if done {
		t.Fatal("SAN 变化后 Check 必须为 false")
	}
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run after SAN change: %v", err)
	}
	after, err := os.ReadFile(c.Paths.CertPath(constants.APIServerCertName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("SAN 变化后 apiserver 证书必须重签")
	}
	cert := parseCert(t, c.Paths.CertPath(constants.APIServerCertName))
	if !slices.Contains(cert.DNSNames, "api2.internal.example.com") {
		t.Fatalf("新证书缺少 SAN: %v", cert.DNSNames)
	}
}

func TestEtcdCertCNUsesNodeName(t *testing.T) {
	cluster, host := testCluster()
	host.NodeName = "m1.example.com"
	c := testContext(t, cluster, host)
	c.NodeNameValue = host.NodeName
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range []string{"server.crt", "peer.crt"} {
		cert := parseCert(t, c.Paths.EtcdCertPath(name))
		if cert.Subject.CommonName != "m1.example.com" {
			t.Errorf("%s CN = %q, want m1.example.com", name, cert.Subject.CommonName)
		}
		if !contains(cert.DNSNames, "m1.example.com") {
			t.Errorf("%s SAN 缺少 nodeName: %v", name, cert.DNSNames)
		}
	}
}

// TestRunImportsExternalCAsForAllDomains 固定三个信任域都可以导入企业 CA：
// kubernetes-ca / front-proxy-ca / etcd-ca，各自签发的叶子必须来自对应 CA。
func TestRunImportsExternalCAsForAllDomains(t *testing.T) {
	mk := func(name string) (*pki.CA, string, string) {
		t.Helper()
		dir := t.TempDir()
		ca, err := pki.NewCA(name, pki.ECDSAP256, 10*365*24*time.Hour)
		if err != nil {
			t.Fatalf("NewCA(%s): %v", name, err)
		}
		certPath := filepath.Join(dir, "ca.crt")
		keyPath := filepath.Join(dir, "ca.key")
		if err := ca.WriteToDisk(certPath, keyPath); err != nil {
			t.Fatalf("WriteToDisk(%s): %v", name, err)
		}
		return ca, certPath, keyPath
	}
	kubeCA, kubeCert, kubeKey := mk("corp-kubernetes-ca")
	frontCA, frontCert, frontKey := mk("corp-front-proxy-ca")
	etcdCA, etcdCert, etcdKey := mk("corp-etcd-ca")

	cluster, host := testCluster()
	cluster.ControlPlane.PKI.ExternalCA = clusterconfig.ExternalCASpec{CertFile: kubeCert, KeyFile: kubeKey}
	cluster.ControlPlane.PKI.ExternalFrontProxyCA = clusterconfig.ExternalCASpec{CertFile: frontCert, KeyFile: frontKey}
	cluster.ControlPlane.PKI.ExternalEtcdCA = clusterconfig.ExternalCASpec{CertFile: etcdCert, KeyFile: etcdKey}
	c := testContext(t, cluster, host)
	ctx := context.Background()

	if done, err := (&Phase{}).Check(ctx, c); err != nil || done {
		t.Fatalf("导入前 Check = %v/%v，期望 false", done, err)
	}
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, tc := range []struct {
		name string
		path string
		want *pki.CA
	}{
		{"kubernetes-ca", c.Paths.CertPath(constants.CACertName), kubeCA},
		{"front-proxy-ca", c.Paths.CertPath(constants.FrontProxyCACertName), frontCA},
		{"etcd-ca", c.Paths.EtcdCertPath(constants.CACertName), etcdCA},
	} {
		got, err := pki.LoadCA(tc.path, strings.TrimSuffix(tc.path, ".crt")+".key")
		if err != nil {
			t.Fatalf("LoadCA(%s): %v", tc.name, err)
		}
		if !bytes.Equal(got.Cert.Raw, tc.want.Cert.Raw) {
			t.Fatalf("%s 未使用导入的企业 CA", tc.name)
		}
	}
	for _, tc := range []struct {
		name string
		path string
		ca   *pki.CA
	}{
		{"apiserver", c.Paths.CertPath(constants.APIServerCertName), kubeCA},
		{"front-proxy-client", c.Paths.CertPath(constants.FrontProxyClientCertName), frontCA},
		{"etcd-server", c.Paths.EtcdCertPath(constants.EtcdServerCertName), etcdCA},
	} {
		cert, err := pki.LoadCertificate(tc.path)
		if err != nil {
			t.Fatalf("LoadCertificate(%s): %v", tc.name, err)
		}
		if err := cert.CheckSignatureFrom(tc.ca.Cert); err != nil {
			t.Fatalf("%s 不是由 %s 签发: %v", tc.name, tc.ca.Cert.Subject.CommonName, err)
		}
	}
	if done, err := (&Phase{}).Check(ctx, c); err != nil || !done {
		t.Fatalf("导入后 Check = %v/%v，期望 true", done, err)
	}
}

// recordingRestarter 记录 certs phase 要求重启的组件。
type recordingRestarter struct {
	components []string
	err        error
	calls      int
}

func (r *recordingRestarter) Restart(_ context.Context, components []string) error {
	r.calls++
	r.components = append([]string(nil), components...)
	return r.err
}

// TestSANChangeRestartsAPIServer 固定 P0-4：
// certSANs 变化重签 apiserver 证书后，必须重启 apiserver 才会加载新证书；
// 仅写文件会让“重签”看起来成功、实际仍在用旧 SAN。
func TestSANChangeRestartsAPIServer(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	restarter := &recordingRestarter{}
	p := &Phase{Restarter: restarter}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	restarter.components = nil

	cluster.ControlPlane.CertSANs = append(cluster.ControlPlane.CertSANs, "api2.internal.example.com")
	done, err := p.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("SAN 变化后 Check 必须为 false")
	}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run after SAN change: %v", err)
	}
	if !slices.Contains(restarter.components, constants.ComponentAPIServer) {
		t.Fatalf("SAN 变化后必须重启 apiserver，实际重启 %v", restarter.components)
	}
}

// TestInstalledNodeRefusesToCreateMissingCA 固定 CA 缺失时的 fail-closed：
// 没有 --force-init 的同步/修复不能凭空生成第二套信任根。
func TestInstalledNodeRefusesToCreateMissingCA(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	c.IsInitNode = false
	c.Node = &clusterconfig.NodeConfig{NodeName: "m1", ClusterID: "cid-1"}
	err := (&Phase{}).Run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "不会自动生成新 CA") {
		t.Fatalf("CA 缺失时应拒绝重建，实际 %v", err)
	}
	if pki.FileExists(c.Paths.CertPath(constants.CACertName)) {
		t.Fatal("拒绝后不应写出新 CA")
	}
}

// TestInstalledNodeRefusesToRegenerateMismatchedSA 固定 SA 密钥 fail-closed：
// 只缺一个文件时不能自动重建，否则本机与其他 master 的 SA 公钥会分叉。
func TestInstalledNodeRefusesToRegenerateMismatchedSA(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	// 先以 init 身份生成完整 PKI，再模拟 sa.pub 丢失。
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	if err := os.Remove(c.Paths.ServiceAccountPub()); err != nil {
		t.Fatal(err)
	}
	c.IsInitNode = false
	c.Node = &clusterconfig.NodeConfig{NodeName: "m1", ClusterID: "cid-1"}
	err := (&Phase{}).Run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "拒绝自动重建") {
		t.Fatalf("SA 缺失时应拒绝重建，实际 %v", err)
	}
	if pki.FileExists(c.Paths.ServiceAccountPub()) {
		t.Fatal("拒绝后不应写出新的 SA 公钥")
	}
}

// TestPendingRestartIsRetried 固定“证书已换、组件未重启”的恢复路径：
// 重启失败后 marker 必须保留，下一次 Check 触发 Run 重试，而不是因为证书
// 已经匹配新 SAN 而永久跳过。
func TestPendingRestartIsRetried(t *testing.T) {
	cluster, host := testCluster()
	c := testContext(t, cluster, host)
	restarter := &recordingRestarter{err: errors.New("systemctl restart 失败")}
	p := &Phase{Restarter: restarter}
	if err := p.Run(context.Background(), c); err == nil {
		t.Fatal("重启失败时 Run 必须返回错误")
	}
	if len(readPendingCertRestart(c)) == 0 {
		t.Fatal("重启失败后必须保留 pending marker")
	}
	if done, err := p.Check(context.Background(), c); err != nil || done {
		t.Fatalf("有 pending restart 时 Check = (%v,%v)，期望 false", done, err)
	}

	restarter.err = nil
	restarter.calls = 0
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("retry Run: %v", err)
	}
	if restarter.calls == 0 {
		t.Fatal("retry Run 必须再次调用 Restarter")
	}
	if len(readPendingCertRestart(c)) != 0 {
		t.Fatal("重启成功后必须清除 pending marker")
	}
}
