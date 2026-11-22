package etcd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"kubemono/internal/bom"
	"kubemono/internal/constants"
	etcdapi "kubemono/internal/etcd"
	"kubemono/internal/phase"
)

func TestJoinAddsLearnerWaitsForCatchUpAndPromotes(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	paths := constants.NewPaths(t.TempDir())
	spec := unitSpec(paths, "m2", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m2", "m1=https://10.0.0.1:2380")

	var members []etcdapi.Member
	members = append(members, memberHealthy(1, "m1"))
	api := &fakeAPI{}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return append([]etcdapi.Member(nil), members...), nil
	}
	api.addFn = func(_ context.Context, name string, peerURLs []string) (uint64, error) {
		if len(peerURLs) != 1 || peerURLs[0] != "https://10.0.0.2:2380" {
			t.Errorf("AddLearner peerURLs = %v", peerURLs)
		}
		members = append(members, etcdapi.Member{ID: 2, PeerURLs: peerURLs, IsLearner: true})
		return 2, nil
	}
	api.promoteFn = func(context.Context, uint64) error {
		for i := range members {
			if members[i].ID == 2 {
				members[i].IsLearner = false
				members[i].Name = "m2"
			}
		}
		return nil
	}
	localRaft := uint64(0)
	api.statusFn = func(_ context.Context, ep string) (etcdapi.Status, error) {
		if strings.Contains(ep, "10.0.0.1") {
			return etcdapi.Status{Endpoint: ep, Healthy: true, MemberID: 1, Leader: 1, RaftIndex: 100}, nil
		}
		localRaft += 40 // 每轮追 40，第三轮追平 / catch up 40 per round; caught up on the third round
		return etcdapi.Status{Endpoint: ep, Healthy: true, MemberID: 2, Leader: 1, RaftIndex: localRaft, IsLearner: true}, nil
	}

	local := newFakeLocal()
	svc := &fakeServices{}
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	pc.Paths = paths
	p := testPhase(ModeJoin, api, local, svc, clock, hook)

	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 清单必须被改写为 existing + 真实成员列表（learners 也算成员）。
	data, err := os.ReadFile(paths.Resolve("/etc/systemd/system/" + etcdapi.ServiceName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--initial-cluster-state=existing") {
		t.Fatalf("清单 state 未改写：%s", data)
	}
	if !strings.Contains(string(data), "--initial-cluster=m1=https://10.0.0.1:2380,m2=https://10.0.0.2:2380") {
		t.Fatalf("清单 initial-cluster 未改写为实际成员：%s", data)
	}
	calls := api.Calls()
	if !containsCall(calls, "AddLearner:m2") || !containsCall(calls, "Promote:2") {
		t.Fatalf("调用序列缺少 add/promote：%v", calls)
	}
	if indexOf(calls, "AddLearner:m2") > indexOf(calls, "Promote:2") {
		t.Fatalf("promote 必须在 add 之后：%v", calls)
	}
	if indexOf(calls, "AddLearner:m2") > indexOfPrefix(calls, "Status:") {
		t.Fatalf("应先 AddLearner 再启动本机等健康：%v", calls)
	}
	// 幂等：成员已是投票成员且健康时 Check 为真，重复 Run 不 add/promote。
	done, err := p.Check(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("join 完成后 Check 必须为 true")
	}
	before := len(api.Calls())
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("重复 Run（幂等）：%v", err)
	}
	for _, call := range api.Calls()[before:] {
		if strings.HasPrefix(call, "AddLearner") || strings.HasPrefix(call, "Promote") {
			t.Fatalf("已经是投票成员时不应再变更成员：%v", api.Calls()[before:])
		}
	}
}

func TestJoinSkipsAddWhenMemberAlreadyVoting(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	paths := constants.NewPaths(t.TempDir())
	spec := unitSpec(paths, "m2", "existing", "m1=https://10.0.0.1:2380", "m2=https://10.0.0.2:2380")
	hook := unitRenderHook(spec, "m2", "m1=https://10.0.0.1:2380", "m2=https://10.0.0.2:2380")
	api := &fakeAPI{}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return []etcdapi.Member{memberHealthy(1, "m1"), memberHealthy(2, "m2")}, nil
	}
	api.statusFn = func(ctx context.Context, ep string) (etcdapi.Status, error) {
		return etcdapi.Status{Endpoint: ep, Healthy: true, MemberID: 2, RaftIndex: 100}, nil
	}
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	pc.Paths = paths
	p := testPhase(ModeJoin, api, newFakeLocal(), &fakeServices{}, clock, hook)
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, call := range api.Calls() {
		if strings.HasPrefix(call, "AddLearner") || strings.HasPrefix(call, "Promote") {
			t.Fatalf("已是投票成员时不应做成员变更：%v", api.Calls())
		}
	}
}

