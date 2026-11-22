package runtime

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

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	kubemonoruntime "kubemono/internal/runtime"
	"kubemono/internal/util/output"
)

// recordingReporter 记录所有 Infof 消息，便于断言 phase 的上报内容。
type recordingReporter struct {
	mu    sync.Mutex
	infos []string
}

func (r *recordingReporter) Debugf(string, ...any)       {}
func (r *recordingReporter) Infof(f string, a ...any)    { r.add(f, a...) }
func (r *recordingReporter) Warnf(string, ...any)        {}
func (r *recordingReporter) Errorf(string, ...any)       {}
func (r *recordingReporter) Step(string) func(error)     { return func(error) {} }
func (r *recordingReporter) Result(bool, string, ...any) {}

func (r *recordingReporter) add(format string, args ...any) {
	r.mu.Lock()
	r.infos = append(r.infos, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

// messages 返回记录到的消息。
func (r *recordingReporter) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.infos...)
}

// contains 判断是否记录到包含某子串的消息。
func (r *recordingReporter) contains(sub string) bool {
	for _, m := range r.messages() {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// phaseServices 是 phase 测试用的 ServiceManager（记录动作与 active 状态）。
type phaseServices struct {
	mu      sync.Mutex
	active  map[string]bool
	actions []string
}

// newPhaseServices 返回一个把给定 unit 视为 active 的服务管理器。
func newPhaseServices(active ...string) *phaseServices {
	s := &phaseServices{active: map[string]bool{}}
	for _, unit := range active {
		s.active[unit] = true
	}
	return s
}

// Action 实现 kubemonoruntime.ServiceManager。
func (s *phaseServices) Action(_ context.Context, unit, action string) error {
	s.mu.Lock()
	s.actions = append(s.actions, action+":"+unit)
	s.mu.Unlock()
	return nil
}

// DaemonReload 实现 kubemonoruntime.ServiceManager。
func (s *phaseServices) DaemonReload(context.Context) error {
	s.mu.Lock()
	s.actions = append(s.actions, "daemon-reload")
	s.mu.Unlock()
	return nil
}

// IsActive 实现 kubemonoruntime.ServiceManager。
func (s *phaseServices) IsActive(_ context.Context, unit string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[unit], nil
}

// recorded 返回全部动作。
func (s *phaseServices) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.actions...)
}

// equalStrings 比较两个字符串切片。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// testCluster 返回一个含一个 master、一个 worker 的集群配置。
func testCluster(t *testing.T, runtimeType string) *clusterconfig.Cluster {
	t.Helper()
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(c)
	if runtimeType == clusterconfig.RuntimeCustom {
		c.Node.Runtime.Custom = clusterconfig.CustomRuntimeSpec{
			CRISocket:    "unix:///run/crio/crio.sock",
			CgroupDriver: "systemd",
			ServiceName:  "crio",
		}
	}
	return c
}

// testPhaseContext 返回指向临时目录的 phase.Context（w1 节点）。
func testPhaseContext(t *testing.T, cluster *clusterconfig.Cluster, root string, report output.Reporter) *phase.Context {
	t.Helper()
	host, ok := cluster.Cluster.Hosts.FindHost("w1")
	if !ok {
		t.Fatal("配置里没有 w1")
	}
	return &phase.Context{
		Cluster: cluster,
		Host:    host,
		BOM:     bom.MustLoad(),
		Paths:   constants.NewPaths(root),
		Release: "v1.31.4-km.1",
		Report:  report,
		Now:     time.Now,
	}
}

// testSpec 返回只含 containerd 受管文件的渲染结果。
func testSpec(paths constants.Paths) *render.Spec {
	rc := &render.Context{Paths: paths}
	spec := &render.Spec{}
	spec.AddFile(render.ContainerdConfigPath(rc), []byte("version = 2\n"), 0o644, "containerd", render.KindConf, render.ActionRestart)
	spec.AddFile(filepath.Join(render.ContainerdCertsDir(rc), "reg.example.com", "hosts.toml"),
		[]byte("server = \"https://reg.example.com\"\n"), 0o644, "containerd", render.KindConf, render.ActionNone)
	spec.Sort()
	return spec
}

