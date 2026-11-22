package kubeconfig

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	kubeconfigpkg "kubemono/internal/kubeconfig"
	"kubemono/internal/phase"
	"kubemono/internal/pki"
	"kubemono/internal/util/output"
)

func testCluster(mode string) *clusterconfig.Cluster {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(c)
	return c
}

func testContext(t *testing.T, cluster *clusterconfig.Cluster, node string) *phase.Context {
	t.Helper()
	host, _ := cluster.Cluster.Hosts.FindHost(node)
	return &phase.Context{
		Cluster: cluster,
		Host:    host,
		Paths:   constants.NewPaths(t.TempDir()),
		Release: "v1.31.4-km.1",
		Report:  output.Discard(),
		Now:     func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// prepareCA 运行 certs phase，确保 CA 存在。
func prepareCA(t *testing.T, c *phase.Context) {
	t.Helper()
	ca, err := pki.NewCA("kubernetes-ca", pki.RSA2048, constants.DefaultCATTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.WriteToDisk(c.Paths.CertPath(constants.CACertName), c.Paths.CertPath(constants.CAKeyName)); err != nil {
		t.Fatal(err)
	}
}

func TestMasterCreatesUserKubeconfigSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "m1")
	prepareCA(t, c)
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	link := filepath.Join(home, ".kube", "config")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("~/.kube/config 未创建: %v", err)
	}
	if target != c.Paths.KubeconfigPath(constants.AdminKubeconfigName) {
		t.Fatalf("软链目标 = %s", target)
	}
	if done, err := (&Phase{}).Check(context.Background(), c); err != nil || !done {
		t.Fatalf("Check after Run: done=%v err=%v", done, err)
	}
}

func TestDedicatedMasterHasNoKubeletKubeconfig(t *testing.T) {
	cluster := testCluster("staticpod")
	cluster.Cluster.Hosts[0].Roles = []clusterconfig.Role{clusterconfig.RoleMaster}
	c := testContext(t, cluster, "m1")
	prepareCA(t, c)
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(c.Paths.KubeconfigPath(constants.KubeletKubeconfigName)); !os.IsNotExist(err) {
		t.Fatalf("纯控制面 master 不应生成 kubelet.conf，err=%v", err)
	}
	if _, err := os.Stat(c.Paths.KubeconfigPath(constants.AdminKubeconfigName)); err != nil {
		t.Fatalf("admin.conf 应存在: %v", err)
	}
}

func TestAppliesEverywhere(t *testing.T) {
	cluster := testCluster("staticpod")
	for _, node := range []string{"m1", "w1"} {
		c := testContext(t, cluster, node)
		if !(&Phase{}).Applies(c) {
			t.Fatalf("%s 必须有 kubeconfig", node)
		}
	}
}

func TestMasterKubeconfigs(t *testing.T) {
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "m1")
	prepareCA(t, c)
	ctx := context.Background()

	done, err := (&Phase{}).Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("fresh master must not report the phase as done")
	}
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	done, err = (&Phase{}).Check(ctx, c)
	if err != nil || !done {
		t.Fatalf("Check after Run: done=%v err=%v", done, err)
	}

	// 每个文件都是 0600，且都指向本机 127.0.0.1:6443。
	wantConfs := map[string]string{
		constants.AdminKubeconfigName:             constants.UserAdmin,
		constants.SuperAdminKubeconfigName:        constants.UserSuperAdmin,
		constants.ControllerManagerKubeconfigName: constants.UserControllerManager,
		constants.SchedulerKubeconfigName:         constants.UserScheduler,
		constants.KubeletKubeconfigName:           "system:node:m1",
	}
	for conf, user := range wantConfs {
		path := c.Paths.KubeconfigPath(conf)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s 未生成: %v", conf, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s 权限 %04o，期望 0600", conf, info.Mode().Perm())
		}
		cfg, err := loadKubeconfig(t, path)
		if err != nil {
			t.Fatalf("%s: %v", conf, err)
		}
		if cfg.server != "https://127.0.0.1:6443" {
			t.Errorf("%s server = %s", conf, cfg.server)
		}
		if cfg.user != user {
			t.Errorf("%s user = %s, want %s", conf, cfg.user, user)
		}
	}

	// 身份分组必须正确：admin 走 RBAC（可撤销），super-admin 走 system:masters。
	assertSubject(t, c.Paths.CertPath(constants.AdminClientCertName), constants.UserAdmin, []string{constants.GroupClusterAdmins})
	assertSubject(t, c.Paths.CertPath(constants.SuperAdminClientCertName), constants.UserSuperAdmin, []string{constants.GroupSystemMasters})
	assertSubject(t, c.Paths.CertPath(constants.KubeletClientCertName), "system:node:m1", []string{constants.GroupSystemNodes})
	assertSubject(t, c.Paths.CertPath(constants.ControllerManagerClientCertName), constants.UserControllerManager, nil)
	assertSubject(t, c.Paths.CertPath(constants.SchedulerClientCertName), constants.UserScheduler, nil)

	// 客户端证书私钥 0600、证书 0644。
	assertMode(t, c.Paths.CertPath(constants.AdminClientKeyName), 0o600)
	assertMode(t, c.Paths.CertPath(constants.AdminClientCertName), 0o644)
}