func TestJoinCatchUpTimeoutIsBounded(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	paths := constants.NewPaths(t.TempDir())
	spec := unitSpec(paths, "m2", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m2", "m1=https://10.0.0.1:2380")
	members := []etcdapi.Member{memberHealthy(1, "m1"), {ID: 2, PeerURLs: []string{"https://10.0.0.2:2380"}, IsLearner: true}}
	api := &fakeAPI{}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return append([]etcdapi.Member(nil), members...), nil
	}
	api.statusFn = func(_ context.Context, ep string) (etcdapi.Status, error) {
		if strings.Contains(ep, "10.0.0.1") {
			return etcdapi.Status{Endpoint: ep, Healthy: true, MemberID: 1, Leader: 1, RaftIndex: 1000}, nil
		}
		// 本机永远追不上：promote 也应一直被拒绝。
		return etcdapi.Status{Endpoint: ep, Healthy: true, MemberID: 2, Leader: 1, RaftIndex: 1, IsLearner: true}, nil
	}
	api.promoteFn = func(context.Context, uint64) error {
		return fmt.Errorf("etcdserver: learner not ready")
	}
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	pc.Paths = paths
	p := testPhase(ModeJoin, api, newFakeLocal(), &fakeServices{}, clock, hook)
	p.Timeout = 4
	p.Interval = 1

	err := p.Run(context.Background(), pc)
	if err == nil {
		t.Fatal("追平超时必须返回错误")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("err = %v", err)
	}
	statusCalls := 0
	for _, call := range api.Calls() {
		if strings.HasPrefix(call, "Status:") {
			statusCalls++
		}
	}
	if statusCalls > 20 {
		t.Fatalf("轮询必须有界，status 调用 %d 次", statusCalls)
	}
}

func TestJoinRefusesWhenNodeNotInConfiguration(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	// 构造一个“本机运行 etcd 但不在配置的成员表里”的场景（角色刚被改掉）。
	stub := &stubProvider{
		form:       etcdapi.FormStacked,
		local:      etcdapi.Local{Name: "m9", Member: true, PeerURLs: []string{"https://10.0.0.9:2380"}, ClientURL: "https://10.0.0.9:2379", DataDir: constants.EtcdDataDir, ServiceName: etcdapi.ServiceName},
		names:      []string{"m1", "m2"},
		configured: []string{"m1=https://10.0.0.1:2380", "m2=https://10.0.0.2:2380"},
	}
	pc, _ := newPhaseContext(t, cluster, &hosts[0], clock)
	p := testPhase(ModeJoin, &fakeAPI{}, newFakeLocal(), &fakeServices{}, clock, nil)
	p.ProviderFactory = func(*phase.Context) (etcdapi.Provider, error) { return stub, nil }
	err := p.Run(context.Background(), pc)
	if err == nil || !strings.Contains(err.Error(), "不在集群配置") {
		t.Fatalf("err = %v, want 拒绝非配置成员", err)
	}
}

func TestJoinDryRunDoesNotTouchHost(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	paths := constants.NewPaths(t.TempDir())
	spec := unitSpec(paths, "m2", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m2", "m1=https://10.0.0.1:2380")
	members := []etcdapi.Member{memberHealthy(1, "m1")}
	api := &fakeAPI{}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return append([]etcdapi.Member(nil), members...), nil
	}
	api.addFn = func(_ context.Context, _ string, peerURLs []string) (uint64, error) {
		members = append(members, etcdapi.Member{ID: 2, PeerURLs: peerURLs, IsLearner: true})
		return 2, nil
	}
	local := newFakeLocal()
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	pc.Paths = paths
	pc.DryRun = true
	p := testPhase(ModeJoin, api, local, &fakeServices{}, clock, hook)
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if _, err := os.Stat(paths.Resolve("/etc/systemd/system/" + etcdapi.ServiceName)); !os.IsNotExist(err) {
		t.Fatal("dry-run 不得写清单")
	}
	for _, call := range api.Calls() {
		if strings.HasPrefix(call, "AddLearner:") || strings.HasPrefix(call, "Promote:") {
			t.Fatalf("dry-run 不得改变成员列表，实际调用 %v", api.Calls())
		}
	}
	prepared, renames, removes := local.snapshot()
	if len(prepared)+len(renames)+len(removes) != 0 {
		t.Fatalf("dry-run 不得动本机：%v %v %v", prepared, renames, removes)
	}
}