// fakeProvider 是 phase 测试用的运行时实现。
type fakeProvider struct {
	paths      constants.Paths
	installDir string
	bins       []kubemonoruntime.Binary
	spec       *render.Spec
	decision   kubemonoruntime.ConfigDecision
	applied    []render.AppliedChange
	healthy    bool
	statusErr  error

	mu        sync.Mutex
	calls     []string
	installed bool
	restarts  int
}

func (f *fakeProvider) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeProvider) called(call string) bool { return f.callCount(call) > 0 }

func (f *fakeProvider) callCount(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (f *fakeProvider) allCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Name 实现 kubemonoruntime.Provider。
func (f *fakeProvider) Name() string { return "fake" }

// Install 实现 kubemonoruntime.Provider：把必需二进制写入临时目录。
func (f *fakeProvider) Install(context.Context) error {
	f.record("install")
	for _, b := range f.bins {
		if !b.Required {
			continue
		}
		dst := kubemonoruntime.LinkPath(f.installDir, b.Name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(b.Name), 0o755); err != nil {
			return err
		}
	}
	// 补齐 Check 需要的 CNI 插件。
	cniDir := f.paths.CNIBin()
	if err := os.MkdirAll(cniDir, 0o755); err != nil {
		return err
	}
	for _, name := range kubemonoruntime.CNIPlugins(bom.MustLoad()) {
		if err := os.WriteFile(filepath.Join(cniDir, name), []byte(name), 0o755); err != nil {
			return err
		}
	}
	f.installed = true
	return nil
}

// ApplyConfig 实现 kubemonoruntime.Provider。
func (f *fakeProvider) ApplyConfig(_ context.Context, spec *render.Spec) (*kubemonoruntime.Plan, error) {
	f.record("apply")
	f.spec = spec
	// 生产实现会同时渲染 containerd.service；fake 也写同一份 unit，
	// 这样 Check 的 unit 内容比较在被测路径上成立。
	unit, err := kubemonoruntime.RenderUnit()
	if err != nil {
		return nil, err
	}
	unitPath := f.paths.Resolve(kubemonoruntime.UnitPath)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(unitPath, unit, 0o644); err != nil {
		return nil, err
	}
	return &kubemonoruntime.Plan{Decision: f.decision, Applied: f.applied}, nil
}

// Status 实现 kubemonoruntime.Provider。
func (f *fakeProvider) Status(context.Context) (kubemonoruntime.Status, error) {
	f.record("status")
	if f.statusErr != nil {
		return kubemonoruntime.Status{}, f.statusErr
	}
	hash := ""
	if f.spec != nil {
		hash = kubemonoruntime.DesiredConfigHash(f.spec)
	}
	st := kubemonoruntime.Status{Running: f.installed && f.healthy, ConfigHash: hash, CRISocket: kubemonoruntime.DefaultCRISocket}
	if st.Running {
		st.Message = "containerd 运行中"
	}
	return st, nil
}

// Restart 实现 kubemonoruntime.Provider。
func (f *fakeProvider) Restart(context.Context) error {
	f.record("restart")
	f.mu.Lock()
	f.restarts++
	f.mu.Unlock()
	return nil
}

// newFakeProvider 构造与集群配置匹配的假 provider。
func newFakeProvider(cluster *clusterconfig.Cluster, root, installDir string) *fakeProvider {
	return &fakeProvider{
		paths:      constants.NewPaths(root),
		installDir: installDir,
		bins:       kubemonoruntime.Binaries(cluster.Node.Runtime, bom.MustLoad()),
		healthy:    true,
	}
}

// filesUnder 返回 root 下的普通文件相对路径。
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", root, err)
	}
	return out
}

