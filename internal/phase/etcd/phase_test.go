package etcd

import (
	"context"
	"os"
	"strings"
	"testing"

	"kubemono/internal/constants"
	etcdapi "kubemono/internal/etcd"
	"kubemono/internal/phase"
)

func TestPhaseRegisteredAsEtcd(t *testing.T) {
	reg := phase.Default()
	if !reg.Has(PhaseName) {
		t.Fatalf("phase %q 未注册：%v", PhaseName, reg.Names())
	}
	if got := (&Phase{Mode: ModeInit}).Mode; got != ModeInit {
		t.Fatalf("ModeInit = %s", got)
	}
}

func TestModeAutoSelectsInitForInitNode(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	p := &Phase{}
	initCtx, _ := newPhaseContext(t, cluster, &hosts[0], nil)
	if got := p.mode(initCtx); got != ModeInit {
		t.Fatalf("init 节点应选 ModeInit，got %s", got)
	}
	joinCtx, _ := newPhaseContext(t, cluster, &hosts[1], nil)
	if got := p.mode(joinCtx); got != ModeJoin {
		t.Fatalf("其他 master 应选 ModeJoin，got %s", got)
	}
}

func TestAppliesMatrix(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "w1")
	worker := &hosts[1]
	// stacked 的 worker 不运行 etcd。
	local := newFakeLocal()
	pc, _ := newPhaseContext(t, cluster, worker, nil)
	p := testPhase(ModeAuto, &fakeAPI{}, local, &fakeServices{}, newTestClock(), nil)
	if p.Applies(pc) {
		t.Fatal("stacked worker 不应适用 etcd phase")
	}
	// stacked master 适用。
	masterCtx, _ := newPhaseContext(t, cluster, &hosts[0], nil)
	if !p.Applies(masterCtx) {
		t.Fatal("stacked master 必须适用 etcd phase")
	}
	// external unmanaged：任何节点都不适用。
	unmanaged, uhosts := testCluster(true, "m1")
	uctx, _ := newPhaseContext(t, unmanaged, &uhosts[0], nil)
	if p.Applies(uctx) {
		t.Fatal("external+unmanaged 不应适用 etcd phase")
	}
}

func TestInitRunStartsLocalEtcdAndWaitsHealthy(t *testing.T) {
	cluster, hosts := testCluster(false, "m1")
	clock := newTestClock()
	api := &fakeAPI{}
	statusCalls := 0
	api.statusFn = func(context.Context, string) (etcdapi.Status, error) {
		statusCalls++
		if statusCalls < 3 {
			return etcdapi.Status{}, context.DeadlineExceeded
		}
		return etcdapi.Status{Healthy: true, MemberID: 1, RaftIndex: 42}, nil
	}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return []etcdapi.Member{memberHealthy(1, "m1")}, nil
	}
	local := newFakeLocal()
	svc := &fakeServices{}
	paths := constants.NewPaths(t.TempDir())
	spec := unitSpec(paths, "m1", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m1", "m1=https://10.0.0.1:2380")
	pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
	pc.Paths = paths
	p := testPhase(ModeInit, api, local, svc, clock, hook)

	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 数据目录就绪 + etcd.service 落盘 + systemd enable/start。
	prepared, _, _ := local.snapshot()
	if len(prepared) != 1 || prepared[0] != constants.EtcdDataDir {
		t.Fatalf("prepared = %v", prepared)
	}
	if len(svc.actions) == 0 {
		t.Fatal("unit 模式必须调用 systemd")
	}
	data, err := os.ReadFile(paths.Resolve("/etc/systemd/system/" + etcdapi.ServiceName))
	if err != nil {
		t.Fatalf("清单未写入：%v", err)
	}
	if !strings.Contains(string(data), "--initial-cluster-state=new") {
		t.Fatalf("清单内容不正确：%s", data)
	}
	if statusCalls < 3 {
		t.Fatalf("应当轮询等待健康，statusCalls = %d", statusCalls)
	}
	// 幂等：再跑一次不报错，内容不变。
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("第二次 Run（幂等）失败：%v", err)
	}
	again, err := os.ReadFile(paths.Resolve("/etc/systemd/system/" + etcdapi.ServiceName))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Fatal("重复执行不应改变清单内容")
	}
}

