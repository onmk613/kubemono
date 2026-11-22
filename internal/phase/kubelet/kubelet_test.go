package kubelet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	controlplane "kubemono/internal/controlplane"
	kubeletpkg "kubemono/internal/kubelet"
	"kubemono/internal/kubelet/kubeletfake"
	"kubemono/internal/phase"
	"kubemono/internal/profile"
	"kubemono/internal/render"
)

// testReporter 记录每条消息，便于测试断言面向运维人员的输出。
type testReporter struct {
	mu  sync.Mutex
	msg []string
}

func (r *testReporter) log(level, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msg = append(r.msg, level+": "+fmt.Sprintf(format, args...))
}

func (r *testReporter) Debugf(format string, args ...any) { r.log("debug", format, args...) }
func (r *testReporter) Infof(format string, args ...any)  { r.log("info", format, args...) }
func (r *testReporter) Warnf(format string, args ...any)  { r.log("warn", format, args...) }
func (r *testReporter) Errorf(format string, args ...any) { r.log("error", format, args...) }
func (r *testReporter) Step(name string) func(error) {
	r.log("step", "%s", name)
	return func(error) {}
}
func (r *testReporter) Result(ok bool, format string, args ...any) {
	r.log("result", "%v "+format, append([]any{ok}, args...)...)
}

