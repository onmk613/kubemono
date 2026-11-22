package etcd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	etcdapi "kubemono/internal/etcd"
	"kubemono/internal/phase"
	"kubemono/internal/render"
)

// fakeAPI 是 etcdapi.API 的可编程 fake：只实现测试用到的 RPC，
// 未实现的方法会因嵌入 nil 接口而 panic（这正是我们想要的：测试不应意外依赖它）。
type fakeAPI struct {
	etcdapi.API

	mu    sync.Mutex
	calls []string

	membersFn  func(context.Context) ([]etcdapi.Member, error)
	statusFn   func(context.Context, string) (etcdapi.Status, error)
	addFn      func(context.Context, string, []string) (uint64, error)
	promoteFn  func(context.Context, uint64) error
	removeFn   func(context.Context, uint64) error
	canRemove  func(context.Context, uint64) error
	canRemoveA func(context.Context) error

	closed bool
}

func (f *fakeAPI) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// Calls 返回已记录的调用序列。
func (f *fakeAPI) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAPI) Health(context.Context) ([]etcdapi.MemberHealth, error) { return nil, nil }

func (f *fakeAPI) Members(ctx context.Context) ([]etcdapi.Member, error) {
	f.record("Members")
	if f.membersFn != nil {
		return f.membersFn(ctx)
	}
	return nil, nil
}

func (f *fakeAPI) AddLearner(ctx context.Context, name string, peerURLs []string) (uint64, error) {
	f.record("AddLearner:" + name)
	if f.addFn != nil {
		return f.addFn(ctx, name, peerURLs)
	}
	return 0, nil
}

func (f *fakeAPI) Promote(ctx context.Context, id uint64) error {
	f.record(fmt.Sprintf("Promote:%d", id))
	if f.promoteFn != nil {
		return f.promoteFn(ctx, id)
	}
	return nil
}

func (f *fakeAPI) RemoveMember(ctx context.Context, id uint64) error {
	f.record(fmt.Sprintf("RemoveMember:%d", id))
	if f.removeFn != nil {
		return f.removeFn(ctx, id)
	}
	return nil
}

func (f *fakeAPI) Status(ctx context.Context, endpoint string) (etcdapi.Status, error) {
	f.record("Status:" + endpoint)
	if f.statusFn != nil {
		return f.statusFn(ctx, endpoint)
	}
	return etcdapi.Status{Endpoint: endpoint, Healthy: true}, nil
}

func (f *fakeAPI) CanRemove(ctx context.Context) error {
	f.record("CanRemove")
	if f.canRemoveA != nil {
		return f.canRemoveA(ctx)
	}
	return nil
}

func (f *fakeAPI) CanRemoveMember(ctx context.Context, id uint64) error {
	f.record(fmt.Sprintf("CanRemoveMember:%d", id))
	if f.canRemove != nil {
		return f.canRemove(ctx, id)
	}
	return nil
}

func (f *fakeAPI) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeLocal 是内存版的本机文件操作。
type fakeLocal struct {
	mu       sync.Mutex
	paths    map[string]bool
	prepared []string
	renames  [][2]string
	removes  []string
}

func newFakeLocal(paths ...string) *fakeLocal {
	f := &fakeLocal{paths: map[string]bool{}}
	for _, p := range paths {
		f.paths[p] = true
	}
	return f
}

func (f *fakeLocal) PrepareDir(path string, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, path)
	f.paths[path] = true
	return nil
}

func (f *fakeLocal) Rename(oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renames = append(f.renames, [2]string{oldPath, newPath})
	delete(f.paths, oldPath)
	f.paths[newPath] = true
	return nil
}

func (f *fakeLocal) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, path)
	delete(f.paths, path)
	return nil
}

func (f *fakeLocal) Exists(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[path]
}

func (f *fakeLocal) snapshot() (prepared []string, renames [][2]string, removes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prepared...), append([][2]string(nil), f.renames...), append([]string(nil), f.removes...)
}

// fakeServices 记录 systemd 动作。
type fakeServices struct {
	mu      sync.Mutex
	actions []string
	active  bool
	reloads int
	err     error
}

func (f *fakeServices) Action(_ context.Context, unit, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action+":"+unit)
	if f.err != nil {
		return f.err
	}
	switch action {
	case "start":
		f.active = true
	case "stop":
		f.active = false
	}
	return nil
}

func (f *fakeServices) DaemonReload(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	return f.err
}

func (f *fakeServices) IsActive(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, f.err
}

func (f *fakeServices) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

