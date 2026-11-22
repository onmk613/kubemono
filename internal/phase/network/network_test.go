package network_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/network"
	"kubemono/internal/phase"
	networkphase "kubemono/internal/phase/network"
	"kubemono/internal/render"
	"kubemono/internal/util/output"
)

// testPhaseContext 构建一个以临时目录为根的 phase 上下文。
func testPhaseContext(t *testing.T, doc, nodeName string) *phase.Context {
	t.Helper()
	cluster, err := clusterconfig.LoadCluster([]byte(doc))
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	host, ok := cluster.Cluster.Hosts.FindHost(nodeName)
	if !ok {
		t.Fatalf("host %s 不在配置里", nodeName)
	}
	root := t.TempDir()
	return &phase.Context{
		Cluster:   cluster,
		ClusterID: "test-cluster",
		Host:      host,
		BOM:       bom.MustLoad(),
		Paths:     constants.NewPaths(root),
		Release:   "v1.31.4-km.1",
		Report:    output.Discard(),
		Now:       func() time.Time { return time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC) },
	}
}

// clusterDoc 是 phase 测试使用的完整且通过校验的集群配置。
func clusterDoc(plugin string) string {
	return `cluster:
  hosts:
    - { name: m1, address: 10.0.0.1, roles: [master, worker] }
    - { name: m2, address: 10.0.0.2, roles: [master, worker] }
    - { name: w1, address: 10.0.0.11, roles: [worker] }
network:
  plugin: ` + plugin + `
  podCIDR: 10.244.0.0/16
  serviceCIDR: 10.96.0.0/12
`
}

// fakeChecker 是 phase 测试使用的可注入探针。
type fakeChecker struct {
	filesOK  bool
	healthOK bool
	healths  []network.Health
}

func (f *fakeChecker) CheckLocalFiles(*render.Spec) (bool, error) { return f.filesOK, nil }

func (f *fakeChecker) CheckHealth(_ context.Context, healths []network.Health) error {
	f.healths = append(f.healths, healths...)
	if f.healthOK {
		return nil
	}
	return errUnhealthy
}

var errUnhealthy = &healthError{}

type healthError struct{}

func (*healthError) Error() string { return "probe failed" }

// TestRunCreatesClusterObjectsAndIsIdempotent 断言 Run 创建集群对象且重复执行幂等。
func TestRunCreatesClusterObjectsAndIsIdempotent(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "m1")
	client := fake.NewSimpleClientset()
	p := &networkphase.Phase{Client: client}

	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// network phase 管理 CNI（kube-router）与 Service 代理（kube-proxy）。
	ctx := context.Background()
	if _, err := client.AppsV1().DaemonSets("kube-system").Get(ctx, "kube-router", metaGet); err != nil {
		t.Errorf("kube-router DaemonSet 未创建：%v", err)
	}
	if _, err := client.RbacV1().ClusterRoles().Get(ctx, "kube-router", metaGet); err != nil {
		t.Errorf("kube-router ClusterRole 未创建：%v", err)
	}
	if _, err := client.AppsV1().DaemonSets("kube-system").Get(ctx, "kube-proxy", metaGet); err != nil {
		t.Errorf("kube-proxy DaemonSet 未创建：%v", err)
	}
	if _, err := client.CoreV1().ConfigMaps("kube-system").Get(ctx, "kube-proxy", metaGet); err != nil {
		t.Errorf("kube-proxy ConfigMap 未创建：%v", err)
	}

	// 本机文件：只有 CNI 配置。
	cniPath := network.CNIConfigPath(renderCtx(c))
	if _, err := os.Stat(cniPath); err != nil {
		t.Errorf("CNI 配置未落盘：%v", err)
	}
	// kube-router 仍使用 ConfigMap 里的 ServiceAccount kubeconfig。
	cm, err := client.CoreV1().ConfigMaps("kube-system").Get(context.Background(), "kube-router", metaGet)
	if err != nil {
		t.Fatalf("kube-router ConfigMap: %v", err)
	}
	kc := cm.Data[network.DaemonSetKubeconfigKey]
	for _, want := range []string{"tokenFile: " + network.ServiceAccountTokenPath, network.ServiceAccountCAPath, "https://127.0.0.1:6443"} {
		if !strings.Contains(kc, want) {
			t.Errorf("kube-router 的 SA kubeconfig 缺少 %q：\n%s", want, kc)
		}
	}
	if _, err := os.Stat(c.Paths.KubeconfigPath("kube-router.conf")); err == nil {
		t.Error("kube-router 不应再有宿主机 kubeconfig（它引用的客户端证书从未被签发）")
	}

	// 第二次执行：内容不变，因此不应有任何写入动作（幂等）。
	p2 := &networkphase.Phase{Client: client}
	if err := p2.Run(context.Background(), c); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	before, _ := os.ReadFile(cniPath)
	p3 := &networkphase.Phase{Client: client}
	if err := p3.Run(context.Background(), c); err != nil {
		t.Fatalf("第三次 Run: %v", err)
	}
	after, _ := os.ReadFile(cniPath)
	if string(before) != string(after) {
		t.Error("重复执行改变了 CNI 配置内容")
	}
}

