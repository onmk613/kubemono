package certrenew

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/pki"
)

func TestCheckExpirationScansPKIAndKubeconfig(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, apiserverLeaf())
	installLeaf(t, f, leafState{
		name:     "kubelient.crt",
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "kubemono-etcd-client", Usages: pki.UsageClient},
		validity: 100 * 24 * time.Hour,
	})
	// 一张 etcd 证书。
	installLeaf(t, f, leafState{
		name:     "server.crt",
		etcd:     true,
		ca:       "etcd-ca",
		cfg:      pki.CertConfig{CommonName: "m1", Usages: pki.UsageServerClient},
		validity: 100 * 24 * time.Hour,
	})
	caPEM := readFile(t, f.paths.CertPath(constants.CACertName))
	leafPEM := readFile(t, f.paths.CertPath(constants.APIServerCertName))
	writeKubeconfig(t, f.paths.KubeconfigPath(constants.AdminKubeconfigName), caPEM, leafPEM, "")

	opts := testOptions(t, f.paths, testCluster(t))
	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}
	sources := map[string]int{}
	var sawCA bool
	for _, status := range statuses {
		for _, src := range status.Sources {
			sources[src]++
		}
		if status.Error != "" {
			t.Fatalf("unexpected error for %s: %s", status.Path, status.Error)
		}
		if status.IsCA && status.CommonName == "kubernetes-ca" {
			sawCA = true
		}
		if status.Remaining == "" {
			t.Fatalf("status %s has no remaining lifetime", status.Path)
		}
	}
	if sources["pki"] == 0 || sources["etcd-pki"] == 0 || sources["kubeconfig"] == 0 {
		t.Fatalf("scan did not cover every source: %v", sources)
	}
	if !sawCA {
		t.Fatal("the CA certificate must be reported")
	}
}

func TestCheckExpirationWarningWindow(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, expiringLeaf()) // 10 天：落在 30 天窗口内 / 10 days: within the 30-day window
	installLeaf(t, f, leafState{
		name:     constants.SchedulerClientCertName,
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "system:kube-scheduler", Usages: pki.UsageClient},
		validity: 60 * 24 * time.Hour, // 窗口外 / outside the window
	})
	opts := testOptions(t, f.paths, testCluster(t))
	opts.WarnBefore = 30 * 24 * time.Hour

	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}
	byName := map[string]CertStatus{}
	for _, status := range statuses {
		byName[status.Name] = status
	}
	if !byName[constants.APIServerCertName].Warn || byName[constants.APIServerCertName].Expired {
		t.Fatalf("10-day certificate must warn: %+v", byName[constants.APIServerCertName])
	}
	if byName[constants.SchedulerClientCertName].Warn {
		t.Fatalf("60-day certificate must not warn: %+v", byName[constants.SchedulerClientCertName])
	}
	if len(Expiring(statuses)) != 1 {
		t.Fatalf("Expiring = %d, want 1", len(Expiring(statuses)))
	}

	// 边界：把窗口放宽到 90 天，两张都在窗口内。
	opts.WarnBefore = 90 * 24 * time.Hour
	statuses, err = CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}
	for _, status := range statuses {
		if status.IsCA {
			continue
		}
		if !status.Warn {
			t.Fatalf("90-day window must warn for %s", status.Path)
		}
	}
}

func TestCheckExpirationExpired(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, apiserverLeaf())
	opts := testOptions(t, f.paths, testCluster(t))
	// 把时钟拨到叶子证书之后（CA 是 10 年，仍然有效）。
	opts.Now = func() time.Time { return time.Now().Add(400 * 24 * time.Hour) }

	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}
	var expired int
	for _, status := range statuses {
		if status.IsCA {
			continue
		}
		if !status.Expired {
			t.Fatalf("certificate %s must be reported as expired: %+v", status.Path, status)
		}
		if !strings.Contains(status.Remaining, "已过期") {
			t.Fatalf("remaining = %q, want an expired marker", status.Remaining)
		}
		expired++
	}
	if expired == 0 {
		t.Fatal("no expired certificate was reported")
	}
}

