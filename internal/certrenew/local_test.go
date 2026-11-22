package certrenew

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/pki"
)

// leafState 描述测试节点的一张叶子证书。
type leafState struct {
	name     string
	etcd     bool
	ca       string
	cfg      pki.CertConfig
	validity time.Duration
}

// installLeaf 签发并写入一张叶子证书。
func installLeaf(t *testing.T, f *caFixture, leaf leafState) string {
	t.Helper()
	certPath, keyPath := "", ""
	found := false
	for _, spec := range pki.DefaultCatalogue.AllLeaves() {
		if spec.CertFile == leaf.name {
			certPath, keyPath = spec.Paths(f.paths)
			found = true
			break
		}
	}
	if !found {
		// 扫描测试允许任意文件名（CheckExpiration 故意扫描全部 *.crt）。
		keyName := strings.TrimSuffix(leaf.name, ".crt") + ".key"
		if leaf.etcd {
			certPath, keyPath = f.paths.EtcdCertPath(leaf.name), f.paths.EtcdCertPath(keyName)
		} else {
			certPath, keyPath = f.paths.CertPath(leaf.name), f.paths.CertPath(keyName)
		}
	}
	cfg := leaf.cfg
	cfg.Validity = leaf.validity
	f.writeLeaf(t, leaf.ca, cfg, certPath, keyPath)
	return certPath
}

// apiserverLeaf 是一张 300 天的 apiserver 证书（在 30 天窗口之外）。
func apiserverLeaf() leafState {
	return leafState{
		name: constants.APIServerCertName,
		ca:   "kubernetes-ca",
		cfg: pki.CertConfig{
			CommonName:  "kube-apiserver",
			DNSNames:    []string{"kubernetes", "localhost"},
			Usages:      pki.UsageServer,
			IPAddresses: nil,
		},
		validity: 300 * 24 * time.Hour,
	}
}

// expiringLeaf 是一张 10 天的 apiserver 证书（在 30 天窗口之内）。
func expiringLeaf() leafState {
	leaf := apiserverLeaf()
	leaf.validity = 10 * 24 * time.Hour
	return leaf
}

func TestLocalRenewsOnlyExpiringCertificates(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, apiserverLeaf())
	installLeaf(t, f, leafState{
		name:     constants.ControllerManagerClientCertName,
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "system:kube-controller-manager", Usages: pki.UsageClient},
		validity: 10 * 24 * time.Hour,
	})

	opts := testOptions(t, f.paths, testCluster(t))
	restarter := &fakeRestarter{}
	opts.Restarter = restarter
	beforeAPIServer := fingerprintOfFile(t, f.paths.CertPath(constants.APIServerCertName))
	beforeCA := fingerprintOfFile(t, f.paths.CertPath(constants.CACertName))

	report, err := Local(context.Background(), opts)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if report.Renewed != 1 || report.Skipped != 1 {
		t.Fatalf("renewed=%d skipped=%d, want 1/1: %+v", report.Renewed, report.Skipped, report.Results)
	}
	if after := fingerprintOfFile(t, f.paths.CertPath(constants.APIServerCertName)); after != beforeAPIServer {
		t.Fatal("a still valid certificate must not be re-signed")
	}
	// CA 不变化。
	if after := fingerprintOfFile(t, f.paths.CertPath(constants.CACertName)); after != beforeCA {
		t.Fatal("the CA must not change on renewal")
	}
	if report.CAFingerprints["kubernetes-ca"] != beforeCA {
		t.Fatalf("CA fingerprint report = %q, want %q", report.CAFingerprints["kubernetes-ca"], beforeCA)
	}
	// 重签后的证书由同一个 CA 签发，且主题/SAN 不变。
	newCert := fingerprintOfFile(t, f.paths.CertPath(constants.ControllerManagerClientCertName))
	if newCert == "" {
		t.Fatal("controller-manager certificate missing")
	}
	data, err := os.ReadFile(f.paths.CertPath(constants.ControllerManagerClientCertName))
	if err != nil {
		t.Fatal(err)
	}
	certs, err := pki.ParseCertsPEM(data)
	if err != nil {
		t.Fatal(err)
	}
	if certs[0].Subject.CommonName != "system:kube-controller-manager" {
		t.Fatalf("CN changed: %q", certs[0].Subject.CommonName)
	}
	if !certs[0].NotAfter.After(opts.Now().Add(200 * 24 * time.Hour)) {
		t.Fatalf("renewed certificate expires too early: %s", certs[0].NotAfter)
	}
	// 受影响的组件必须重启。
	if restarter.calls != 1 || strings.Join(restarter.components, ",") != constants.ComponentControllerManager {
		t.Fatalf("restarter = %+v", restarter)
	}
	// 报告必须说明 kubelet 自动轮换。
	joined := strings.Join(report.Notes, " ")
	if !strings.Contains(joined, "kubelet") {
		t.Fatalf("notes must document kubelet's automatic rotation: %v", report.Notes)
	}
}