func TestInitRunBinaryModeUsesSystemd(t *testing.T) {
	cluster, hosts := testCluster(false, "m1")
	clock := newTestClock()
	api := &fakeAPI{}
	api.statusFn = func(context.Context, string) (etcdapi.Status, error) {
		return etcdapi.Status{Healthy: true, MemberID: 1}, nil
	}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return []etcdapi.Member{memberHealthy(1, "m1")}, nil
	}
	local := newFakeLocal()
	svc := &fakeServices{}
	paths := constants.NewPaths(t.TempDir())
	spec := unitSpec(paths, "m1", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m1", "m1=https://10.0.0.1:2380")
	pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
	pc.Paths = paths
	p := testPhase(ModeInit, api, local, svc, clock, hook)
	// unit 模式必须先安装 etcd 宿主二进制；测试里注入假实现（temp root 没有物料）。
	var installed []string
	p.InstallBinary = func(_ context.Context, _ *phase.Context, component string) error {
		installed = append(installed, component)
		return nil
	}

	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(installed) != 1 || installed[0] != "etcd" {
		t.Fatalf("unit 模式必须先安装 etcd 二进制，实际 %v", installed)
	}
	got := strings.Join(svc.recorded(), ",")
	if got != "enable:"+etcdapi.ServiceName+",start:"+etcdapi.ServiceName {
		t.Fatalf("systemd 动作 = %q", got)
	}
	if svc.reloads != 1 {
		t.Fatalf("daemon-reload 次数 = %d, want 1", svc.reloads)
	}
	if _, err := os.Stat(paths.Resolve("/etc/systemd/system/" + etcdapi.ServiceName)); err != nil {
		t.Fatalf("unit 未写入：%v", err)
	}
}