func TestWorkerOnlyGetsBootstrapKubeconfig(t *testing.T) {
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "w1")
	prepareCA(t, c)
	c.JoinSecret = &clusterconfig.JoinSecret{Role: clusterconfig.RoleWorker, Token: "abcdef.0123456789abcdef"}
	ctx := context.Background()

	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	bootPath := c.Paths.KubeconfigPath(constants.BootstrapKubeletKubeconfigName)
	if _, err := os.Stat(bootPath); err != nil {
		t.Fatalf("worker 缺少 bootstrap-kubelet.conf: %v", err)
	}
	cfg, err := loadKubeconfig(t, bootPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.user != constants.UserKubeletBootstrap {
		t.Fatalf("bootstrap user = %s", cfg.user)
	}
	if cfg.token != "abcdef.0123456789abcdef" {
		t.Fatalf("bootstrap token = %q", cfg.token)
	}
	// worker 不应该拿到管理员凭据，也不应该有本机签发的客户端证书。
	for _, conf := range []string{constants.AdminKubeconfigName, constants.SuperAdminKubeconfigName, constants.KubeletKubeconfigName} {
		if _, err := os.Stat(c.Paths.KubeconfigPath(conf)); !os.IsNotExist(err) {
			t.Errorf("worker 不应生成 %s", conf)
		}
	}
	for _, cert := range []string{constants.KubeletClientCertName} {
		if _, err := os.Stat(c.Paths.CertPath(cert)); !os.IsNotExist(err) {
			t.Errorf("worker 不应持有本机签发的 %s", cert)
		}
	}
	// 引导地址必须是**可直连**的 master，不能是 127.0.0.1：worker 的本地代理
	// 本身是 kubelet 管的静态 Pod，用回环地址引导会死锁（真机实测：kubelet 报
	if cfg.server == "" || strings.Contains(cfg.server, "127.0.0.1") {
		t.Fatalf("worker 的 bootstrap 地址不能是回环：%q", cfg.server)
	}
	if !strings.HasPrefix(cfg.server, "https://") {
		t.Fatalf("bootstrap 地址必须是 https：%q", cfg.server)
	}

	// 已存在 bootstrap 文件即视为完成。
	done, err := (&Phase{}).Check(ctx, c)
	if err != nil || !done {
		t.Fatalf("Check(done=%v err=%v)", done, err)
	}
}

func TestWorkerWithoutJoinTokenFails(t *testing.T) {
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "w1")
	prepareCA(t, c)
	err := (&Phase{}).Run(context.Background(), c)
	if err == nil {
		t.Fatal("缺少 bootstrap token 时必须报错")
	}
	if !contains(err.Error(), "--token") {
		t.Fatalf("错误信息应提示 --token: %v", err)
	}
}

func TestMissingCAIsReported(t *testing.T) {
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "m1")
	err := (&Phase{}).Run(context.Background(), c)
	if err == nil {
		t.Fatal("缺少 CA 时必须报错")
	}
	if !contains(err.Error(), "certs") {
		t.Fatalf("错误信息应提示先执行 certs phase: %v", err)
	}
}

func TestRunIsIdempotentAndKeepsCredentials(t *testing.T) {
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "m1")
	prepareCA(t, c)
	ctx := context.Background()
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatal(err)
	}
	certBefore, err := os.ReadFile(c.Paths.CertPath(constants.AdminClientCertName))
	if err != nil {
		t.Fatal(err)
	}
	confBefore, err := os.ReadFile(c.Paths.KubeconfigPath(constants.AdminKubeconfigName))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	certAfter, _ := os.ReadFile(c.Paths.CertPath(constants.AdminClientCertName))
	if string(certBefore) != string(certAfter) {
		t.Fatal("重复执行不得重签客户端证书")
	}
	confAfter, _ := os.ReadFile(c.Paths.KubeconfigPath(constants.AdminKubeconfigName))
	if string(confBefore) != string(confAfter) {
		t.Fatal("重复执行不得改变 kubeconfig 内容")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "m1")
	prepareCA(t, c)
	c.DryRun = true
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(c.Paths.KubeconfigPath(constants.AdminKubeconfigName)); !os.IsNotExist(err) {
		t.Fatal("dry-run 不得写文件")
	}
	if _, err := os.Stat(c.Paths.CertPath(constants.AdminClientCertName)); !os.IsNotExist(err) {
		t.Fatal("dry-run 不得签发证书")
	}
}