func TestLocalForceRenewsEverything(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, apiserverLeaf())
	installLeaf(t, f, leafState{
		name:     constants.SchedulerClientCertName,
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "system:kube-scheduler", Usages: pki.UsageClient},
		validity: 300 * 24 * time.Hour,
	})
	opts := testOptions(t, f.paths, testCluster(t))
	opts.Force = true
	restarter := &fakeRestarter{}
	opts.Restarter = restarter

	beforeAPI := fingerprintOfFile(t, f.paths.CertPath(constants.APIServerCertName))
	beforeScheduler := fingerprintOfFile(t, f.paths.CertPath(constants.SchedulerClientCertName))

	report, err := Local(context.Background(), opts)
	if err != nil {
		t.Fatalf("Local --force: %v", err)
	}
	if report.Renewed != 2 || report.Skipped != 0 {
		t.Fatalf("renewed=%d skipped=%d, want 2/0", report.Renewed, report.Skipped)
	}
	if fingerprintOfFile(t, f.paths.CertPath(constants.APIServerCertName)) == beforeAPI {
		t.Fatal("--force must re-sign a valid certificate")
	}
	if fingerprintOfFile(t, f.paths.CertPath(constants.SchedulerClientCertName)) == beforeScheduler {
		t.Fatal("--force must re-sign a valid certificate")
	}
	if strings.Join(restarter.components, ",") != constants.ComponentAPIServer+","+constants.ComponentScheduler {
		t.Fatalf("restarted components = %v", restarter.components)
	}
}

func TestLocalDryRunWritesNothing(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, expiringLeaf())
	opts := testOptions(t, f.paths, testCluster(t))
	opts.DryRun = true
	restarter := &fakeRestarter{}
	opts.Restarter = restarter

	before := fingerprintOfFile(t, f.paths.CertPath(constants.APIServerCertName))
	report, err := Local(context.Background(), opts)
	if err != nil {
		t.Fatalf("Local dry-run: %v", err)
	}
	if report.Renewed != 1 || !report.DryRun {
		t.Fatalf("dry-run report = %+v", report)
	}
	if after := fingerprintOfFile(t, f.paths.CertPath(constants.APIServerCertName)); after != before {
		t.Fatal("dry-run rewrote a certificate")
	}
	if restarter.calls != 0 {
		t.Fatal("dry-run restarted components")
	}
}

func TestLocalRenewsEtcdCertificates(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, leafState{
		name:     "server.crt",
		etcd:     true,
		ca:       "etcd-ca",
		cfg:      pki.CertConfig{CommonName: "m1", DNSNames: []string{"m1", "localhost"}, Usages: pki.UsageServerClient},
		validity: 5 * 24 * time.Hour,
	})
	opts := testOptions(t, f.paths, testCluster(t))
	restarter := &fakeRestarter{}
	opts.Restarter = restarter

	report, err := Local(context.Background(), opts)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if report.Renewed != 1 {
		t.Fatalf("renewed = %d: %+v", report.Renewed, report.Results)
	}
	if strings.Join(restarter.components, ",") != constants.ComponentEtcd {
		t.Fatalf("restarted = %v, want etcd", restarter.components)
	}
	if report.CAFingerprints["etcd-ca"] == "" {
		t.Fatal("etcd CA fingerprint missing")
	}
}

func TestLocalWithoutCAFails(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	opts := testOptions(t, paths, testCluster(t))
	if _, err := Local(context.Background(), opts); err == nil {
		t.Fatal("renewal without a CA must fail")
	} else if !strings.Contains(err.Error(), "CA") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLocalReportsUnknownIssuer(t *testing.T) {
	f := newCAFixture(t)
	// 一张由未知 CA 签发的证书：不能自动续期，必须报告而不是静默跳过。
	unknown := selfSignedCert(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), "kube-apiserver", false)
	writeFile(t, f.paths.CertPath(constants.APIServerCertName), unknown)
	writeFile(t, f.paths.CertPath(strings.TrimSuffix(constants.APIServerCertName, ".crt")+".key"), []byte("not-a-key"))

	opts := testOptions(t, f.paths, testCluster(t))
	report, err := Local(context.Background(), opts)
	if err == nil {
		t.Fatal("a certificate with an unknown issuer must be reported as a failure")
	}
	if report == nil || report.Failed == 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestLocalWithoutRestarterDocumentsManualSteps(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, expiringLeaf())
	opts := testOptions(t, f.paths, testCluster(t))
	report, err := Local(context.Background(), opts)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if len(report.Restarted) != 0 {
		t.Fatalf("no restarter was injected: %+v", report.Restarted)
	}
	if !strings.Contains(strings.Join(report.Notes, " "), "未注入 Restarter") {
		t.Fatalf("the report must document the manual restart: %v", report.Notes)
	}
}

