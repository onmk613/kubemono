package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"kubemono/internal/bom"
	"kubemono/internal/bootstrap"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/discovery"
	"kubemono/internal/identity"
	"kubemono/internal/network"
	"kubemono/internal/phase"
	_ "kubemono/internal/phase/etcdhealth"
	"kubemono/internal/render"
	"kubemono/internal/util/output"
)

func testCluster() *clusterconfig.Cluster {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}},
		{Name: "m2", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(c)
	return c
}

func testOptions(t *testing.T, node string) *Options {
	t.Helper()
	paths := constants.NewPaths(t.TempDir())
	return &Options{
		Cluster:              testCluster(),
		Release:              bom.MustLoad().Release,
		BOM:                  bom.MustLoad(),
		Node:                 node,
		AllowAddressMismatch: true,
		Paths:                paths,
		Report:               output.Discard(),
		Now:                  func() time.Time { return time.Unix(1700000000, 0).UTC() },
		Sleep:                func(time.Duration) {},
		// 默认按 join 建计划；需要 init/sync 的用例显式覆盖 ForceAction。
		ForceAction: ActionJoin,
		Resolver: &identity.Resolver{
			NodeConfigPath: paths.NodeConfig(),
			Hostname:       func() (string, error) { return "m1", nil },
		},
	}
}

func initOptions(t *testing.T, node string) *Options {
	o := testOptions(t, node)
	o.ForceAction = ActionInit
	return o
}

// TestJoinRefusesPlaintextEndpoint 验证 join 不会把引导令牌发到明文 http://。
func TestJoinRefusesPlaintextEndpoint(t *testing.T) {
	opts := testOptions(t, "m2")
	opts.JoinSecret = &clusterconfig.JoinSecret{
		Role:  clusterconfig.RoleMaster,
		Token: "abcdef.0123456789abcdef",
	}
	opts.Cluster.ControlPlane.ExternalEndpoints = []string{"http://10.0.0.1:6443"}
	token, err := bootstrap.ParseToken(opts.JoinSecret.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opts.joinServer(context.Background(), token); err == nil || !strings.Contains(err.Error(), "http://") {
		t.Fatalf("明文端点必须被拒绝，got %v", err)
	}
}

func TestDecideInit(t *testing.T) {
	opts := initOptions(t, "m1")
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != ActionInit {
		t.Fatalf("action = %s, want init", plan.Action)
	}
	if !plan.IsMaster {
		t.Fatal("m1 应该是 master")
	}
	want := []string{"preflight", "host-init", "runtime", "certs", "kubeconfig", "kubelet", "etcd", "etcd-health", "control-plane", "cluster-init", "mark-control-plane", "network", "addon"}
	if strings.Join(plan.Phases, ",") != strings.Join(want, ",") {
		t.Fatalf("phases = %v, want %v", plan.Phases, want)
	}
}

func TestDecideDedicatedMasterHasNoKubelet(t *testing.T) {
	opts := initOptions(t, "m1")
	opts.Cluster.Cluster.Hosts[0].Roles = []clusterconfig.Role{clusterconfig.RoleMaster}
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	joined := strings.Join(plan.Phases, ",")
	for _, forbidden := range []string{"runtime", "kubelet", "mark-control-plane"} {
		if slices.Contains(plan.Phases, forbidden) {
			t.Fatalf("纯控制面 master 不应包含 %s：%v", forbidden, plan.Phases)
		}
	}
	for _, required := range []string{"certs", "kubeconfig", "etcd", "control-plane", "cluster-init", "network", "addon"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("纯控制面 master 缺少 %s：%v", required, plan.Phases)
		}
	}
}

// TestDecideHalfInstalledStaysInit 覆盖真机踩到的坑：分步安装（--phases 只跑
// 前置阶段）会写 node.yaml，但本机还没有集群数据。此时完整 install 必须继续
// init（跑 etcd/control-plane/cluster-init），不能当成"已安装 → sync"——
// sync 的计划里没有 etcd 与 cluster-init，集群永远建不起来。
func TestDecideForcedSyncRequiresInstalledNode(t *testing.T) {
	opts := testOptions(t, "m1")
	opts.ForceAction = ActionSync
	_, err := Decide(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "尚未安装") {
		t.Fatalf("未安装节点执行 repair 必须被拒绝，实际 %v", err)
	}
}