type parsedKubeconfig struct {
	server string
	user   string
	token  string
}

func kubeconfigLoad(path string) (parsedKubeconfig, error) {
	cfg, err := kubeconfigpkg.Load(path)
	if err != nil {
		return parsedKubeconfig{}, err
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx == nil {
		return parsedKubeconfig{}, fmt.Errorf("%s: 没有当前上下文", path)
	}
	out := parsedKubeconfig{user: ctx.AuthInfo}
	if cluster := cfg.Clusters[ctx.Cluster]; cluster != nil {
		out.server = cluster.Server
	}
	if ai := cfg.AuthInfos[ctx.AuthInfo]; ai != nil {
		out.token = ai.Token
	}
	return out, nil
}

func loadKubeconfig(t *testing.T, path string) (parsedKubeconfig, error) {
	t.Helper()
	return kubeconfigLoad(path)
}

func assertSubject(t *testing.T, certPath, cn string, groups []string) {
	t.Helper()
	cert := readCert(t, certPath)
	if cert.Subject.CommonName != cn {
		t.Errorf("%s CN = %s, want %s", certPath, cert.Subject.CommonName, cn)
	}
	if len(groups) != len(cert.Subject.Organization) {
		t.Fatalf("%s O = %v, want %v", certPath, cert.Subject.Organization, groups)
	}
	for i, g := range groups {
		if cert.Subject.Organization[i] != g {
			t.Errorf("%s O[%d] = %s, want %s", certPath, i, cert.Subject.Organization[i], g)
		}
	}
	// 客户端证书必须能用于客户端认证。
	ok := false
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			ok = true
		}
	}
	if !ok {
		t.Errorf("%s 缺少 clientAuth 用途", certPath)
	}
}

func assertMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if info.Mode().Perm() != mode {
		t.Errorf("%s 权限 %04o，期望 %04o", path, info.Mode().Perm(), mode)
	}
}

func readCert(t *testing.T, path string) *x509.Certificate {
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

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestCheckDetectsServerDriftAndRunRepairs(t *testing.T) {
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "m1")
	prepareCA(t, c)
	ctx := context.Background()
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	adminPath := c.Paths.KubeconfigPath(constants.AdminKubeconfigName)
	cfg, err := kubeconfigpkg.Load(adminPath)
	if err != nil {
		t.Fatal(err)
	}
	cur := cfg.Contexts[cfg.CurrentContext]
	if cur == nil || cfg.Clusters[cur.Cluster] == nil {
		t.Fatal("admin.conf 缺少当前上下文")
	}
	cfg.Clusters[cur.Cluster].Server = "https://wrong.example.com:6443"
	if err := kubeconfigpkg.WriteToFile(adminPath, cfg); err != nil {
		t.Fatal(err)
	}
	done, err := (&Phase{}).Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("server 漂移后 Check 必须为 false")
	}
	if err := (&Phase{}).Run(ctx, c); err != nil {
		t.Fatalf("repair Run: %v", err)
	}
	cfg, err = kubeconfigpkg.Load(adminPath)
	if err != nil {
		t.Fatal(err)
	}
	ctxCfg := cfg.Contexts[cfg.CurrentContext]
	if ctxCfg == nil {
		t.Fatal("missing current context")
	}
	if got := cfg.Clusters[ctxCfg.Cluster].Server; got == "https://wrong.example.com:6443" {
		t.Fatalf("Run 未修复 server 漂移: %q", got)
	}
}

func TestMasterKubeletUsesNodeName(t *testing.T) {
	cluster := testCluster("staticpod")
	c := testContext(t, cluster, "m1")
	c.Host.NodeName = "m1.example.com"
	c.NodeNameValue = "m1.example.com"
	prepareCA(t, c)
	if err := (&Phase{}).Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cfg, err := loadKubeconfig(t, c.Paths.KubeconfigPath(constants.KubeletKubeconfigName))
	if err != nil {
		t.Fatalf("kubelet.conf: %v", err)
	}
	if cfg.user != "system:node:m1.example.com" {
		t.Fatalf("kubelet.conf user = %q, want system:node:m1.example.com", cfg.user)
	}
	assertSubject(t, c.Paths.CertPath(constants.KubeletClientCertName), "system:node:m1.example.com", []string{constants.GroupSystemNodes})
}

func TestBootstrapUsesExternalEndpointsAndDisablesProxy(t *testing.T) {
	cluster := testCluster("staticpod")
	cluster.ControlPlane.ExternalEndpoints = []string{"vip.example.com:6443"}
	c := testContext(t, cluster, "w1")
	if got := serverForBootstrap(c); got != "https://vip.example.com:6443" {
		t.Fatalf("serverForBootstrap = %q，期望 externalEndpoints", got)
	}
	if cluster.ControlPlane.LocalProxyEnabled() {
		t.Fatal("设置 externalEndpoints 后不应再部署 worker 本地代理")
	}
}