// TestPhaseMetadata 校验注册名与适用性。
func TestPhaseMetadata(t *testing.T) {
	p := &Phase{}
	if p.Applies(&phase.Context{}) {
		t.Error("没有 Host 时 runtime phase 不应适用")
	}
	if p.Applies(&phase.Context{Host: &clusterconfig.Host{Name: "m1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}}}) {
		t.Error("纯控制面 master 没有容器运行时")
	}
	for _, roles := range [][]clusterconfig.Role{{clusterconfig.RoleWorker}, {clusterconfig.RoleMaster, clusterconfig.RoleWorker}} {
		if !p.Applies(&phase.Context{Host: &clusterconfig.Host{Name: "n1", Roles: roles}}) {
			t.Errorf("roles %v 应该安装运行时", roles)
		}
	}
}

// TestPhaseRendersDesiredWithoutOverride 未注入 Spec 时走 render.DefaultBuilders
// 的完整渲染路径（证明 phase 与 desired 生成器接线正确）。
func TestPhaseRendersDesiredWithoutOverride(t *testing.T) {
	root := t.TempDir()
	cluster := testCluster(t, clusterconfig.RuntimeContainerd)
	c := testPhaseContext(t, cluster, root, &recordingReporter{})
	p := &Phase{
		Services: newPhaseServices(),
		Commands: &fakeRunnerForPhase{},
	}
	// 全新节点上二进制缺失：Check 必须返回 (false, nil) 而不是渲染错误。
	done, err := p.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("Check 渲染 desired 失败: %v", err)
	}
	if done {
		t.Error("未安装运行时，Check 应为 false")
	}
}

// TestPhaseFreshNodeCheckRunCheck 覆盖“未就绪 → Run → 就绪”的幂等路径。
func TestPhaseFreshNodeCheckRunCheck(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	installDir := filepath.Join(root, "usr", "local", "bin")
	cluster := testCluster(t, clusterconfig.RuntimeContainerd)
	report := &recordingReporter{}
	c := testPhaseContext(t, cluster, root, report)
	fp := newFakeProvider(cluster, root, installDir)
	svc := newPhaseServices() // 尚未启动 / not started yet

	p := &Phase{
		ProviderFactory: func(*phase.Context) (kubemonoruntime.Provider, error) { return fp, nil },
		Services:        svc,
		ProviderOptions: kubemonoruntime.Options{InstallDir: installDir},
		Spec:            testSpec(c.Paths),
	}

	done, err := p.Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("全新节点上 Check 应为 false")
	}

	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, call := range []string{"install", "apply"} {
		if !fp.called(call) {
			t.Errorf("Run 应调用 %s，实际调用 %v", call, fp.allCalls())
		}
	}
	want := []string{"daemon-reload", "enable:containerd", "start:containerd"}
	if got := svc.recorded(); !equalStrings(got, want) {
		t.Errorf("服务动作 = %v，期望 %v", got, want)
	}

	svc.active["containerd"] = true
	done, err = p.Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatalf("Run 之后 Check 应为 true；provider 调用 %v", fp.allCalls())
	}
}

// TestPhaseCheckDetectsConfigDrift 配置漂移时 Check 为 false。
func TestPhaseCheckDetectsConfigDrift(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	installDir := filepath.Join(root, "usr", "local", "bin")
	cluster := testCluster(t, clusterconfig.RuntimeContainerd)
	c := testPhaseContext(t, cluster, root, output.Discard())
	fp := newFakeProvider(cluster, root, installDir)
	svc := newPhaseServices("containerd")

	p := &Phase{
		ProviderFactory: func(*phase.Context) (kubemonoruntime.Provider, error) { return fp, nil },
		Services:        svc,
		ProviderOptions: kubemonoruntime.Options{InstallDir: installDir},
		Spec:            testSpec(c.Paths),
	}
	if err := fp.Install(ctx); err != nil {
		t.Fatal(err)
	}
	fp.spec = &render.Spec{} // 摘要为空 → 与期望不一致 / empty hash → mismatch with desired
	done, err := p.Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Error("配置漂移时 Check 应为 false")
	}
}

