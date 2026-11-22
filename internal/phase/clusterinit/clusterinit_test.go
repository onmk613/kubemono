package clusterinit

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"kubemono/internal/bom"
	"kubemono/internal/clusterinit"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/phase"
	"kubemono/internal/util/output"
)

func testContext(t *testing.T, node string) (*clusterconfig.Cluster, *phase.Context) {
	t.Helper()
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "m2", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
	}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost(node)
	pc := &phase.Context{
		Cluster:    cluster,
		Host:       host,
		ClusterID:  "cid-1234",
		IsInitNode: node == "m1",
		BOM:        bom.MustLoad(),
		Paths:      constants.NewPaths(t.TempDir()),
		Release:    bom.MustLoad().Release,
		Report:     output.Discard(),
		Now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	// 真实路径要求集群 CA 在位（cluster-info 必须内嵌它）。
	caPath := pc.Paths.CertPath(constants.CACertName)
	if err := os.MkdirAll(pc.Paths.PKI(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\nZm9v\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cluster, pc
}

func TestAppliesToInitNodeAndInstalledMasters(t *testing.T) {
	_, pc := testContext(t, "m1")
	if !(&Phase{}).Applies(pc) {
		t.Fatal("init 节点必须运行 cluster-init")
	}

	// 已安装 master（node repair）：必须能补回集群级资源，否则半装集群卡在 RBAC。
	_, master := testContext(t, "m2")
	if master.IsInitNode {
		t.Fatal("测试前提错误：m2 不应是 init 节点")
	}
	if !(&Phase{}).Applies(master) {
		t.Fatal("已安装 master 在 repair 时必须运行 cluster-init")
	}

	// clusterID 未知时不能写 kubemono-cluster：那会把集群身份写成空值。
	noID := *master
	noID.ClusterID = ""
	if (&Phase{}).Applies(&noID) {
		t.Fatal("clusterID 为空时不应运行 cluster-init（会把集群身份写成空值）")
	}

	// worker 不维护集群级资源。
	worker := *master
	worker.Host = &clusterconfig.Host{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}}
	if (&Phase{}).Applies(&worker) {
		t.Fatal("worker 不应运行 cluster-init")
	}
}

// TestRepairHealsMissingClusterObjects 守住 node repair 的承诺：
// 半装 master 上缺 RBAC/命名空间时，repair 必须能把它们补回来。
// 该路径之所以可达，是因为 Applies 覆盖了"已安装 master"（只认 init 节点时
// 这一项在 repair 里永远是空操作）。
func TestRepairHealsMissingClusterObjects(t *testing.T) {
	_, pc := testContext(t, "m2") // 非 init 的 master
	// 模拟 node repair：没有 join 材料。
	pc.JoinSecret = nil
	pc.IsInitNode = false

	client := fake.NewSimpleClientset()
	p := &Phase{Client: client}
	ctx := context.Background()

	if !p.Applies(pc) {
		t.Fatal("已安装 master 在 repair 时必须适用 cluster-init")
	}
	done, err := p.Check(ctx, pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("空集群不应报告完成")
	}
	if err := p.Run(ctx, pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	done, err = p.Check(ctx, pc)
	if err != nil {
		t.Fatalf("Check after Run: %v", err)
	}
	if !done {
		t.Fatal("Run 之后 Check 必须报告完成（repair 能补回集群级对象）")
	}
	// repair 不应碰引导令牌：没有 join 材料时不该建出任何 bootstrap-token Secret。
	list, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range list.Items {
		if strings.HasPrefix(sec.Name, "bootstrap-token-") {
			t.Fatalf("repair 不应创建引导令牌 Secret：%s", sec.Name)
		}
	}
}

func TestRunCreatesResources(t *testing.T) {
	_, pc := testContext(t, "m1")
	client := fake.NewSimpleClientset()
	p := &Phase{Client: client}
	ctx := context.Background()

	done, err := p.Check(ctx, pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("空集群不应报告完成")
	}
	if err := p.Run(ctx, pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	done, err = p.Check(ctx, pc)
	if err != nil || !done {
		t.Fatalf("Run 后 Check(done=%v err=%v)", done, err)
	}
	// 集群状态必须存在。
	if _, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Get(ctx, constants.ClusterStateName, metav1.GetOptions{}); err != nil {
		t.Fatalf("kubemono-cluster 未写入: %v", err)
	}
	// 幂等：再跑一次不应报错。
	if err := p.Run(ctx, pc); err != nil {
		t.Fatalf("重复 Run 必须幂等: %v", err)
	}
}

func TestDryRunCreatesNothing(t *testing.T) {
	_, pc := testContext(t, "m1")
	pc.DryRun = true
	client := fake.NewSimpleClientset()
	p := &Phase{Client: client}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := client.RbacV1().ClusterRoles().Get(context.Background(), "kubemono:cluster-admins", metav1.GetOptions{}); err == nil {
		t.Fatal("dry-run 不得创建任何对象")
	}
}

func TestMissingClientIsReported(t *testing.T) {
	_, pc := testContext(t, "m1")
	p := &Phase{}
	if err := p.Run(context.Background(), pc); err == nil {
		t.Fatal("缺少客户端必须报错")
	} else if !strings.Contains(err.Error(), "admin kubeconfig") {
		t.Fatalf("错误信息应提示需要 admin kubeconfig: %v", err)
	}
	if _, err := p.Check(context.Background(), pc); err == nil {
		t.Fatal("Check 缺少客户端也必须报错")
	}
}

func TestInjectedResourcesOverride(t *testing.T) {
	_, pc := testContext(t, "m1")
	client := fake.NewSimpleClientset()
	rs, err := buildTestResources()
	if err != nil {
		t.Fatal(err)
	}
	p := &Phase{Client: client, Resources: rs}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Get(context.Background(), constants.ClusterStateName, metav1.GetOptions{}); err != nil {
		t.Fatalf("注入的资源未被应用: %v", err)
	}
}

// buildTestResources 为一个最小集群构造默认资源集。
func buildTestResources() (*clusterinit.ResourceSet, error) {
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}}}
	clusterconfig.SetDefaults(cluster)
	return clusterinit.Build(clusterinit.Options{
		Cluster:   cluster,
		ClusterID: "cid-1234",
		Release:   bom.MustLoad().Release,
	})
}