func TestCheckExpirationKubeconfigFileReference(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, apiserverLeaf())
	caPEM := readFile(t, f.paths.CertPath(constants.CACertName))
	// kubeconfig 用相对路径引用证书文件（kubemono 实际写绝对路径）：
	// 扫描时必须按 kubeconfig 所在目录解析。
	writeKubeconfig(t, f.paths.KubeconfigPath(constants.AdminKubeconfigName), caPEM, nil,
		filepath.Join("pki", constants.APIServerCertName))

	opts := testOptions(t, f.paths, testCluster(t))
	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}
	want := f.paths.CertPath(constants.APIServerCertName)
	var found *CertStatus
	for i := range statuses {
		if statuses[i].Path == want && slices.Contains(statuses[i].Sources, "kubeconfig") {
			found = &statuses[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("kubeconfig file reference was not resolved to %s: %+v", want, statuses)
	}
	// admin.conf 引用了 apiserver.crt，而 kubemono 期望 admin.crt：必须报不一致。
	if found.Mismatch == "" {
		t.Fatalf("kubeconfig 引用错误的证书路径时必须标 Mismatch: %+v", found)
	}
}

// TestCheckExpirationAggregatesCertificateReferences 固定聚合语义：同一张证书
// 被 PKI 与多个 kubeconfig 引用只出一行，引用列表完整。
func TestCheckExpirationAggregatesCertificateReferences(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, leafState{
		name:     constants.AdminClientCertName,
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "kubemono-admin", Usages: pki.UsageClient},
		validity: 300 * 24 * time.Hour,
	})
	installLeaf(t, f, leafState{
		name:     constants.SchedulerClientCertName,
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "system:kube-scheduler", Usages: pki.UsageClient},
		validity: 300 * 24 * time.Hour,
	})
	caFile := f.paths.CertPath(constants.CACertName)
	writeFileRefKubeconfig(t, f.paths.KubeconfigPath(constants.AdminKubeconfigName), caFile,
		f.paths.CertPath(constants.AdminClientCertName))
	writeFileRefKubeconfig(t, f.paths.KubeconfigPath(constants.SchedulerKubeconfigName), caFile,
		f.paths.CertPath(constants.SchedulerClientCertName))

	opts := testOptions(t, f.paths, testCluster(t))
	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}

	var caRows int
	for _, st := range statuses {
		switch {
		case st.IsCA && st.CommonName == "kubernetes-ca":
			caRows++
			if !slices.Contains(st.Sources, "pki") || !slices.Contains(st.Sources, "kubeconfig") {
				t.Errorf("CA 行应同时来自 pki 与 kubeconfig: %+v", st.Sources)
			}
			if len(st.Kubeconfigs) != 2 {
				t.Errorf("CA 行应聚合两个 kubeconfig，实际 %v", st.Kubeconfigs)
			}
		case st.Path == f.paths.CertPath(constants.AdminClientCertName):
			if len(st.Kubeconfigs) != 1 || filepath.Base(st.Kubeconfigs[0]) != constants.AdminKubeconfigName {
				t.Errorf("admin.crt 的 kubeconfig 引用 = %v", st.Kubeconfigs)
			}
			if st.Mismatch != "" {
				t.Errorf("一致的引用不应报 mismatch: %s", st.Mismatch)
			}
		}
	}
	if caRows != 1 {
		t.Fatalf("CA 应聚合为 1 行，实际 %d 行: %+v", caRows, statuses)
	}
}

// TestCheckExpirationFlagsKubeconfigCAMismatch 固定：kubeconfig 内嵌的 CA 与本机
// ca.crt 不一致时必须报出来（典型场景：从别的集群拷来的 kubeconfig）。
func TestCheckExpirationFlagsKubeconfigCAMismatch(t *testing.T) {
	f := newCAFixture(t)
	installLeaf(t, f, leafState{
		name:     constants.AdminClientCertName,
		ca:       "kubernetes-ca",
		cfg:      pki.CertConfig{CommonName: "kubemono-admin", Usages: pki.UsageClient},
		validity: 300 * 24 * time.Hour,
	})
	otherCA := selfSignedCert(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), "other-ca", true)
	writeKubeconfig(t, f.paths.KubeconfigPath(constants.AdminKubeconfigName), otherCA, nil,
		f.paths.CertPath(constants.AdminClientCertName))

	opts := testOptions(t, f.paths, testCluster(t))
	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}
	var found bool
	for _, st := range statuses {
		if st.IsCA && st.Mismatch != "" && strings.Contains(st.Path, "#") {
			found = true
		}
	}
	if !found {
		t.Fatalf("kubeconfig 内嵌的 CA 与本机 ca.crt 不一致时必须报 Mismatch: %+v", statuses)
	}
}

// TestCheckExpirationAllowsKubeletManagedClientCert 固定：worker 的 kubelet.conf
// 引用 kubelet 自己维护的证书（TLS Bootstrap 产生）不算不一致。
func TestCheckExpirationAllowsKubeletManagedClientCert(t *testing.T) {
	f := newCAFixture(t)
	kubeletPKI := filepath.Join(f.paths.Kubelet(), "pki")
	kubeletCert := filepath.Join(kubeletPKI, "kubelet-client-current.pem")
	f.writeLeaf(t, "kubernetes-ca", pki.CertConfig{
		CommonName:    "system:node:m1",
		Organizations: []string{"system:nodes"},
		Usages:        pki.UsageClient,
	}, kubeletCert, filepath.Join(kubeletPKI, "kubelet-client-current.key"))
	writeFileRefKubeconfig(t, f.paths.KubeconfigPath(constants.KubeletKubeconfigName),
		f.paths.CertPath(constants.CACertName), kubeletCert)

	opts := testOptions(t, f.paths, testCluster(t))
	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}
	for _, st := range statuses {
		if st.Path == kubeletCert && st.Mismatch != "" {
			t.Fatalf("kubelet 自己维护的证书不应报 mismatch: %s", st.Mismatch)
		}
	}
}

func TestCheckExpirationEmptyNode(t *testing.T) {
	opts := testOptions(t, constants.NewPaths(t.TempDir()), testCluster(t))
	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration: %v", err)
	}
	if len(statuses) != 0 {
		t.Fatalf("expected no certificates, got %+v", statuses)
	}
}

func TestCheckExpirationBrokenCertificate(t *testing.T) {
	f := newCAFixture(t)
	writeFile(t, f.paths.CertPath(constants.APIServerCertName), []byte("not a certificate"))
	opts := testOptions(t, f.paths, testCluster(t))
	statuses, err := CheckExpiration(context.Background(), opts)
	if err != nil {
		t.Fatalf("CheckExpiration must report per-file errors, not fail: %v", err)
	}
	var broken bool
	for _, status := range statuses {
		if status.Path == f.paths.CertPath(constants.APIServerCertName) && status.Error != "" {
			broken = true
		}
	}
	if !broken {
		t.Fatalf("broken certificate not reported: %+v", statuses)
	}
	if !strings.HasPrefix(filepath.Base(f.paths.CertPath(constants.APIServerCertName)), "apiserver") {
		t.Fatal("fixture name changed")
	}
}