// TestPhaseCustomUsesConfiguredService custom 运行时按 serviceName 启停。
func TestPhaseCustomUsesConfiguredService(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	installDir := filepath.Join(root, "usr", "local", "bin")
	cluster := testCluster(t, clusterconfig.RuntimeCustom)
	c := testPhaseContext(t, cluster, root, &recordingReporter{})
	fp := newFakeProvider(cluster, root, installDir)
	svc := newPhaseServices()

	p := &Phase{
		ProviderFactory: func(*phase.Context) (kubemonoruntime.Provider, error) { return fp, nil },
		Services:        svc,
		ProviderOptions: kubemonoruntime.Options{InstallDir: installDir},
		Spec:            &render.Spec{},
	}
	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := svc.recorded()
	want := []string{"daemon-reload", "enable:crio", "start:crio"}
	if !equalStrings(got, want) {
		t.Errorf("服务动作 = %v，期望 %v", got, want)
	}
	// custom 运行时没有受管配置，Check 只看 socket 与服务。
	svc.active["crio"] = true
	done, err := p.Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Error("custom 运行时就绪后 Check 应为 true")
	}
}

// TestPhaseDryRunWritesNothing dry-run 走真实 provider 也不写任何文件。
func TestPhaseDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	installDir := filepath.Join(root, "usr", "local", "bin")
	cluster := testCluster(t, clusterconfig.RuntimeContainerd)
	c := testPhaseContext(t, cluster, root, &recordingReporter{})
	c.DryRun = true
	svc := newPhaseServices()

	p := &Phase{
		Services:        svc,
		Commands:        &fakeRunnerForPhase{},
		ProviderOptions: kubemonoruntime.Options{InstallDir: installDir},
		Spec:            testSpec(c.Paths),
	}
	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if got := svc.recorded(); len(got) != 0 {
		t.Errorf("dry-run 不应有服务动作，实际 %v", got)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("dry-run 不应写任何文件，实际 %v", files)
	}
}

// fakeRunnerForPhase 是 phase 测试用的命令执行器；dry-run 下不应被调用。
type fakeRunnerForPhase struct{}

// Run 实现 kubemonoruntime.CommandRunner。
func (*fakeRunnerForPhase) Run(context.Context, string, ...string) (kubemonoruntime.CommandResult, error) {
	return kubemonoruntime.CommandResult{}, errors.New("dry-run 不应执行命令")
}

// TestPhaseCRITimeoutNamesSocket CRI 始终无应答时有界超时，错误里带 socket。
func TestPhaseCRITimeoutNamesSocket(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	installDir := filepath.Join(root, "usr", "local", "bin")
	cluster := testCluster(t, clusterconfig.RuntimeContainerd)
	c := testPhaseContext(t, cluster, root, &recordingReporter{})
	fp := newFakeProvider(cluster, root, installDir)
	fp.statusErr = errors.New("dial unix /run/containerd/containerd.sock: connect: no such file or directory")
	svc := newPhaseServices()

	p := &Phase{
		ProviderFactory: func(*phase.Context) (kubemonoruntime.Provider, error) { return fp, nil },
		Services:        svc,
		WaitKnobs: phase.WaitKnobs{
			Sleep:    func(time.Duration) {},
			Timeout:  5 * time.Millisecond,
			Interval: time.Millisecond,
		},
		ProviderOptions: kubemonoruntime.Options{InstallDir: installDir},
		Spec:            testSpec(c.Paths),
	}
	err := p.Run(ctx, c)
	if err == nil {
		t.Fatal("CRI 无应答时应超时报错")
	}
	msg := err.Error()
	for _, want := range []string{kubemonoruntime.DefaultCRISocket, "应答", "journalctl", "/var/lib/containerd"} {
		if !strings.Contains(msg, want) {
			t.Errorf("超时错误应包含 %q: %v", want, msg)
		}
	}
	if n := fp.callCount("status"); n == 0 || n > 10 {
		t.Errorf("等待应有界且至少探测一次，实际 %d 次", n)
	}
}

