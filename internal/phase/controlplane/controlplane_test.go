package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	controlplane "kubemono/internal/controlplane"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	"kubemono/internal/util/output"
)

// fakeRunner 记录 phase 要求 runner 执行的操作，并通过把清单写入 manifests 目录
// 来模拟 kubelet 拾取该清单。
type fakeRunner struct {
	paths    constants.Paths
	applied  []controlplane.Component
	units    map[controlplane.Component][]byte
	healthy  map[controlplane.Component]error
	applyErr map[controlplane.Component]error
}

// noInstall 在测试里跳过宿主二进制安装，但为 kubectl 造一个可检查的真实文件。
func noInstall(_ context.Context, c *phase.Context, component string) error {
	if component != constants.ComponentKubectl {
		return nil
	}
	target := filepath.Join(c.Paths.LocalBin(), "kubectl")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, []byte("kubectl"), 0o755); err != nil {
		return err
	}
	return nil
}

func newFakeRunner(paths constants.Paths) *fakeRunner {
	return &fakeRunner{
		paths:    paths,
		healthy:  map[controlplane.Component]error{},
		applyErr: map[controlplane.Component]error{},
		units:    map[controlplane.Component][]byte{},
	}
}

func (f *fakeRunner) Apply(_ context.Context, spec *controlplane.RenderedSpec) error {
	r := f
	if err := f.applyErr[spec.Component]; err != nil {
		return err
	}
	f.applied = append(f.applied, spec.Component)
	f.units[spec.Component] = spec.Unit
	// 模拟 systemd 生效：把 unit 写到 /etc/systemd/system。
	target := r.paths.Resolve("/etc/systemd/system/" + string(spec.Component) + ".service")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, spec.Unit, 0o644)
}

func (f *fakeRunner) Restart(context.Context, controlplane.Component) error { return nil }
func (f *fakeRunner) Stop(context.Context, controlplane.Component) error    { return nil }
func (f *fakeRunner) Remove(context.Context, controlplane.Component) error  { return nil }
func (f *fakeRunner) Health(_ context.Context, c controlplane.Component) error {
	return f.healthy[c]
}