func TestInitRunDryRunTouchesNothing(t *testing.T) {
	cluster, hosts := testCluster(false, "m1")
	clock := newTestClock()
	local := newFakeLocal()
	paths := constants.NewPaths(t.TempDir())
	spec := unitSpec(paths, "m1", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m1", "m1=https://10.0.0.1:2380")
	pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
	pc.Paths = paths
	pc.DryRun = true
	p := testPhase(ModeInit, &fakeAPI{}, local, &fakeServices{}, clock, hook)
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	prepared, renames, removes := local.snapshot()
	if len(prepared) != 0 || len(renames) != 0 || len(removes) != 0 {
		t.Fatalf("dry-run 不得动本机：%v %v %v", prepared, renames, removes)
	}
	if _, err := os.Stat(paths.Resolve("/etc/systemd/system/" + etcdapi.ServiceName)); !os.IsNotExist(err) {
		t.Fatal("dry-run 不得写清单")
	}
}

func TestInitRunRefusesWhenNodeDoesNotRunEtcd(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "w1")
	clock := newTestClock()
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	p := testPhase(ModeInit, &fakeAPI{}, newFakeLocal(), &fakeServices{}, clock, nil)
	err := p.Run(context.Background(), pc)
	if err == nil || !strings.Contains(err.Error(), "不运行 etcd 成员") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckReportsCompletion(t *testing.T) {
	cluster, hosts := testCluster(false, "m1")
	clock := newTestClock()
	paths := constants.NewPaths(t.TempDir())
	spec := unitSpec(paths, "m1", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m1", "m1=https://10.0.0.1:2380")
	dbPath := constants.EtcdDataDir + "/member/snap/db"

	newAPI := func(members []etcdapi.Member, healthy bool) *fakeAPI {
		api := &fakeAPI{}
		api.membersFn = func(context.Context) ([]etcdapi.Member, error) { return members, nil }
		api.statusFn = func(ctx context.Context, ep string) (etcdapi.Status, error) {
			return etcdapi.Status{Endpoint: ep, Healthy: healthy, MemberID: 1}, nil
		}
		return api
	}

	t.Run("数据目录未初始化且清单缺失时未完成", func(t *testing.T) {
		pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
		pc.Paths = paths
		p := testPhase(ModeInit, newAPI([]etcdapi.Member{memberHealthy(1, "m1")}, true), newFakeLocal(), &fakeServices{}, clock, hook)
		done, err := p.Check(context.Background(), pc)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			t.Fatal("清单还没写，Check 必须为 false")
		}
	})

	t.Run("数据目录未初始化但清单与期望一致时算完成", func(t *testing.T) {
		local := newFakeLocal()
		pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
		pc.Paths = paths
		if err := os.MkdirAll(paths.Resolve("/etc/systemd/system"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.Resolve("/etc/systemd/system/"+etcdapi.ServiceName), spec.Files[0].Content, 0o644); err != nil {
			t.Fatal(err)
		}
		p := testPhase(ModeInit, newAPI([]etcdapi.Member{memberHealthy(1, "m1")}, true), local, &fakeServices{}, clock, hook)
		done, err := p.Check(context.Background(), pc)
		if err != nil || !done {
			t.Fatalf("done = %v, err = %v", done, err)
		}
	})

	t.Run("数据目录已初始化时只看成员与健康", func(t *testing.T) {
		local := newFakeLocal(dbPath)
		pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
		pc.Paths = paths
		p := testPhase(ModeInit, newAPI([]etcdapi.Member{memberHealthy(1, "m1")}, true), local, &fakeServices{}, clock, hook)
		done, err := p.Check(context.Background(), pc)
		if err != nil || !done {
			t.Fatalf("done = %v, err = %v", done, err)
		}
	})

	t.Run("成员不在集群里时未完成", func(t *testing.T) {
		local := newFakeLocal(dbPath)
		pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
		pc.Paths = paths
		p := testPhase(ModeInit, newAPI([]etcdapi.Member{memberHealthy(9, "other")}, true), local, &fakeServices{}, clock, hook)
		done, err := p.Check(context.Background(), pc)
		if err != nil || done {
			t.Fatalf("done = %v, err = %v", done, err)
		}
	})

	t.Run("本机 endpoint 不健康时未完成", func(t *testing.T) {
		local := newFakeLocal(dbPath)
		pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
		pc.Paths = paths
		p := testPhase(ModeInit, newAPI([]etcdapi.Member{memberHealthy(1, "m1")}, false), local, &fakeServices{}, clock, hook)
		done, err := p.Check(context.Background(), pc)
		if err != nil || done {
			t.Fatalf("done = %v, err = %v", done, err)
		}
	})

	t.Run("learner 在 join 模式下未完成", func(t *testing.T) {
		joinCluster, joinHosts := testCluster(false, "m1", "m2")
		local := newFakeLocal(dbPath)
		pc, _ := newPhaseContext(t, joinCluster, &joinHosts[1], clock)
		pc.Paths = paths
		learner := memberHealthy(2, "m2")
		learner.IsLearner = true
		p := testPhase(ModeJoin, newAPI([]etcdapi.Member{memberHealthy(1, "m1"), learner}, true), local, &fakeServices{}, clock, hook)
		done, err := p.Check(context.Background(), pc)
		if err != nil || done {
			t.Fatalf("done = %v, err = %v", done, err)
		}
	})
}

func TestCheckJoinComparesInitialClusterStateBeforeFirstStart(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	paths := constants.NewPaths(t.TempDir())
	// 本机是 m2，已是投票成员但没有数据目录：Check 必须比对 initial-cluster-state。
	spec := unitSpec(paths, "m2", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m2", "m1=https://10.0.0.1:2380")
	if err := os.MkdirAll(paths.Manifests(), 0o700); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return []etcdapi.Member{memberHealthy(1, "m1"), memberHealthy(2, "m2")}, nil
	}
	api.statusFn = func(ctx context.Context, ep string) (etcdapi.Status, error) {
		return etcdapi.Status{Endpoint: ep, Healthy: true, MemberID: 2, RaftIndex: 10}, nil
	}
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	pc.Paths = paths
	local := newFakeLocal()
	p := testPhase(ModeJoin, api, local, &fakeServices{}, clock, hook)

	if err := os.MkdirAll(paths.Resolve("/etc/systemd/system"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Resolve("/etc/systemd/system/"+etcdapi.ServiceName), spec.Files[0].Content, 0o644); err != nil {
		t.Fatal(err)
	}
	done, err := p.Check(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("state=new 的加入者不算完成")
	}
	// 写入正确的 patched 内容后应为完成（与 expectedArtifact 同口径：
	// 真实成员列表 + state=existing）。
	patched := unitSpec(paths, "m2", "existing", "m1=https://10.0.0.1:2380", "m2=https://10.0.0.2:2380")
	if err := os.WriteFile(paths.Resolve("/etc/systemd/system/"+etcdapi.ServiceName), patched.Files[0].Content, 0o644); err != nil {
		t.Fatal(err)
	}
	done, err = p.Check(context.Background(), pc)
	if err != nil || !done {
		t.Fatalf("done = %v, err = %v", done, err)
	}
}

// TestEtcdArtifactMatches：数据目录初始化后只忽略 initial-cluster* 两行，
// 其它 flag（quota/extraArgs）漂移必须被 Check 发现。
func TestEtcdArtifactMatches(t *testing.T) {
	live := []byte("--name=m1\n--initial-cluster=m1=https://10.0.0.1:2380\n--initial-cluster-state=new\n--quota-backend-bytes=2147483648\n")
	same := []byte("--name=m1\n--initial-cluster=m1=https://10.0.0.1:2380,m2=https://10.0.0.2:2380\n--initial-cluster-state=existing\n--quota-backend-bytes=2147483648\n")
	if !etcdArtifactMatches(live, same, true) {
		t.Fatal("初始化后 initial-cluster* 变化不应判成漂移")
	}
	changed := []byte("--name=m1\n--initial-cluster=m1=https://10.0.0.1:2380\n--initial-cluster-state=new\n--quota-backend-bytes=1073741824\n")
	if etcdArtifactMatches(live, changed, true) {
		t.Fatal("quota 变化必须判成漂移")
	}
	if etcdArtifactMatches(live, same, false) {
		t.Fatal("未初始化时 initial-cluster 不同必须判成漂移")
	}
}