// testClock 是可注入的假时钟：Sleep 会推进它，因此轮询测试不会真的等待。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2024, 11, 1, 2, 3, 4, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testCluster 构造一个 stacked 或 external 的集群配置。
func testCluster(external bool, hostNames ...string) (*clusterconfig.Cluster, []clusterconfig.Host) {
	var hosts []clusterconfig.Host
	for i, name := range hostNames {
		role := clusterconfig.RoleMaster
		if strings.HasPrefix(name, "w") {
			role = clusterconfig.RoleWorker
		}
		hosts = append(hosts, clusterconfig.Host{
			Name:    name,
			Address: fmt.Sprintf("10.0.0.%d", i+1),
			Roles:   []clusterconfig.Role{role},
		})
	}
	cluster := &clusterconfig.Cluster{
		Cluster: clusterconfig.ClusterSpec{
			Hosts: hosts,
		},
		ControlPlane: clusterconfig.ControlPlaneSpec{
			Etcd: func() clusterconfig.EtcdSpec {
				spec := clusterconfig.EtcdSpec{Local: clusterconfig.LocalEtcdSpec{DataDir: constants.EtcdDataDir}}
				if external {
					spec.External = clusterconfig.ExternalEtcdSpec{Endpoints: []string{"https://etcd.example:2379"}}
				}
				return spec
			}(),
		},
	}
	return cluster, hosts
}

// newPhaseContext 组装一个最小可用的 phase.Context（临时目录作为路径根）。
func newPhaseContext(t *testing.T, cluster *clusterconfig.Cluster, host *clusterconfig.Host, clock *testClock) (*phase.Context, constants.Paths) {
	t.Helper()
	paths := constants.NewPaths(t.TempDir())
	pc := &phase.Context{
		Cluster: cluster,
		Host:    host,
		Paths:   paths,
		Release: "v1.31.4-km.1",
	}
	if clock != nil {
		pc.Now = clock.Now
	}
	if cluster != nil && host != nil {
		pc.IsInitNode = host.Name == cluster.Cluster.Hosts[0].Name
	}
	return pc, paths
}

// unitSpec 构造一个含 etcd.service 的渲染结果（binary 模式的基准内容）。
func unitSpec(paths constants.Paths, name string, state string, members ...string) *render.Spec {
	unit := fmt.Sprintf(`[Unit]
Description=kubemono managed etcd

[Service]
Type=simple
ExecStart=/usr/local/bin/etcd --name=%s --initial-cluster=%s --initial-cluster-state=%s --data-dir=/var/lib/etcd
Restart=always
`, name, strings.Join(members, ","), state)
	spec := &render.Spec{Node: name}
	spec.AddFile(paths.Resolve("/etc/systemd/system/"+etcdapi.ServiceName), []byte(unit), 0o644,
		constants.ComponentEtcd, render.KindUnit, render.ActionRestart)
	return spec
}

// unitRenderHook 构造一个注入的渲染函数：像生产 render 一样只消费
// rc.EtcdInitialCluster / rc.EtcdInitialClusterState 两个结构化字段，
// 为空时回退到 base 的基准内容。测试注入它以避免文本改写路径复活。
func unitRenderHook(base *render.Spec, name string, members ...string) func(*render.Context) (*render.Spec, error) {
	return func(rc *render.Context) (*render.Spec, error) {
		if rc == nil || (len(rc.EtcdInitialCluster) == 0 && rc.EtcdInitialClusterState == "") {
			return base, nil
		}
		list := rc.EtcdInitialCluster
		if len(list) == 0 {
			list = members
		}
		state := rc.EtcdInitialClusterState
		if state == "" {
			state = "new"
		}
		return unitSpec(rc.Paths, name, state, list...), nil
	}
}

// testPhase 构造一个使用注入 fake 的 phase（默认 ModeAuto）。
func testPhase(mode Mode, api *fakeAPI, local *fakeLocal, svc *fakeServices, clock *testClock, render func(*render.Context) (*render.Spec, error)) *Phase {
	return &Phase{
		Mode:          mode,
		InstallBinary: func(context.Context, *phase.Context, string) error { return nil },
		ClientFactory: func(*phase.Context) (etcdapi.API, error) { return api, nil },
		Local:         local,
		Services:      svc,
		WaitKnobs: phase.WaitKnobs{
			Timeout:  10 * time.Second,
			Interval: time.Second,
			Sleep:    clock.Sleep,
			Clock:    clock.Now,
		},
		Render: render,
	}
}

// memberHealthy 返回一个健康的成员。
func memberHealthy(id uint64, name string) etcdapi.Member {
	return etcdapi.Member{
		ID:       id,
		Name:     name,
		PeerURLs: []string{fmt.Sprintf("https://10.0.0.%d:2380", id)},
	}
}