func TestDecideForcedSyncUsesRepairPhases(t *testing.T) {
	opts := testOptions(t, "m1")
	opts.ClusterID = "cid-1"
	opts.ForceAction = ActionSync
	store := &identity.Store{Dir: opts.Paths.Kubemono()}
	if err := store.Save(&clusterconfig.NodeConfig{ClusterID: "cid-1", NodeName: "m1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}}); err != nil {
		t.Fatal(err)
	}
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != ActionSync {
		t.Fatalf("action = %s, want sync", plan.Action)
	}
	for _, p := range plan.Phases {
		if p == "preflight" {
			t.Fatalf("已安装节点 repair 不应重跑 %s：%v", p, plan.Phases)
		}
	}
	if !slices.Contains(plan.Phases, "etcd") || !slices.Contains(plan.Phases, "cluster-init") {
		t.Fatalf("master repair 必须包含 etcd/cluster-init：%v", plan.Phases)
	}
}

func TestDecideResumesInitAfterCertsFailure(t *testing.T) {
	opts := initOptions(t, "m1")
	// 上一次 init 在 certs 阶段失败：没有 node.yaml、没有 etcd 数据，只有 PKI。
	if err := os.MkdirAll(opts.Paths.PKI(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opts.Paths.PKI(), "ca.crt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("仅有 PKI 残留时应允许续跑 init（复用 CA）: %v", err)
	}
	if plan.Action != ActionInit {
		t.Fatalf("action = %s", plan.Action)
	}

	// 真有 etcd 数据才说明存在旧集群，必须 --force-init。
	if err := os.MkdirAll(opts.Paths.EtcdData(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opts.Paths.EtcdData(), "member"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Decide(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "--force-init") {
		t.Fatalf("etcd 数据存在时必须要求 --force-init: %v", err)
	}
	opts.ForceInit = true
	plan, err = Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("--force-init 后应允许 init: %v", err)
	}
	if plan.Action != ActionInit {
		t.Fatalf("action = %s", plan.Action)
	}
}

func TestWorkerPhaseList(t *testing.T) {
	opts := testOptions(t, "w1")
	opts.ClusterID = "cid-1"
	opts.JoinSecret = &clusterconfig.JoinSecret{Role: clusterconfig.RoleWorker, Token: "abcdef.0123456789abcdef"}
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"preflight", "host-init", "runtime", "kubeconfig", "local-proxy", "kubelet", "mark-control-plane", "network"}
	if strings.Join(plan.Phases, ",") != strings.Join(want, ",") {
		t.Fatalf("worker phases = %v, want %v", plan.Phases, want)
	}
}

// stubPhase 记录执行情况。
type stubPhase struct {
	name string
	run  func(*phase.Context) error
}

func (s *stubPhase) Name() string                                        { return s.name }
func (s *stubPhase) Applies(*phase.Context) bool                         { return true }
func (s *stubPhase) Check(context.Context, *phase.Context) (bool, error) { return false, nil }
func (s *stubPhase) Run(_ context.Context, c *phase.Context) error {
	if s.run != nil {
		return s.run(c)
	}
	return nil
}

func registryWith(names ...string) *phase.Registry {
	reg := phase.NewRegistry()
	for _, n := range names {
		_ = reg.Register(&stubPhase{name: n})
	}
	return reg
}

func TestRunRejectsMissingPhases(t *testing.T) {
	opts := initOptions(t, "m1")
	opts.Registry = registryWith("preflight") // 其余 phase 未实现 / the other phases are not implemented
	_, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("缺少 phase 必须失败，而不是静默跳过")
	}
	if !strings.Contains(err.Error(), "尚未注册") {
		t.Fatalf("错误信息应说明哪些 phase 尚未注册: %v", err)
	}
}

func TestRunExecutesInOrderAndWritesNodeConfig(t *testing.T) {
	opts := initOptions(t, "m1")
	var order []string
	reg := phase.NewRegistry()
	for _, name := range []string{"preflight", "host-init", "runtime", "certs", "kubeconfig", "kubelet", "etcd", "etcd-health", "control-plane", "cluster-init", "mark-control-plane", "network", "addon"} {
		n := name
		_ = reg.Register(&stubPhase{name: n, run: func(*phase.Context) error {
			order = append(order, n)
			return nil
		}})
	}
	opts.Registry = reg
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 13 {
		t.Fatalf("执行了 %d 个 phase: %v", len(order), order)
	}
	if order[0] != "preflight" || order[len(order)-1] != "addon" {
		t.Fatalf("执行顺序不对: %v", order)
	}
	// clusterID 自动生成并登记到 node.yaml。
	if result.ClusterID == "" || !strings.HasPrefix(result.ClusterID, "km-") {
		t.Fatalf("clusterID = %q", result.ClusterID)
	}
	store := &identity.Store{Dir: opts.Paths.Kubemono()}
	node, err := store.Load()
	if err != nil || node == nil {
		t.Fatalf("node.yaml 未写入: %v", err)
	}
	if node.ClusterID != result.ClusterID || node.NodeName != "m1" || node.HostName != "m1" {
		t.Fatalf("node.yaml 内容不正确: %+v", node)
	}
}

func TestRunDryRunExecutesNothing(t *testing.T) {
	opts := initOptions(t, "m1")
	opts.DryRun = true
	reg := registryWith("preflight", "host-init", "runtime", "certs", "kubeconfig", "kubelet", "etcd", "etcd-health", "control-plane", "cluster-init", "mark-control-plane", "network", "addon")
	opts.Registry = reg
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	store := &identity.Store{Dir: opts.Paths.Kubemono()}
	if node, _ := store.Load(); node != nil {
		t.Fatal("dry-run 不应写 node.yaml")
	}
}

// TestForcedActionInit：`kubemono init` 必须走同一条安装路径，且只能在 master 上。
func TestForcedActionInit(t *testing.T) {
	opts := testOptions(t, "m1")
	opts.ForceAction = ActionInit
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != ActionInit {
		t.Fatalf("action = %s", plan.Action)
	}
	// 非 init 节点必须被拒绝（不做探测）。
	other := testOptions(t, "w1")
	other.ForceAction = ActionInit
	if _, err := Decide(context.Background(), other); err == nil {
		t.Fatal("非初始化节点执行 init 必须被拒绝")
	}
	// 有残留数据时必须要求 --force-init。
	withResidual := testOptions(t, "m1")
	withResidual.ForceAction = ActionInit
	if err := os.MkdirAll(withResidual.Paths.EtcdData(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withResidual.Paths.EtcdData(), "member"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Decide(context.Background(), withResidual); err == nil {
		t.Fatal("有 etcd 数据时必须要求 --force-init")
	}
}

// TestForcedActionJoin：`kubemono join` 不做探测，但必须有 join 凭证。
func TestForcedActionJoin(t *testing.T) {
	opts := testOptions(t, "w1")
	opts.ForceAction = ActionJoin
	if _, err := Decide(context.Background(), opts); err == nil {
		t.Fatal("缺少 join 凭证时必须报错")
	}
	opts.JoinSecret = &clusterconfig.JoinSecret{Role: clusterconfig.RoleWorker, Token: "abcdef.0123456789abcdef"}
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != ActionJoin || len(plan.Phases) == 0 {
		t.Fatalf("plan = %+v", plan)
	}
	// 未知动作必须拒绝。
	opts.ForceAction = "bogus"
	if _, err := Decide(context.Background(), opts); err == nil {
		t.Fatal("未知强制动作必须被拒绝")
	}
}

// TestRunProvidesLazyClusterClient: 集群级 phase（cluster-init、mark-control-plane、
// network、addon、daemon）在 install 期间必须能拿到 apiserver 客户端。
// admin.conf 由同一个 phase 序列里的 kubeconfig phase 写出，所以 install 只能
// 传 ClientFunc；这条测试锁住"决定计划时还没有凭据、执行 phase 时才有"的行为。
func TestRunProvidesLazyClusterClient(t *testing.T) {
	opts := initOptions(t, "m1")
	lazy := fake.NewSimpleClientset()
	calls := 0
	opts.ClientFunc = func() (kubernetes.Interface, error) {
		calls++
		return lazy, nil
	}
	var resolved kubernetes.Interface
	reg := phase.NewRegistry()
	for _, name := range []string{"preflight", "host-init", "runtime", "certs", "kubeconfig", "kubelet", "etcd", "etcd-health", "control-plane", "cluster-init", "mark-control-plane", "network", "addon"} {
		n := name
		_ = reg.Register(&stubPhase{name: n, run: func(c *phase.Context) error {
			if n != "cluster-init" {
				return nil
			}
			client, err := c.KubeClient()
			if err != nil {
				return err
			}
			resolved = client
			return nil
		}})
	}
	opts.Registry = reg
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resolved != kubernetes.Interface(lazy) {
		t.Fatalf("cluster-init 拿到的客户端不是 ClientFunc 构造的")
	}
	if calls != 1 {
		t.Fatalf("ClientFunc 调用 %d 次，期望 1 次", calls)
	}
}

// TestPhaseListsFollowCanonicalOrder: Runner 按 phase.CanonicalOrder 执行，计划里的
// 顺序必须与之一致，否则 `install --dry-run` 打印的顺序会与真实执行顺序不同。
// 同时锁住 ：kubelet 必须先于 etcd/control-plane（静态 Pod 靠它拉起），
// worker 的本地代理必须先于 kubelet（TLS Bootstrap 走 127.0.0.1:6443）。
func TestPhaseListsFollowCanonicalOrder(t *testing.T) {
	rank := map[string]int{}
	for i, n := range phase.CanonicalOrder {
		rank[n] = i
	}
	hosts := map[string]*clusterconfig.Host{
		"master": {Name: "m1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		"worker": {Name: "w1", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	for hname, host := range hosts {
		for _, action := range []Action{ActionInit, ActionJoin, ActionSync} {
			list := phasesFor(action, host)
			for i := 1; i < len(list); i++ {
				if rank[list[i-1]] >= rank[list[i]] {
					t.Errorf("%s/%s: %s 排在 %s 之后，与 CanonicalOrder 不一致：%v", hname, action, list[i], list[i-1], list)
				}
			}
			idx := map[string]int{}
			for i, n := range list {
				idx[n] = i
			}
			if k, ok := idx["kubelet"]; ok {
				if e, ok := idx["etcd"]; ok && e < k {
					t.Errorf("%s/%s: etcd 先于 kubelet", hname, action)
				}
				if lp, ok := idx["local-proxy"]; ok && lp > k {
					t.Errorf("%s/%s: local-proxy 晚于 kubelet", hname, action)
				}
			}
		}
	}
	// 第 13/14 步：master 的 init 必须部署网络、附加组件并启动守护进程。
	init := strings.Join(phasesFor(ActionInit, hosts["master"]), ",")
	for _, n := range []string{"network", "addon"} {
		if !strings.Contains(init, n) {
			t.Errorf("init 序列缺少 %s：%s", n, init)
		}
	}
}

// TestRunWritesDesiredSnapshot: install 结束后必须留下与 node repair 相同口径的
// desired 基线（render.DefaultBuilders），否则后续修复没有参照。
func TestRunWritesDesiredSnapshot(t *testing.T) {
	opts := initOptions(t, "m1")
	opts.Registry = registryWith(phasesFor(ActionInit, &clusterconfig.Host{Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}})...)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	store := render.NewStore(opts.Paths)
	snap, err := store.Load()
	if err != nil || snap == nil {
		t.Fatalf("desired 快照未写入: %v", err)
	}
	if snap.Node != "m1" || snap.Release == "" {
		t.Fatalf("快照元数据不正确: node=%s release=%q", snap.Node, snap.Release)
	}
	spec, err := store.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	host, _ := opts.Cluster.Cluster.Hosts.FindHost("m1")
	want, err := buildDesired(&render.Context{
		Cluster: opts.Cluster, Host: host, BOM: opts.BOM, Paths: opts.Paths, Release: opts.Release,
		ClusterID: opts.ClusterID,
		HostMTU:   network.DetectHostMTU(host.AllAddresses()...),
	}, "")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if spec.Hash() != want.Hash() {
		t.Fatal("快照内容必须与 node repair 的渲染口径（render.DefaultBuilders）一致")
	}
	// install 还必须写下本机配置缓存；否则第一次修复
	// 只能以 remote 为准（没有本地快照可比）。
	if _, err := os.Stat(opts.Paths.ClusterConfig()); err != nil {
		t.Fatalf("install 未写入本机配置缓存 %s: %v", opts.Paths.ClusterConfig(), err)
	}
}

// TestDryRunWritesNoDesiredSnapshot：dry-run 不写 desired 快照。
func TestDryRunWritesNoDesiredSnapshot(t *testing.T) {
	opts := initOptions(t, "m1")
	opts.DryRun = true
	opts.Registry = registryWith(phasesFor(ActionInit, &clusterconfig.Host{Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}})...)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if snap, _ := render.NewStore(opts.Paths).Load(); snap != nil {
		t.Fatal("dry-run 不应写 desired 快照")
	}
}

// TestSaveNodeConfigKeepsExistingClusterID 覆盖真机踩到的坑：sync / 幂等修复的
// 调用方不会传 ClusterID，saveNodeConfig 不能无条件覆盖它——否则在已装好的
// 节点上跑一次 `kubemono install --phases ...` 就把 node.yaml 的 clusterID 清空，
// 之后所有 install 都被判成"没有安装记录"。
func TestSaveNodeConfigKeepsExistingClusterID(t *testing.T) {
	opts := testOptions(t, "m1")
	opts.ClusterID = "cid-1"
	plan := &Plan{NodeName: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}}
	if err := saveNodeConfig(opts, plan); err != nil {
		t.Fatalf("saveNodeConfig: %v", err)
	}

	// 第二次不带 ClusterID（模拟 sync）：已记录的身份必须保留。
	opts.ClusterID = ""
	if err := saveNodeConfig(opts, plan); err != nil {
		t.Fatalf("第二次 saveNodeConfig: %v", err)
	}
	store := &identity.Store{Dir: opts.Paths.Kubemono()}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ClusterID != "cid-1" {
		t.Fatalf("clusterID = %q，期望保留 cid-1", got.ClusterID)
	}
}

func TestRetryJoinWaitsForUnreachableClusterAndRejectsHardErrors(t *testing.T) {
	opts := &Options{
		Wait:   time.Second,
		Sleep:  func(time.Duration) {},
		Report: output.Discard(),
	}
	attempts := 0
	err := opts.retryJoin(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return discovery.ErrUnreachable
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("retryJoin = %v, attempts=%d, want nil/3", err, attempts)
	}

	hard := errors.New("cluster-info JWS 校验失败")
	attempts = 0
	err = opts.retryJoin(context.Background(), func() error {
		attempts++
		return hard
	})
	if !errors.Is(err, hard) || attempts != 1 {
		t.Fatalf("hard error = %v, attempts=%d，必须立即失败", err, attempts)
	}
}

func TestRunWritesActualNodeNameAndConfigIdentity(t *testing.T) {
	opts := initOptions(t, "m1")
	opts.Cluster.Cluster.Hosts[0].NodeName = clusterconfig.NodeNameFromAddress
	opts.Registry = registryWith("preflight", "host-init", "runtime", "certs", "kubeconfig", "kubelet", "etcd", "etcd-health", "control-plane", "cluster-init", "mark-control-plane", "network", "addon")
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	node, err := (&identity.Store{Dir: opts.Paths.Kubemono()}).Load()
	if err != nil || node == nil {
		t.Fatalf("node.yaml: %v", err)
	}
	if node.NodeName != "10.0.0.1" || node.HostName != "m1" {
		t.Fatalf("node.yaml = node %q host %q, want 10.0.0.1/m1", node.NodeName, node.HostName)
	}
}

func TestJoinEndpointsPrefersExternalAndJoinFile(t *testing.T) {
	opts := testOptions(t, "m2")
	opts.Cluster.ControlPlane.ExternalEndpoints = []string{"https://vip.example:6443"}
	opts.JoinSecret = &clusterconfig.JoinSecret{
		Role:               clusterconfig.RoleMaster,
		DiscoveryEndpoints: []string{"https://10.0.0.2:6443", "https://vip.example:6443"},
	}
	got := opts.joinEndpoints()
	want := []string{
		"https://vip.example:6443",
		"https://10.0.0.2:6443",
		"10.0.0.1",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("joinEndpoints = %v, want %v", got, want)
	}
}

// TestJoinEndpointsPreferredFirst 固定 `join <endpoint>` 显式入口：
// 显式给出的入口必须排在 cluster.yaml 的 externalEndpoints 之前。
func TestJoinEndpointsPreferredFirst(t *testing.T) {
	opts := testOptions(t, "m2")
	opts.Cluster.ControlPlane.ExternalEndpoints = []string{"https://vip.example:6443"}
	opts.PreferredEndpoint = "10.0.0.9:6443"
	opts.JoinSecret = &clusterconfig.JoinSecret{
		Role:               clusterconfig.RoleMaster,
		DiscoveryEndpoints: []string{"https://10.0.0.2:6443"},
	}
	got := opts.joinEndpoints()
	want := []string{
		"10.0.0.9:6443",
		"https://vip.example:6443",
		"https://10.0.0.2:6443",
		"10.0.0.1",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("joinEndpoints = %v, want %v", got, want)
	}
}

// TestDefaultsApplyDataDirs 校验 install 使用配置中的 kubelet/etcd 数据目录。
func TestDefaultsApplyDataDirs(t *testing.T) {
	cluster := &clusterconfig.Cluster{}
	cluster.Node.Kubelet.RootDir = "/data/kubelet"
	cluster.ControlPlane.Etcd.Local.DataDir = "/data/etcd"
	o := &Options{Cluster: cluster, Paths: constants.NewPaths("/root")}
	o.defaults()
	if got := o.Paths.Kubelet(); got != "/root/data/kubelet" {
		t.Errorf("Kubelet() = %q", got)
	}
	if got := o.Paths.EtcdData(); got != "/root/data/etcd" {
		t.Errorf("EtcdData() = %q", got)
	}
}

// TestRunDesiredSnapshotUsesNodePodCIDR 固定 P0-1 的 install 侧：
// install 收尾写 desired 时也必须使用 Node.spec.podCIDR，否则第一次修复
// 会把 CNI 改回兜底子网。
func TestRunDesiredSnapshotUsesNodePodCIDR(t *testing.T) {
	opts := initOptions(t, "m1")
	opts.Registry = registryWith(phasesFor(ActionInit, &clusterconfig.Host{Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}})...)
	opts.ClientFunc = func() (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "m1"},
			Spec:       corev1.NodeSpec{PodCIDR: "10.244.2.0/24"},
		}), nil
	}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	spec, err := render.NewStore(opts.Paths).Restore()
	if err != nil || spec == nil {
		t.Fatalf("Restore desired: %v", err)
	}
	found := false
	for _, f := range spec.Files {
		if filepath.Base(f.Path) != "10-kuberouter.conflist" {
			continue
		}
		found = true
		if !strings.Contains(string(f.Content), `"subnet": "10.244.2.0/24"`) {
			t.Fatalf("install desired 的 CNI 没有使用 Node.spec.podCIDR：\n%s", f.Content)
		}
	}
	if !found {
		t.Fatal("desired 快照缺少 CNI 配置")
	}
}