// TestPhaseRecordsConfigDecision 记录并上报重启 / 热加载判定。
func TestPhaseRecordsConfigDecision(t *testing.T) {
	cases := []struct {
		decision kubemonoruntime.ConfigDecision
		wantMsg  string
	}{
		{kubemonoruntime.DecisionRestart, "重启"},
		{kubemonoruntime.DecisionReload, "热加载"},
		{kubemonoruntime.DecisionNone, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.decision), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			installDir := filepath.Join(root, "usr", "local", "bin")
			cluster := testCluster(t, clusterconfig.RuntimeContainerd)
			report := &recordingReporter{}
			c := testPhaseContext(t, cluster, root, report)
			fp := newFakeProvider(cluster, root, installDir)
			fp.decision = tc.decision
			svc := newPhaseServices()

			p := &Phase{
				ProviderFactory: func(*phase.Context) (kubemonoruntime.Provider, error) { return fp, nil },
				Services:        svc,
				ProviderOptions: kubemonoruntime.Options{InstallDir: installDir},
				Spec:            testSpec(c.Paths),
			}
			if err := p.Run(ctx, c); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !fp.called("apply") {
				t.Error("provider 应走 ApplyConfig")
			}
			plan := p.last
			if plan == nil {
				t.Fatal("LastPlan 不应为 nil")
			}
			if plan.Decision != tc.decision {
				t.Errorf("记录的判定 = %s，期望 %s", plan.Decision, tc.decision)
			}
			if tc.wantMsg != "" && !report.contains(tc.wantMsg) {
				t.Errorf("上报消息应包含 %q，实际 %v", tc.wantMsg, report.messages())
			}
		})
	}
}

// TestPhaseRollbackRestoresFiles 回滚恢复配置并重启服务。

// TestRuntimeCheckDetectsUnitDriftAndPendingRestart：containerd.service 不在
// render.Spec 里，Check 必须单独比较它；写后未重启的 marker 也必须让 Check 为 false。
func TestRuntimeCheckDetectsUnitDriftAndPendingRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	installDir := filepath.Join(root, "usr", "local", "bin")
	cluster := testCluster(t, clusterconfig.RuntimeContainerd)
	c := testPhaseContext(t, cluster, root, output.Discard())
	fp := newFakeProvider(cluster, root, installDir)
	svc := newPhaseServices()
	p := &Phase{
		ProviderFactory: func(*phase.Context) (kubemonoruntime.Provider, error) { return fp, nil },
		Services:        svc,
		ProviderOptions: kubemonoruntime.Options{InstallDir: installDir},
		Spec:            testSpec(c.Paths),
	}
	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	svc.active["containerd"] = true
	done, err := p.Check(ctx, c)
	if err != nil || !done {
		t.Fatalf("Run 后 Check 应为 true（done=%v err=%v）", done, err)
	}
	unitPath := c.Paths.Resolve(kubemonoruntime.UnitPath)
	if err := os.WriteFile(unitPath, []byte("drifted"), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err = p.Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("containerd.service 漂移时 Check 必须为 false")
	}
	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := phase.WriteRestartMarker(c, restartMarkerName, []string{"containerd"}, "test"); err != nil {
		t.Fatal(err)
	}
	done, err = p.Check(ctx, c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("存在待重启标记时 Check 必须为 false")
	}
}

// TestRuntimeRunRetriesPendingRestart：上次“写配置后重启失败”的标记必须让
// 本次 Run 补一次 restart（即使 ApplyConfig 判定无变化）。
func TestRuntimeRunRetriesPendingRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	installDir := filepath.Join(root, "usr", "local", "bin")
	cluster := testCluster(t, clusterconfig.RuntimeContainerd)
	c := testPhaseContext(t, cluster, root, output.Discard())
	fp := newFakeProvider(cluster, root, installDir)
	fp.decision = kubemonoruntime.DecisionNone
	svc := newPhaseServices()
	svc.active["containerd"] = true
	p := &Phase{
		ProviderFactory: func(*phase.Context) (kubemonoruntime.Provider, error) { return fp, nil },
		Services:        svc,
		ProviderOptions: kubemonoruntime.Options{InstallDir: installDir},
		Spec:            testSpec(c.Paths),
	}
	if err := phase.WriteRestartMarker(c, restartMarkerName, []string{"containerd"}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "restart:containerd"
	for _, call := range svc.recorded() {
		if call == want {
			return
		}
	}
	t.Fatalf("Run 应补 restart，实际动作 %v", svc.recorded())
}