// TestUnitRenderHookConsumesStructuredFlags 覆盖注入渲染函数的契约：
// join 首启只通过 rc.EtcdInitialCluster / EtcdInitialClusterState 两个
// 结构化字段影响渲染内容——测试与生产都没有"渲染后改写文本"的路径。
func TestUnitRenderHookConsumesStructuredFlags(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	paths := constants.NewPaths(t.TempDir())
	base := unitSpec(paths, "m2", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(base, "m2", "m1=https://10.0.0.1:2380")
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	pc.Paths = paths

	// 没有覆盖：返回基准内容。
	spec, err := hook(pc.RenderContext())
	if err != nil {
		t.Fatal(err)
	}
	if string(spec.Files[0].Content) != string(base.Files[0].Content) {
		t.Fatal("无覆盖时应返回基准内容")
	}
	// 覆盖后：initial-cluster 与 state 全部来自结构化字段。
	rc := pc.RenderContext()
	rc.EtcdInitialCluster = []string{"m1=https://10.0.0.1:2380", "m2=https://10.0.0.2:2380"}
	rc.EtcdInitialClusterState = "existing"
	spec, err = hook(rc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(spec.Files[0].Content)
	if !strings.Contains(s, "--initial-cluster=m1=https://10.0.0.1:2380,m2=https://10.0.0.2:2380") {
		t.Fatalf("initial-cluster 未反映结构化字段：%s", s)
	}
	if !strings.Contains(s, "--initial-cluster-state=existing") {
		t.Fatalf("state 未反映结构化字段：%s", s)
	}
}

// stubProvider 用于构造真实 Provider 不会出现的边界场景。
type stubProvider struct {
	form       etcdapi.Form
	local      etcdapi.Local
	names      []string
	configured []string
}

func (s *stubProvider) Form() etcdapi.Form        { return s.form }
func (s *stubProvider) Local() etcdapi.Local      { return s.local }
func (s *stubProvider) ConfiguredNames() []string { return s.names }
func (s *stubProvider) MemberConfig() (etcdapi.Config, error) {
	return etcdapi.Config{Endpoints: []string{"https://10.0.0.1:2379"}}, nil
}
func (s *stubProvider) ClientConfig() (etcdapi.Config, error) { return s.MemberConfig() }

func containsCall(calls []string, want string) bool {
	return indexOf(calls, want) >= 0
}

func indexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

func indexOfPrefix(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestJoinAfterBootstrapKeepsRenderedFile(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	paths := constants.NewPaths(t.TempDir())
	// 渲染结果仍写着 state=new（本仓库里 install 之前没人设置 EtcdInitialClusterState），
	// 但数据目录已经初始化：join 不能再改写 --initial-cluster*。
	spec := unitSpec(paths, "m2", "new", "m1=https://10.0.0.1:2380")
	hook := unitRenderHook(spec, "m2", "m1=https://10.0.0.1:2380")
	api := &fakeAPI{}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return []etcdapi.Member{memberHealthy(1, "m1"), memberHealthy(2, "m2")}, nil
	}
	api.statusFn = func(ctx context.Context, ep string) (etcdapi.Status, error) {
		return etcdapi.Status{Endpoint: ep, Healthy: true, MemberID: 2, RaftIndex: 50}, nil
	}
	local := newFakeLocal(constants.EtcdDataDir + "/member/snap/db")
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	pc.Paths = paths
	p := testPhase(ModeJoin, api, local, &fakeServices{}, clock, hook)
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(paths.Resolve("/etc/systemd/system/" + etcdapi.ServiceName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "--initial-cluster-state=existing") {
		t.Fatalf("数据目录已初始化时不应改写清单（会与 control-plane phase 争抢）：%s", data)
	}
	for _, call := range api.Calls() {
		if strings.HasPrefix(call, "AddLearner") || strings.HasPrefix(call, "Promote") {
			t.Fatalf("已是投票成员时不应做成员变更：%v", api.Calls())
		}
	}
	// Check 也应为完成（成员存在 + 健康 + 已 promote）。
	done, err := p.Check(context.Background(), pc)
	if err != nil || !done {
		t.Fatalf("done = %v, err = %v", done, err)
	}
}

// TestExpectedArtifactRendersStructuredJoinFlags 覆盖生产路径：join 首启不是
// “渲染后正则改写”，而是把真实成员列表与 existing 状态作为结构化 flags 渲染。
func TestExpectedArtifactRendersStructuredJoinFlags(t *testing.T) {
	cluster, hosts := testCluster(false, "m1", "m2")
	clock := newTestClock()
	pc, _ := newPhaseContext(t, cluster, &hosts[1], clock)
	pc.BOM = bom.MustLoad()
	api := &fakeAPI{}
	api.membersFn = func(context.Context) ([]etcdapi.Member, error) {
		return []etcdapi.Member{memberHealthy(1, "m1"), memberHealthy(2, "m2")}, nil
	}
	local := newFakeLocal()
	p := testPhase(ModeJoin, api, local, &fakeServices{}, clock, nil)
	prov, err := p.provider(pc)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	_, content, err := p.expectedArtifact(context.Background(), pc, prov, api)
	if err != nil {
		t.Fatalf("expectedArtifact: %v", err)
	}
	s := string(content)
	for _, want := range []string{
		"--initial-cluster=m1=https://10.0.0.1:2380,m2=https://10.0.0.2:2380",
		"--initial-cluster-state=existing",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("expectedArtifact 缺少 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--initial-cluster-state=new") {
		t.Fatalf("join 首启不能再用 new:\n%s", s)
	}
}