// TestPrepareJoinWorkerRejectsMasterFile 固定 worker 的最小证书边界：
// worker 只需要 CA，不能接受带 certificateKey 的 master 凭证。
func TestPrepareJoinWorkerRejectsMasterFile(t *testing.T) {
	opts := testOptions(t, "w1")
	opts.JoinSecret = &clusterconfig.JoinSecret{
		Role:           clusterconfig.RoleMaster,
		Token:          "abcdef.0123456789abcdef",
		CertificateKey: "deadbeef",
	}
	err := opts.prepareJoin(context.Background())
	if err == nil || !strings.Contains(err.Error(), "纯 worker") {
		t.Fatalf("worker 使用 master 凭证必须拒绝，got %v", err)
	}
}

// TestPrepareJoinMasterRejectsWorkerFile 固定 master 不能拿 worker 文件：
// worker 文件没有 certificateKey，无法解密集群证书包。
func TestPrepareJoinMasterRejectsWorkerFile(t *testing.T) {
	opts := testOptions(t, "m2")
	opts.JoinSecret = &clusterconfig.JoinSecret{
		Role:  clusterconfig.RoleWorker,
		Token: "abcdef.0123456789abcdef",
	}
	err := opts.prepareJoin(context.Background())
	if err == nil || !strings.Contains(err.Error(), "master/etcd") {
		t.Fatalf("master 使用 worker 凭证必须拒绝，got %v", err)
	}
}