// TestLocalRenewsOnlySelectedCertificate 固定 --cert 过滤：只重签指定证书，
// 其他证书保持不变，重启组件只来自被重签的证书。
func TestLocalRenewsOnlySelectedCertificate(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, expiringLeaf())
	installLeaf(t, f, leafState{
		name:     constants.ControllerManagerClientCertName,
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "system:kube-controller-manager", Usages: pki.UsageClient},
		validity: 10 * 24 * time.Hour,
	})
	opts := testOptions(t, f.paths, testCluster(t))
	opts.Force = true
	opts.CertIDs = []string{"apiserver"}
	restarter := &fakeRestarter{}
	opts.Restarter = restarter

	beforeCM := fingerprintOfFile(t, f.paths.CertPath(constants.ControllerManagerClientCertName))
	report, err := Local(context.Background(), opts)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if report.Renewed != 1 {
		t.Fatalf("renewed = %d，期望 1: %+v", report.Renewed, report.Results)
	}
	if after := fingerprintOfFile(t, f.paths.CertPath(constants.ControllerManagerClientCertName)); after != beforeCM {
		t.Fatal("未选中的 controller-manager 证书不应被重签")
	}
	if restarter.calls != 1 || strings.Join(restarter.components, ",") != constants.ComponentAPIServer {
		t.Fatalf("restarter = %+v，期望只重启 apiserver", restarter)
	}
}

// TestLocalRejectsUnknownCertificateFilter 固定未知 --cert 的失败关闭行为。
func TestLocalRejectsUnknownCertificateFilter(t *testing.T) {
	f := newCAFixture(t)
	opts := testOptions(t, f.paths, testCluster(t))
	opts.CertIDs = []string{"does-not-exist"}
	if _, err := Local(context.Background(), opts); err == nil {
		t.Fatal("未知证书应报错")
	}
}

// TestLocalExplicitMissingCertFails 固定：`--cert` 点名本机没有的证书必须失败，
// 不能静默成功并给出“都在续期窗口之外”的误导提示。
func TestLocalExplicitMissingCertFails(t *testing.T) {
	f := newCAFixture(t)
	opts := testOptions(t, f.paths, testCluster(t))
	opts.CertIDs = []string{string(pki.CertEtcdServer)}

	report, err := Local(context.Background(), opts)
	if err == nil {
		t.Fatalf("显式指定本机不存在的证书必须返回错误: %+v", report)
	}
	if report == nil || report.Failed != 1 || len(report.Results) != 1 {
		t.Fatalf("应记录一条失败结果: %+v", report)
	}
	if !strings.Contains(report.Results[0].Error, "本机没有") {
		t.Fatalf("错误说明 = %q", report.Results[0].Error)
	}
	for _, note := range report.Notes {
		if strings.Contains(note, "无需续期") {
			t.Fatalf("失败时不应给出“无需续期”提示: %v", report.Notes)
		}
	}
}

// TestLocalRenewsKubeletClientRestartsKubelet 固定：kubelet-client 续期后要重启
// kubelet（kubeconfig 里的客户端证书只在进程启动时加载），而不是重启 apiserver。
func TestLocalRenewsKubeletClientRestartsKubelet(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, leafState{
		name:     constants.KubeletClientCertName,
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "system:node:m1", Organizations: []string{"system:nodes"}, Usages: pki.UsageClient},
		validity: 10 * 24 * time.Hour,
	})
	opts := testOptions(t, f.paths, testCluster(t))
	opts.CertIDs = []string{string(pki.CertKubeletClient)}
	restarter := &fakeRestarter{}
	opts.Restarter = restarter

	report, err := Local(context.Background(), opts)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if report.Renewed != 1 {
		t.Fatalf("renewed=%d, want 1", report.Renewed)
	}
	if strings.Join(restarter.components, ",") != constants.ComponentKubelet {
		t.Fatalf("kubelet-client 续期必须重启 kubelet，实际 %v", restarter.components)
	}
}

// TestFingerprintOfCertFileTracksDisk 固定：CA 指纹必须来自磁盘快照。
// 之前用内存里的 CA 对象自比，续期期间的 CA 替换永远检测不到。
func TestFingerprintOfCertFileTracksDisk(t *testing.T) {
	f := newCAFixture(t)
	caFile := f.paths.CertPath(constants.CACertName)
	before := fingerprintOfCertFile(caFile)
	if before == "" {
		t.Fatal("CA 指纹不应为空")
	}
	other, err := pki.NewCA("replaced-ca", pki.ECDSAP256, 0)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := other.WriteToDisk(caFile, f.paths.CertPath(constants.CAKeyName)); err != nil {
		t.Fatalf("WriteToDisk: %v", err)
	}
	if after := fingerprintOfCertFile(caFile); after == before {
		t.Fatal("CA 文件被替换后指纹必须变化")
	}
}