// TestKubeRouterDoesNotPassUnsupportedMTUFlag 断言 MTU 只进 CNI 配置，不进
// kube-router 参数：kube-router v2.11.1 不认识 --mtu，带上会直接 CrashLoopBackOff。
func TestKubeRouterDoesNotPassUnsupportedMTUFlag(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "m1")
	c.Cluster.Network.KubeRouter.MTU = 1400
	client := fake.NewSimpleClientset()
	p := &networkphase.Phase{Client: client}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ds, err := client.AppsV1().DaemonSets("kube-system").Get(context.Background(), "kube-router", metaGet)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range ds.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(arg, "--mtu") {
			t.Fatalf("kube-router 不应收到不支持的 --mtu 参数: %v", ds.Spec.Template.Spec.Containers[0].Args)
		}
	}
	data, err := os.ReadFile(filepath.Join(c.Paths.CNIConfig(), network.CNIConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"mtu": 1400`) {
		t.Fatalf("CNI 配置应带 mtu 1400:\n%s", data)
	}
}

func TestDedicatedMasterWritesNoLocalNetworkFiles(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "m1")
	c.Host.Roles = []clusterconfig.Role{clusterconfig.RoleMaster}
	c.Cluster.Cluster.Hosts = []clusterconfig.Host{*c.Host}
	client := fake.NewSimpleClientset()
	p := &networkphase.Phase{Client: client}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.Paths.CNIConfig(), network.CNIConfigFileName)); !os.IsNotExist(err) {
		t.Fatalf("纯控制面 master 不应写 CNI 配置，err=%v", err)
	}
	if _, err := client.AppsV1().DaemonSets("kube-system").Get(context.Background(), "kube-router", metaGet); err != nil {
		t.Fatalf("集群级 kube-router DaemonSet 仍应由 master 应用: %v", err)
	}
}

// TestRunWithoutClientFailsClearly 断言没有 apiserver 凭据时给出明确错误。
func TestRunWithoutClientFailsClearly(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "m1")
	p := &networkphase.Phase{}
	err := p.Run(context.Background(), c)
	if err == nil {
		t.Fatal("没有 client 时 Run 应当报错")
	}
	msg := err.Error()
	for _, want := range []string{"apiserver", "kubeconfig"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误里应当提到 %q：%v", want, err)
		}
	}
}

// TestPluginNoneWritesNoCNIConfig 断言 ：plugin=none 时网络层不写任何 CNI 配置。
func TestPluginNoneWritesNoCNIConfig(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginNone), "w1")
	p := &networkphase.Phase{Client: fake.NewSimpleClientset()}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(c.Paths.CNIConfig())
	if err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("plugin=none 不应写任何 CNI 配置：%v", names)
	}
	// external CNI 时不应创建 kube-router 对象，但 kube-proxy 仍默认部署。
	for _, a := range p.Client.(*fake.Clientset).Actions() {
		if (a.GetVerb() == "create" || a.GetVerb() == "update") && strings.Contains(a.GetResource().Resource, "kube-router") {
			t.Errorf("plugin=none 不应创建 kube-router 对象：%s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if _, err := p.Client.(*fake.Clientset).AppsV1().DaemonSets("kube-system").Get(context.Background(), "kube-proxy", metaGet); err != nil {
		t.Errorf("plugin=none 也应默认创建 kube-proxy：%v", err)
	}
}

// TestDryRunDoesNotTouchHost 断言 dry-run 不落盘、也不写集群对象。
func TestDryRunDoesNotTouchHost(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "w1")
	c.DryRun = true
	client := fake.NewSimpleClientset()
	p := &networkphase.Phase{Client: client}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(network.CNIConfigPath(renderCtx(c))); !os.IsNotExist(err) {
		t.Errorf("dry-run 不应写 CNI 配置：%v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "update" {
			t.Errorf("dry-run 不应写集群对象：%s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// TestCheckUsesInjectedChecker 断言 Check 通过注入的探针判定。
func TestCheckUsesInjectedChecker(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "w1")
	checker := &fakeChecker{filesOK: true, healthOK: true}
	p := &networkphase.Phase{Client: fake.NewSimpleClientset(), Checker: checker}
	done, err := p.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Error("文件与健康都通过时 Check 应当返回 true")
	}
	if len(checker.healths) == 0 {
		t.Error("Check 应当探测网络组件的健康端点")
	}
	// network phase 只探测 kube-router。
	ports := map[string]bool{}
	for _, h := range checker.healths {
		ports[h.Address] = true
	}
	if !ports["127.0.0.1:20244"] {
		t.Errorf("健康端点应包含 kube-router 的 20244：%v", ports)
	}

	checker.healthOK = false
	if done, err := p.Check(context.Background(), c); err != nil || done {
		t.Errorf("健康检查失败时 Check 应当返回 false，实际 done=%v err=%v", done, err)
	}
	checker.healthOK = true
	checker.filesOK = false
	if done, err := p.Check(context.Background(), c); err != nil || done {
		t.Errorf("文件不一致时 Check 应当返回 false，实际 done=%v err=%v", done, err)
	}
}

// TestCheckDetectsDrift 断言默认探针能发现 live 文件缺失。
func TestCheckDetectsDrift(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "w1")
	p := &networkphase.Phase{Client: fake.NewSimpleClientset()}
	done, err := p.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Error("还没落盘时 Check 应当返回 false")
	}
}

// metaGet 是测试共用的 GetOptions。
var metaGet = metav1.GetOptions{}

// renderCtx 把 phase 上下文转换为用于断言的 render 上下文。
func renderCtx(c *phase.Context) *render.Context {
	return &render.Context{
		Cluster:   c.Cluster,
		Host:      c.Host,
		Node:      c.Node,
		BOM:       c.BOM,
		Paths:     c.Paths,
		Release:   c.Release,
		ClusterID: c.ClusterID,
	}
}

// TestRunKeepsFilesOfOtherComponents 覆盖真机踩到的严重缺陷。
// 本机 desired 快照是**整机共享**的（install / node repair 共同维护）。
// network phase 不能拿整份快照和自己的期望状态比较：那会把控制面清单、kubelet
// 配置等别的组件的文件判成"应删除"并真的删除——刚装好的单节点集群在 network
// phase 结束时 /etc/kubernetes/manifests/* 会被清空，etcd 立刻被 kubelet 杀掉，
// apiserver 随后失联（真实环境实测，日志里就是"已删除网络文件 .../etcd.yaml"）。
func TestRunKeepsFilesOfOtherComponents(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "w1")
	client := fake.NewSimpleClientset()

	// 别的组件的文件：进快照、也真的落盘。
	foreign := []render.File{
		{Path: c.Paths.ManifestPath(constants.ComponentEtcd), Content: []byte("etcd-pod"), Mode: 0o644,
			Component: constants.ComponentEtcd, Kind: render.KindManifest},
		{Path: c.Paths.KubeletConfig(), Content: []byte("kubelet-config"), Mode: 0o644,
			Component: constants.ComponentKubelet, Kind: render.KindConfig},
	}
	snap := &render.Spec{Node: c.NodeName(), Files: append([]render.File(nil), foreign...)}
	if err := render.NewStore(c.Paths).Write(snap); err != nil {
		t.Fatalf("写入快照失败：%v", err)
	}
	for _, f := range foreign {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.Path, f.Content, os.FileMode(f.Mode)); err != nil {
			t.Fatal(err)
		}
	}

	p := &networkphase.Phase{Client: client}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range foreign {
		if _, err := os.Stat(f.Path); err != nil {
			t.Errorf("network phase 删掉了别的组件的文件 %s：%v", f.Path, err)
		}
	}
}

// TestRunReloadsRuntimeOnlyWhenCNIConfigChanges 覆盖真机踩到的坑。
// containerd 只在**启动时**初始化 CNI：如果它在 /etc/cni/net.d 还没有配置时启动，
// 会一直报 "cni plugin not initialized"，节点永远 NotReady（四轮真机验收每次都要
// 手工 systemctl restart containerd）。因此写 CNI 配置后必须主动重启运行时；
// 而幂等重跑（内容没变）时**不能**重启，否则一次 sync 会白白打断所有容器。
func TestRunReloadsRuntimeOnlyWhenCNIConfigChanges(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "m1")
	client := fake.NewSimpleClientset()
	calls := 0
	p := &networkphase.Phase{
		Client:         client,
		RestartRuntime: func(context.Context) error { calls++; return nil },
	}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("首次写入 CNI 配置后必须重启运行时一次，实际 %d 次", calls)
	}

	// 第二次：配置内容不变 → 不应重启。
	p2 := &networkphase.Phase{
		Client:         client,
		RestartRuntime: func(context.Context) error { calls++; return nil },
	}
	if err := p2.Run(context.Background(), c); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("幂等重跑不应重启运行时，累计 %d 次", calls)
	}

	// 删掉 CNI 配置 → 重新写入时应再次重启。
	if err := os.Remove(network.CNIConfigPath(renderCtx(c))); err != nil {
		t.Fatal(err)
	}
	p3 := &networkphase.Phase{
		Client:         client,
		RestartRuntime: func(context.Context) error { calls++; return nil },
	}
	if err := p3.Run(context.Background(), c); err != nil {
		t.Fatalf("第三次 Run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("CNI 配置重建后应再次重启运行时，累计 %d 次", calls)
	}
}

// TestRunRemovesStaleCNIConfigAfterPluginSwitch 保护 plugin 从 kube-router
// 切到 none 时的残留文件：desired 快照不含网络文件，必须靠已知受管路径
// 把旧 CNI 配置纳入删除集合。
func TestRunRemovesStaleCNIConfigAfterPluginSwitch(t *testing.T) {
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginNone), "m1")
	rc := &render.Context{
		Cluster: c.Cluster, Host: c.Host, Node: c.Node, BOM: c.BOM, Paths: c.Paths,
		Release: c.Release, ClusterID: c.ClusterID,
	}
	path := network.CNIConfigPath(rc)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &networkphase.Phase{Client: fake.NewSimpleClientset()}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plugin=none 后旧 CNI 配置应被删除，stat err=%v", err)
	}
}

// TestNetworkSwitchPluginRemovesStaleObjectsAndFiles：plugin kube-router -> none
// 时必须删除旧 DaemonSet 与 CNI 文件，否则“关闭 CNI”只是不再 apply。
func TestNetworkSwitchPluginRemovesStaleObjectsAndFiles(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	c := testPhaseContext(t, clusterDoc(clusterconfig.PluginKubeRouter), "m1")
	p := &networkphase.Phase{Client: client}
	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("Run kube-router: %v", err)
	}
	if _, err := client.AppsV1().DaemonSets("kube-system").Get(ctx, "kube-router", metaGet); err != nil {
		t.Fatalf("kube-router DaemonSet 未创建：%v", err)
	}
	cniPath := network.CNIConfigPath(renderCtx(c))
	if _, err := os.Stat(cniPath); err != nil {
		t.Fatalf("CNI 文件未创建：%v", err)
	}

	c2 := testPhaseContext(t, clusterDoc(clusterconfig.PluginNone), "m1")
	c2.Paths = c.Paths // 同一个测试根：模拟同一台节点切换 plugin
	p2 := &networkphase.Phase{Client: client}
	if err := p2.Run(ctx, c2); err != nil {
		t.Fatalf("Run plugin=none: %v", err)
	}
	if _, err := client.AppsV1().DaemonSets("kube-system").Get(ctx, "kube-router", metaGet); !apierrors.IsNotFound(err) {
		t.Fatalf("旧 kube-router DaemonSet 应被删除，err=%v", err)
	}
	if _, err := os.Stat(cniPath); !os.IsNotExist(err) {
		t.Fatalf("旧 CNI 文件应被删除，err=%v", err)
	}
}