// TestValidateJoinRole 固定 join 凭证角色与密钥的严格匹配。
func TestValidateJoinRole(t *testing.T) {
	worker := testOptions(t, "w1")
	master := testOptions(t, "m2")
	cases := []struct {
		name    string
		opts    *Options
		secret  *clusterconfig.JoinSecret
		wantErr string
	}{
		{"worker worker credential", worker, &clusterconfig.JoinSecret{Role: clusterconfig.RoleWorker, Token: "t"}, ""},
		{"worker master credential", worker, &clusterconfig.JoinSecret{Role: clusterconfig.RoleMaster, CertificateKey: "k", Token: "t"}, "control-plane"},
		{"worker empty role", worker, &clusterconfig.JoinSecret{Token: "t"}, "control-plane"},
		{"worker key on worker credential", worker, &clusterconfig.JoinSecret{Role: clusterconfig.RoleWorker, CertificateKey: "k", Token: "t"}, "certificateKey"},
		{"master master credential", master, &clusterconfig.JoinSecret{Role: clusterconfig.RoleMaster, CertificateKey: "k", Token: "t"}, ""},
		{"master worker credential", master, &clusterconfig.JoinSecret{Role: clusterconfig.RoleWorker, Token: "t"}, "master/etcd"},
		{"master empty role", master, &clusterconfig.JoinSecret{CertificateKey: "k", Token: "t"}, "master/etcd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.JoinSecret = tc.secret
			err := tc.opts.validateJoinRole()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestPhasePlansFollowCanonicalOrder：phasePlans 只表达“哪些 phase 适用”，
// 顺序必须完全由 phase.CanonicalOrder 决定；出现未知 phase 或顺序不一致要失败。
func TestPhasePlansFollowCanonicalOrder(t *testing.T) {
	known := make(map[string]bool, len(phase.CanonicalOrder))
	for _, name := range phase.CanonicalOrder {
		known[name] = true
	}
	for key, phases := range phasePlans {
		for _, name := range phases {
			if !known[name] {
				t.Errorf("plan %+v 包含未知 phase %q", key, name)
			}
		}
		sorted := phase.SortCanonical(phases)
		if len(sorted) != len(phases) {
			t.Errorf("plan %+v 有重复 phase：%v", key, phases)
			continue
		}
		for i := range phases {
			if phases[i] != sorted[i] {
				t.Errorf("plan %+v 未按 CanonicalOrder 书写：%v → %v", key, phases, sorted)
				break
			}
		}
	}
}