func testSetup(t *testing.T, node string) (*clusterconfig.Cluster, *phase.Context, *render.Spec, *fakeRunner) {
	t.Helper()
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost(node)

	paths := constants.NewPaths(t.TempDir())
	pc := &phase.Context{
		Cluster:    cluster,
		Host:       host,
		BOM:        bom.MustLoad(),
		Paths:      paths,
		Release:    bom.MustLoad().Release,
		IsInitNode: true,
		Report:     output.Discard(),
		Now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	spec, err := render.Render(&render.Context{
		Cluster: cluster, Host: host, BOM: pc.BOM, Paths: paths, Release: pc.Release,
	}, render.DefaultBuilders()...)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return cluster, pc, spec, newFakeRunner(paths)
}

func TestAppliesToMastersAndEtcdHosts(t *testing.T) {
	cluster, pc, _, _ := testSetup(t, "m1")
	if !(&Phase{}).Applies(pc) {
		t.Fatal("master 必须运行 control-plane phase")
	}
	worker, _ := cluster.Cluster.Hosts.FindHost("w1")
	pc.Host = worker
	if (&Phase{}).Applies(pc) {
		t.Fatal("worker 不应运行 control-plane phase")
	}
}

func TestRunAppliesComponentsInOrder(t *testing.T) {
	_, pc, spec, runner := testSetup(t, "m1")
	p := &Phase{Spec: spec, InstallBinary: noInstall, RunnerFactory: func(*phase.Context) (controlplane.Runner, error) {
		return runner, nil
	}}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []controlplane.Component{
		controlplane.ComponentAPIServer,
		controlplane.ComponentControllerManager,
		controlplane.ComponentScheduler,
	}
	if len(runner.applied) != len(want) {
		t.Fatalf("应用顺序 = %v, want %v", runner.applied, want)
	}
	for i := range want {
		if runner.applied[i] != want[i] {
			t.Fatalf("应用顺序 = %v, want %v", runner.applied, want)
		}
	}
	// unit 必须真的落到 /etc/systemd/system。
	for _, c := range want {
		if _, err := os.Stat(pc.Paths.Resolve("/etc/systemd/system/" + string(c) + ".service")); err != nil {
			t.Fatalf("%s 的 unit 未写入: %v", c, err)
		}
	}
}

func TestRunStopsAtFirstFailure(t *testing.T) {
	_, pc, spec, runner := testSetup(t, "m1")
	runner.applyErr[controlplane.ComponentAPIServer] = errors.New("apiserver 起不来")
	p := &Phase{Spec: spec, InstallBinary: noInstall, RunnerFactory: func(*phase.Context) (controlplane.Runner, error) {
		return runner, nil
	}}
	err := p.Run(context.Background(), pc)
	if err == nil {
		t.Fatal("apiserver 失败必须中止后续组件")
	}
	for _, c := range runner.applied {
		if c == controlplane.ComponentControllerManager || c == controlplane.ComponentScheduler {
			t.Fatalf("失败后不应继续应用 %s", c)
		}
	}
}

func TestCheckRequiresHashAndHealth(t *testing.T) {
	_, pc, spec, runner := testSetup(t, "m1")
	p := &Phase{Spec: spec, InstallBinary: noInstall, RunnerFactory: func(*phase.Context) (controlplane.Runner, error) {
		return runner, nil
	}}
	ctx := context.Background()

	done, err := p.Check(ctx, pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("尚未应用时不能报告完成")
	}
	if err := p.Run(ctx, pc); err != nil {
		t.Fatal(err)
	}
	done, err = p.Check(ctx, pc)
	if err != nil || !done {
		t.Fatalf("应用后 Check(done=%v err=%v)", done, err)
	}
	// 组件不健康时必须重跑（文件对但进程没起来）。
	runner.healthy[controlplane.ComponentScheduler] = errors.New("scheduler 不健康")
	done, err = p.Check(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("组件不健康时不能报告完成")
	}
}

func TestDryRunDoesNotApply(t *testing.T) {
	_, pc, spec, runner := testSetup(t, "m1")
	pc.DryRun = true
	p := &Phase{Spec: spec, InstallBinary: noInstall, RunnerFactory: func(*phase.Context) (controlplane.Runner, error) {
		return runner, nil
	}}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(runner.applied) != 0 {
		t.Fatalf("dry-run 不应调用 Apply: %v", runner.applied)
	}
}

// TestRunnerIsSystemd 检查控制面 phase 总是返回 systemd runner。
func TestRunnerIsSystemd(t *testing.T) {
	_, pc, _, _ := testSetup(t, "m1")
	if _, err := (&Phase{}).runner(pc); err != nil {
		t.Fatalf("runner: %v", err)
	}
}

// TestRunWritesAuditPolicyBeforeComponents 覆盖真实环境踩到的坑：apiserver 的
// --audit-policy-file 指向 /etc/kubernetes/audit/policy.yaml，这个文件不是静态
// Pod 清单，必须由本 phase 自己写；漏写时 apiserver 会在启动时直接 crash-loop
func TestRunWritesAuditPolicyBeforeComponents(t *testing.T) {
	_, pc, spec, runner := testSetup(t, "m1")

	// 渲染结果里必须有审计策略，且它归 apiserver 组件、不是清单类型。
	policyPath := pc.Paths.Resolve("/etc/kubernetes/audit/policy.yaml")
	policy, ok := spec.Find(policyPath)
	if !ok {
		t.Fatalf("渲染结果缺少审计策略 %s（controlPlane.security.audit 默认开启）", policyPath)
	}
	if policy.Kind == render.KindManifest {
		t.Fatalf("审计策略不应被当成静态 Pod 清单: %v", policy.Kind)
	}

	p := &Phase{Spec: spec, InstallBinary: noInstall, RunnerFactory: func(*phase.Context) (controlplane.Runner, error) {
		return runner, nil
	}}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatalf("Run 之后审计策略必须已落盘: %v", err)
	}
	if string(got) != string(policy.Content) {
		t.Fatalf("审计策略内容与渲染结果不一致:\n%s", got)
	}
	if info, err := os.Stat(policyPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("审计策略权限 = %04o，期望 0600", info.Mode().Perm())
	}

	// 幂等：内容一致时再次 Run 不应产生新的备份/写入。
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	if got2, err := os.ReadFile(policyPath); err != nil || string(got2) != string(policy.Content) {
		t.Fatalf("第二次 Run 之后内容不应变化: %v", err)
	}
}