// TestSuperAdminClientUsesSuperAdminConf 覆盖真机踩到的坑：cluster-init 的
// 引导写入（命名空间、RBAC、ConfigMap）必须用 super-admin.conf（O=system:masters）。
// admin.conf 的身份是 kubemono-admin，它的 kubemono:cluster-admins → cluster-admin 绑定
// 正是本 phase 创建的，用 admin.conf 第一步就 403。
func TestSuperAdminClientUsesSuperAdminConf(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	if err := os.MkdirAll(paths.Kubernetes(), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := `apiVersion: v1
kind: Config
clusters:
  - name: c
    cluster: { server: "https://127.0.0.1:6443", insecure-skip-tls-verify: true }
contexts:
  - name: ctx
    context: { cluster: c, user: u }
current-context: ctx
users:
  - name: u
    user: { token: test }
`
	if err := os.WriteFile(paths.KubeconfigPath(constants.SuperAdminKubeconfigName), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	cli, err := superAdminClient(&phase.Context{Paths: paths})
	if err != nil {
		t.Fatalf("superAdminClient: %v", err)
	}
	if cli == nil {
		t.Fatal("存在 super-admin.conf 时必须返回 system:masters 客户端")
	}

	// 文件不存在时返回 (nil, nil)：调用方回退到 phase 上下文的客户端。
	cli2, err := superAdminClient(&phase.Context{Paths: constants.NewPaths(t.TempDir())})
	if err != nil || cli2 != nil {
		t.Fatalf("没有 super-admin.conf 时应回退，实际 cli=%v err=%v", cli2, err)
	}
}