func (r *testReporter) contains(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msg {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func (r *testReporter) warnCount(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.msg {
		if strings.HasPrefix(m, "warn:") && strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

type fakeClock struct {
	now    time.Time
	sleeps int
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1700000000, 0).UTC()} }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) {
	c.sleeps++
	c.now = c.now.Add(d)
}

type okHealth struct{}

func (okHealth) Check(context.Context, controlplane.HealthCheck) error { return nil }

type failHealth struct{ err error }

func (f failHealth) Check(context.Context, controlplane.HealthCheck) error { return f.err }

// harness 把 phase 接到以临时目录为根的文件系统与内存替身上。
type harness struct {
	phase  *Phase
	pc     *phase.Context
	spec   *render.Spec
	svc    *kubeletfake.MemoryServiceManager
	node   *kubeletfake.FakeNodeClient
	inst   *kubeletpkg.BinaryInstaller
	clock  *fakeClock
	report *testReporter
	host   *clusterconfig.Host
}

func clusterForTest(mode string) *clusterconfig.Cluster {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(c)
	return c
}

func newHarness(t *testing.T, mode, node string, withArtifact bool) *harness {
	t.Helper()
	cluster := clusterForTest(mode)
	host, ok := cluster.Cluster.Hosts.FindHost(node)
	if !ok {
		t.Fatalf("节点 %s 不在配置里", node)
	}
	paths := constants.NewPaths(t.TempDir())
	report := &testReporter{}
	b := bom.MustLoad()
	pc := &phase.Context{
		Cluster: cluster,
		Host:    host,
		BOM:     b,
		Paths:   paths,
		Release: b.Release,
		Report:  report,
		Now:     func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	spec, err := render.Render(&render.Context{
		Cluster: cluster, Host: host, BOM: b, Paths: paths, Release: b.Release, ClusterID: "cid-1",
	}, render.DefaultBuilders()...)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if withArtifact {
		src := filepath.Join(paths.ArtifactDir(b.Release), "bin", "k8s", "kubelet")
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src, []byte("kubelet-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inst := &kubeletpkg.BinaryInstaller{Paths: paths, Release: b.Release, BOM: b}
	svc := kubeletfake.NewMemoryServiceManager()
	nc := &kubeletfake.FakeNodeClient{Ready: map[string]bool{node: true}}
	clock := newFakeClock()
	p := &Phase{
		Spec:           spec,
		ServiceManager: svc,
		NodeClient:     nc,
		Installer:      inst,
		WaitKnobs: phase.WaitKnobs{
			Sleep:    clock.Sleep,
			Clock:    clock.Now,
			Timeout:  6 * time.Second,
			Interval: 2 * time.Second,
		},
		Health: okHealth{},
	}
	return &harness{phase: p, pc: pc, spec: spec, svc: svc, node: nc, inst: inst, clock: clock, report: report, host: host}
}

// writeBootstrapOutput 模拟 kubelet 已完成 TLS Bootstrap。
func (h *harness) writeBootstrapOutput(t *testing.T) {
	t.Helper()
	p := h.pc.Paths.KubeconfigPath(constants.KubeletKubeconfigName)
	if err := os.MkdirAll(h.pc.Paths.Kubernetes(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPhaseIsRegistered(t *testing.T) {
	p, ok := phase.Default().Get(PhaseName)
	if !ok {
		t.Fatal("kubelet phase 必须通过 init() 注册")
	}
	if p.Applies(&phase.Context{}) {
		t.Fatal("没有 Host 时 kubelet phase 不应适用")
	}
	if !p.Applies(&phase.Context{Host: &clusterconfig.Host{Name: "w1", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}}}) {
		t.Fatal("worker 必须运行 kubelet")
	}
}

func TestRunWritesFilesAndCheckBecomesDone(t *testing.T) {
	h := newHarness(t, "binary", "w1", true)
	h.writeBootstrapOutput(t)
	ctx := context.Background()

	done, err := h.phase.Check(ctx, h.pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("全新节点不能报告完成")
	}

	if err := h.phase.Run(ctx, h.pc); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// kubelet 配置必须落盘，内容与渲染结果一致。
	cfg, err := os.ReadFile(h.pc.Paths.KubeletConfig())
	if err != nil {
		t.Fatalf("kubelet 配置未写入: %v", err)
	}
	wantCfg, _ := h.spec.Find(h.pc.Paths.KubeletConfig())
	if string(cfg) != string(wantCfg.Content) {
		t.Fatal("kubelet 配置内容与渲染结果不一致")
	}
	// binary 模式的 unit 必须来自渲染结果。
	unitPath := kubeletpkg.UnitPath(h.pc.Paths)
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("kubelet.service 未写入: %v", err)
	}
	wantUnit, ok := kubeletpkg.UnitFile(h.spec, h.pc.Paths)
	if !ok {
		t.Fatal("binary 模式必须渲染 kubelet.service")
	}
	if string(unit) != string(wantUnit) {
		t.Fatalf("落盘的 unit 与渲染结果不一致:\n%s", unit)
	}
	if !strings.Contains(string(unit), "ExecStart=/usr/local/bin/kubelet") {
		t.Fatalf("unit 的 ExecStart 必须用 /usr/local/bin 稳定路径:\n%s", unit)
	}
	if !h.svc.Enabled[kubeletpkg.UnitName] || !h.svc.Active[kubeletpkg.UnitName] {
		t.Fatalf("kubelet.service 必须被启用并启动: %+v", h.svc.Calls)
	}
	// 首次安装没有旧文件可备份，因此备份目录应为空（备份只针对被替换的内容）。
	if got := countFiles(t, h.pc.Paths.Backup()); got != 0 {
		t.Fatalf("首次安装不应产生备份，实际 %d 个文件", got)
	}

	done, err = h.phase.Check(ctx, h.pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatal("安装完成后 Check 必须为真")
	}

	// 幂等：再跑一次不产生任何文件变更。
	before, _ := os.Stat(h.pc.Paths.KubeletConfig())
	backupsBefore := countFiles(t, h.pc.Paths.Backup())
	if err := h.phase.Run(ctx, h.pc); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	after, _ := os.Stat(h.pc.Paths.KubeletConfig())
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("幂等运行不应重写 kubelet 配置")
	}
	if got := countFiles(t, h.pc.Paths.Backup()); got != backupsBefore {
		t.Fatalf("幂等运行产生了新的备份：%d -> %d", backupsBefore, got)
	}
}

func TestMasterWaitsOnlyForLocalHealth(t *testing.T) {
	// master 的 kubelet 先于 etcd/控制面启动：此时没有 apiserver，只能看本机 healthz。
	h := newHarness(t, "staticpod", "m1", true)
	h.node.Ready["m1"] = false
	if err := h.phase.Run(context.Background(), h.pc); err != nil {
		t.Fatalf("master 不应等待 Node Ready: %v", err)
	}
	if h.node.NodeReadyCalls != 0 {
		t.Fatalf("master 的 Run 不应查询 Node（调用了 %d 次）", h.node.NodeReadyCalls)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	h := newHarness(t, "binary", "w1", true)
	h.writeBootstrapOutput(t)
	h.pc.DryRun = true

	if err := h.phase.Run(context.Background(), h.pc); err != nil {
		t.Fatalf("DryRun Run: %v", err)
	}
	if _, err := os.Stat(h.pc.Paths.KubeletConfig()); err == nil {
		t.Fatal("dry-run 不应写 kubelet 配置")
	}
	if _, err := os.Stat(kubeletpkg.UnitPath(h.pc.Paths)); err == nil {
		t.Fatal("dry-run 不应写 kubelet.service")
	}
	if len(h.svc.Calls) != 0 {
		t.Fatalf("dry-run 不应调用服务管理: %v", h.svc.Calls)
	}
	if h.node.NodeReadyCalls != 0 {
		t.Fatal("dry-run 不应等待节点就绪")
	}
	if !h.report.contains("dry-run") {
		t.Fatalf("dry-run 必须报告将要做的动作: %v", h.report.msg)
	}
}

func TestMasterDoesNotNeedBootstrapFile(t *testing.T) {
	h := newHarness(t, "binary", "m1", true)
	// 故意不创建 kubelet.conf：master 的 kubelet.conf 由 kubeconfig 阶段直签。
	if err := h.phase.Run(context.Background(), h.pc); err != nil {
		t.Fatalf("master 不应等待 bootstrap: %v", err)
	}
}

func TestWorkerWaitsForBootstrapBeforeNodeReady(t *testing.T) {
	h := newHarness(t, "binary", "w1", true)
	err := h.phase.Run(context.Background(), h.pc)
	if err == nil {
		t.Fatal("worker 在 kubelet.conf 出现前不应成功")
	}
	if !strings.Contains(err.Error(), constants.KubeletKubeconfigName) {
		t.Fatalf("错误应说明在等 kubelet.conf: %v", err)
	}
	if h.node.NodeReadyCalls != 0 {
		t.Fatalf("kubelet.conf 未出现时不应查询 Node（调用了 %d 次）", h.node.NodeReadyCalls)
	}
	// 引导完成后同一次 install 重跑即可通过。
	h.writeBootstrapOutput(t)
	if err := h.phase.Run(context.Background(), h.pc); err != nil {
		t.Fatalf("引导完成后 Run 应成功: %v", err)
	}
}

func TestNodeNeverReadyTimesOutWithNodeAndHealthEndpoint(t *testing.T) {
	h := newHarness(t, "binary", "w1", true)
	h.writeBootstrapOutput(t)
	h.node.Ready["w1"] = false
	h.phase.Health = failHealth{err: errors.New("connection refused")}

	err := h.phase.Run(context.Background(), h.pc)
	if err == nil {
		t.Fatal("节点不 Ready 必须超时报错")
	}
	msg := err.Error()
	for _, want := range []string{"w1", "http://127.0.0.1:10248/healthz", "connection refused", "kubectl get csr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("超时错误 %q 缺少 %q", msg, want)
		}
	}
	if h.clock.sleeps == 0 {
		t.Fatal("超时前必须轮询等待")
	}
}

func TestMasterLocalHealthTimesOut(t *testing.T) {
	h := newHarness(t, "binary", "m1", true)
	h.phase.Health = failHealth{err: errors.New("connection refused")}
	err := h.phase.Run(context.Background(), h.pc)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("本机 kubelet 不健康时 master 必须超时报错: %v", err)
	}
}

func TestServiceRestartFailureRollsBackFiles(t *testing.T) {
	h := newHarness(t, "binary", "w1", true)
	h.writeBootstrapOutput(t)

	// 上一次安装留下的配置内容与新渲染结果不同。
	if err := os.MkdirAll(filepath.Dir(h.pc.Paths.KubeletConfig()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.pc.Paths.KubeletConfig(), []byte("previous-config"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.svc.RestartErr[kubeletpkg.UnitName] = errors.New("unit failed to start")

	err := h.phase.Run(context.Background(), h.pc)
	if err == nil {
		t.Fatal("服务启动失败必须让 Run 失败")
	}
	if !strings.Contains(err.Error(), "已回滚") {
		t.Fatalf("错误应说明已回滚: %v", err)
	}
	got, readErr := os.ReadFile(h.pc.Paths.KubeletConfig())
	if readErr != nil {
		t.Fatalf("旧配置应被恢复: %v", readErr)
	}
	if string(got) != "previous-config" {
		t.Fatalf("配置未回滚: %q", got)
	}
	if _, statErr := os.Stat(kubeletpkg.UnitPath(h.pc.Paths)); statErr == nil {
		t.Fatal("新写入的 unit 应在回滚时删除")
	}
	// 被替换的文件必须先备份到 /usr/local/kubemono/backup/<ts>/。
	backups := listFiles(t, h.pc.Paths.Backup())
	if len(backups) != 1 || filepath.Base(backups[0]) != filepath.Base(h.pc.Paths.KubeletConfig()) {
		t.Fatalf("备份文件不对: %v", backups)
	}
	if data, _ := os.ReadFile(backups[0]); string(data) != "previous-config" {
		t.Fatalf("备份内容 = %q", data)
	}
}

func TestInstallFailureStopsBeforeWritingFiles(t *testing.T) {
	h := newHarness(t, "binary", "w1", false) // 没有导入物料 / no artifact imported
	h.writeBootstrapOutput(t)
	err := h.phase.Run(context.Background(), h.pc)
	if err == nil {
		t.Fatal("物料缺失必须报错")
	}
	if !strings.Contains(err.Error(), "bundle load") {
		t.Fatalf("错误应告诉操作员导入物料: %v", err)
	}
	if _, statErr := os.Stat(h.pc.Paths.KubeletConfig()); statErr == nil {
		t.Fatal("二进制安装失败时不应写任何文件")
	}
}

func TestCheckDetectsBinaryDrift(t *testing.T) {
	h := newHarness(t, "binary", "w1", true)
	h.writeBootstrapOutput(t)
	ctx := context.Background()
	if err := h.phase.Run(ctx, h.pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	done, err := h.phase.Check(ctx, h.pc)
	if err != nil || !done {
		t.Fatalf("Check = %v, %v", done, err)
	}
	dir := h.inst.InstallDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubelet"), []byte("drift"), 0o755); err != nil {
		t.Fatal(err)
	}
	done, err = h.phase.Check(ctx, h.pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("二进制漂移时 Check 必须为假")
	}
}

func TestCheckDetectsMissingNodeClient(t *testing.T) {
	h := newHarness(t, "binary", "w1", true)
	h.writeBootstrapOutput(t)
	ctx := context.Background()
	if err := h.phase.Run(ctx, h.pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 模拟 worker 的 kubeconfig 不可用：Check 应按“未完成”处理而不是报错。
	h.phase.NodeClient = nil
	h.phase.NewNodeClient = func(*phase.Context) (kubeletpkg.NodeClient, error) {
		return nil, errors.New("缺少 kubelet.conf")
	}
	done, err := h.phase.Check(ctx, h.pc)
	if err != nil {
		t.Fatalf("Check 不应因为缺少客户端而失败: %v", err)
	}
	if done {
		t.Fatal("无法确认 Node Ready 时不能报告完成")
	}
}

func TestRunWarnsAboutPendingServingCSRs(t *testing.T) {
	h := newHarness(t, "binary", "m1", true)
	h.node.Pending = 2
	if err := h.phase.Run(context.Background(), h.pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.node.PendingCalls == 0 {
		t.Fatal("必须查询待审批的 serving CSR")
	}
	if n := h.report.warnCount("serving CSR"); n != 1 {
		t.Fatalf("应恰好告警一次待审批 CSR，实际 %d 次: %v", n, h.report.msg)
	}
}

// TestKubeletFlagsUseBootstrapForWorkersAndDirectForMasters 断言不会把两种
// kubeconfig 用混：worker 用引导令牌换证书，master 用直签凭据。
func TestKubeletFlagsUseBootstrapForWorkersAndDirectForMasters(t *testing.T) {
	prof, err := profile.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		node      string
		master    bool
		wantBoot  bool
		bootstrap string
		kubeconf  string
	}{
		{node: "m1", master: true},
		{node: "w1", master: false, wantBoot: true},
	} {
		t.Run(tc.node, func(t *testing.T) {
			cluster := clusterForTest("binary")
			host, _ := cluster.Cluster.Hosts.FindHost(tc.node)
			paths := constants.NewPaths(t.TempDir())
			rc := &render.Context{Cluster: cluster, Host: host, BOM: bom.MustLoad(), Paths: paths, Release: bom.MustLoad().Release}
			data, err := render.KubeletFlags(rc)
			if err != nil {
				t.Fatalf("KubeletFlags: %v", err)
			}
			bootstrapConf := paths.KubeconfigPath(constants.BootstrapKubeletKubeconfigName)
			kubeletConf := paths.KubeconfigPath(constants.KubeletKubeconfigName)

			if tc.master {
				if data.BootstrapKubeconfigFile != "" {
					t.Fatalf("master 不应引用引导 kubeconfig，实际 %q", data.BootstrapKubeconfigFile)
				}
			} else if data.BootstrapKubeconfigFile != bootstrapConf {
				t.Fatalf("worker 的引导 kubeconfig = %q, want %q", data.BootstrapKubeconfigFile, bootstrapConf)
			}
			if data.KubeletKubeconfigFile != kubeletConf {
				t.Fatalf("kubelet kubeconfig = %q, want %q", data.KubeletKubeconfigFile, kubeletConf)
			}

			flags, err := prof.RenderFlags(constants.ComponentKubelet, data)
			if err != nil {
				t.Fatalf("RenderFlags: %v", err)
			}
			joined := strings.Join(flags, " ")
			if !strings.Contains(joined, "--kubeconfig="+kubeletConf) {
				t.Errorf("缺少 --kubeconfig=%s: %s", kubeletConf, joined)
			}
			if tc.wantBoot {
				if !strings.Contains(joined, "--bootstrap-kubeconfig="+bootstrapConf) {
					t.Errorf("worker 必须用引导 kubeconfig: %s", joined)
				}
			} else if strings.Contains(joined, "--bootstrap-kubeconfig="+bootstrapConf) {
				t.Errorf("master 不应使用 worker 的引导 kubeconfig: %s", joined)
			}
			if strings.Contains(joined, constants.CAKeyName) {
				t.Errorf("kubelet 不应接触 CA 私钥: %s", joined)
			}
		})
	}
}

func TestAppliesOnlyToWorkers(t *testing.T) {
	p := &Phase{}
	if p.Applies(&phase.Context{}) {
		t.Fatal("没有 Host 时不适用")
	}
	if p.Applies(&phase.Context{Host: &clusterconfig.Host{Name: "m1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}}}) {
		t.Fatal("纯控制面 master 没有 kubelet")
	}
	for _, roles := range [][]clusterconfig.Role{{clusterconfig.RoleWorker}, {clusterconfig.RoleMaster, clusterconfig.RoleWorker}} {
		if !p.Applies(&phase.Context{Host: &clusterconfig.Host{Name: "n1", Roles: roles}}) {
			t.Fatalf("roles %v 应该运行 kubelet", roles)
		}
	}
}

// countFiles 统计 dir 下的普通文件数（目录不存在时为 0）。
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	return len(listFiles(t, dir))
}

// listFiles 返回 dir 下的所有普通文件。
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("遍历 %s 失败: %v", dir, err)
	}
	return out
}

func TestSwitchKubeletToLocalProxy(t *testing.T) {
	h := newHarness(t, "staticpod", "w1", false)
	path := h.pc.Paths.KubeconfigPath(constants.KubeletKubeconfigName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`apiVersion: v1
kind: Config
clusters:
- name: kubemono
  cluster: {server: "https://10.0.0.1:6443", insecure-skip-tls-verify: true}
contexts:
- name: kubemono
  context: {cluster: kubemono, user: kubelet}
current-context: kubemono
users:
- name: kubelet
  user: {token: bootstrap}
`)
	if err := os.WriteFile(path, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	h.phase.switchKubeletToLocalProxy(h.pc)
	loaded, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cl := loaded.Clusters[loaded.Contexts[loaded.CurrentContext].Cluster]
	want := fmt.Sprintf("https://127.0.0.1:%d", constants.DefaultLocalProxyPort)
	if cl == nil || cl.Server != want {
		t.Fatalf("kubelet.conf server = %v, want %s", cl, want)
	}
}

func TestNeedLocalProxySwitch(t *testing.T) {
	h := newHarness(t, "staticpod", "w1", false)
	path := h.pc.Paths.KubeconfigPath(constants.KubeletKubeconfigName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`apiVersion: v1
kind: Config
clusters:
- name: kubemono
  cluster: {server: "https://10.0.0.1:6443", insecure-skip-tls-verify: true}
contexts:
- name: kubemono
  context: {cluster: kubemono, user: kubelet}
current-context: kubemono
users:
- name: kubelet
  user: {token: bootstrap}
`)
	if err := os.WriteFile(path, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if !h.phase.needLocalProxySwitch(h.pc) {
		t.Fatal("直连 master 的 kubelet.conf 应需要切换")
	}
	h.phase.switchKubeletToLocalProxy(h.pc)
	if h.phase.needLocalProxySwitch(h.pc) {
		t.Fatal("切到 127.0.0.1:6443 后不应再需要切换")
	}
}

func TestWaitLocalProxyReady(t *testing.T) {
	h := newHarness(t, "staticpod", "w1", false)
	h.phase.Health = failHealth{err: errors.New("haproxy down")}
	h.phase.WaitKnobs = phase.WaitKnobs{
		Timeout: 5 * time.Millisecond, Interval: time.Millisecond,
		Sleep: func(time.Duration) {}, Clock: h.clock.Now,
	}
	if err := h.phase.waitLocalProxyReady(context.Background(), h.pc); err == nil {
		t.Fatal("本地代理未就绪时应返回错误")
	}
	h.phase.Health = okHealth{}
	if err := h.phase.waitLocalProxyReady(context.Background(), h.pc); err != nil {
		t.Fatalf("本地代理就绪后应通过: %v", err)
	}
}