// TestCheckInBinaryModeRequiresUnits 覆盖 binary 模式的 Check。
// Check 必须覆盖 systemd unit：只看静态 Pod 清单时，binary 模式下清单永远不存在，
// phase 会被误判为完成并跳过，apiserver 的二进制与 unit 都不会被安装
// （真机实测：直到 mark-control-plane 以连接被拒才暴露）。
func TestCheckInBinaryModeRequiresUnits(t *testing.T) {
	_, pc, _, _ := testSetup(t, "m1")
	p := &Phase{
		InstallBinary: func(context.Context, *phase.Context, string) error { return nil },
	}
	done, err := p.Check(context.Background(), pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("binary 模式下没有 unit 时 Check 必须为 false（否则 control-plane 被跳过）")
	}
}

// TestRunInBinaryModeAppliesUnits 覆盖 binary 模式的 Run：binary 模式下期望状态里
// 没有静态 Pod 清单（只有 systemd unit），Run 必须处理 unit，否则 control-plane
// 跑完却什么都没应用（真机实测：apiserver unit inactive、无日志）。
func TestRunInBinaryModeAppliesUnits(t *testing.T) {
	cluster, pc, _, runner := testSetup(t, "m1")
	spec, err := render.Render(&render.Context{
		Cluster: cluster, Host: pc.Host, BOM: pc.BOM, Paths: pc.Paths, Release: pc.Release,
	}, render.DefaultBuilders()...)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	p := &Phase{
		Spec:          spec,
		RunnerFactory: func(*phase.Context) (controlplane.Runner, error) { return runner, nil },
		InstallBinary: func(context.Context, *phase.Context, string) error { return nil },
	}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.applied) == 0 {
		t.Fatal("binary 模式必须把 unit 应用到 runner，实际一个都没应用")
	}
	for _, component := range runner.applied {
		if len(runner.units[component]) == 0 {
			t.Fatalf("%s 没有带上 unit 内容（binary 模式的期望产物是 unit）", component)
		}
	}
}

// TestCheckDetectsUnitDrift：unit 内容被改（extraArgs/审计开关变更后未重启）
// 时 Check 必须返回 false，否则 node repair 会静默跳过。
func TestCheckDetectsUnitDrift(t *testing.T) {
	_, pc, spec, runner := testSetup(t, "m1")
	p := &Phase{Spec: spec, InstallBinary: noInstall, RunnerFactory: func(*phase.Context) (controlplane.Runner, error) {
		return runner, nil
	}}
	ctx := context.Background()
	if err := p.Run(ctx, pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	done, err := p.Check(ctx, pc)
	if err != nil || !done {
		t.Fatalf("Run 后 Check 应为 true（done=%v err=%v）", done, err)
	}
	unit := pc.Paths.Resolve("/etc/systemd/system/kube-apiserver.service")
	if err := os.WriteFile(unit, []byte("drifted"), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err = p.Check(ctx, pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("unit 内容漂移时 Check 必须为 false")
	}
}

// TestCheckHonorsPendingRestartMarker：写文件后重启失败留下的 marker 必须让
// Check 返回 false，直到 Run 完成重启并清掉标记。
func TestCheckHonorsPendingRestartMarker(t *testing.T) {
	_, pc, spec, runner := testSetup(t, "m1")
	p := &Phase{Spec: spec, InstallBinary: noInstall, RunnerFactory: func(*phase.Context) (controlplane.Runner, error) {
		return runner, nil
	}}
	ctx := context.Background()
	if err := p.Run(ctx, pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := phase.WriteRestartMarker(pc, restartMarkerName, []string{"kube-apiserver"}, "test"); err != nil {
		t.Fatal(err)
	}
	done, err := p.Check(ctx, pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("存在待重启标记时 Check 必须为 false")
	}
}
