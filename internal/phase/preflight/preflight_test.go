package preflight

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/hostinit"
	"kubemono/internal/phase"
	"kubemono/internal/util/output"
)

// healthyFacts 描述一个通过所有检查的节点。
func healthyFacts() *hostinit.Facts {
	return &hostinit.Facts{
		OSName:           "Rocky Linux",
		OSFamily:         "rhel",
		OSVersion:        "9.4",
		KernelVersion:    "5.14.0-427.el9.x86_64",
		KernelMajorMinor: "5.14",
		CPUs:             8,
		MemoryBytes:      16 << 30,
		CgroupVersion:    "v2",
		SELinuxMode:      "permissive",
		FirewallBackend:  "none",
	}
}

type fakePorts struct{}

func (fakePorts) Free(context.Context, string, int) (bool, error) { return true, nil }

type fakeCommands struct{ missing map[string]bool }

func (f fakeCommands) LookPath(name string) (string, error) {
	if f.missing[name] {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + name, nil
}

func testContext(t *testing.T) *phase.Context {
	t.Helper()
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost("m1")
	return &phase.Context{
		Cluster: cluster,
		Host:    host,
		BOM:     bom.MustLoad(),
		Paths:   constants.NewPaths(t.TempDir()),
		Release: bom.MustLoad().Release,
		Report:  output.Discard(),
		Now:     time.Now,
	}
}

func healthyPhase(facts *hostinit.Facts) *Phase {
	return &Phase{
		Facts:         facts,
		PortChecker:   fakePorts{},
		CommandLookup: fakeCommands{},
	}
}

func TestHealthyNodePasses(t *testing.T) {
	pc := testContext(t)
	if err := healthyPhase(healthyFacts()).Run(context.Background(), pc); err != nil {
		t.Fatalf("健康节点不应失败: %v", err)
	}
}

func TestCheckAlwaysRunsAgain(t *testing.T) {
	pc := testContext(t)
	done, err := healthyPhase(healthyFacts()).Check(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("preflight 是闸门：每次都必须重新执行")
	}
}

func TestFailuresAreReportedWithRemediation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*hostinit.Facts, *Phase)
		want   string
	}{
		{
			name:   "cgroup v1 且没有 cgroup 挂载",
			mutate: func(f *hostinit.Facts, _ *Phase) { f.CgroupVersion = "unknown" },
			want:   "cgroup",
		},
		{
			name: "缺少必需命令",
			mutate: func(_ *hostinit.Facts, p *Phase) {
				p.CommandLookup = fakeCommands{missing: map[string]bool{"systemctl": true}}
			},
			want: "commands",
		},
		{
			name:   "资源不足",
			mutate: func(f *hostinit.Facts, _ *Phase) { f.CPUs = 1; f.MemoryBytes = 256 << 20 },
			want:   "resources",
		},
		{
			name:   "内核过旧",
			mutate: func(f *hostinit.Facts, _ *Phase) { f.KernelVersion = "4.18.0"; f.KernelMajorMinor = "4.18" },
			want:   "kernel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := testContext(t)
			facts := healthyFacts()
			p := healthyPhase(facts)
			tc.mutate(facts, p)
			err := p.Run(context.Background(), pc)
			if err == nil {
				t.Fatalf("期望预检失败（%s）", tc.want)
			}
			if !strings.Contains(err.Error(), "预检失败") {
				t.Fatalf("错误信息应说明预检失败: %v", err)
			}
		})
	}
}

// TestContainerizedIsOnlyAWarning: 容器化会让安装变得不可靠，但不是硬失败——
// 有些团队确实在特权容器里跑控制面（例如 CI 或嵌套虚拟化环境）。
func TestContainerizedIsOnlyAWarning(t *testing.T) {
	pc := testContext(t)
	facts := healthyFacts()
	facts.Containerized = true
	if err := healthyPhase(facts).Run(context.Background(), pc); err != nil {
		t.Fatalf("容器化应只是警告: %v", err)
	}
}

func TestPortBusyFails(t *testing.T) {
	pc := testContext(t)
	p := healthyPhase(healthyFacts())
	p.PortChecker = busyPorts{}
	if err := p.Run(context.Background(), pc); err == nil {
		t.Fatal("端口被占用时必须失败")
	}
}

type busyPorts struct{}

func (busyPorts) Free(_ context.Context, protocol string, port int) (bool, error) {
	if port == constants.APIServerPort {
		return false, nil
	}
	return true, nil
}

func TestFactsCollectionUsesCollector(t *testing.T) {
	pc := testContext(t)
	p := healthyPhase(healthyFacts())
	// 注入 Facts 时不调用 Collect；这里只是确认接口字段被尊重。
	if p.Facts == nil {
		t.Fatal("Facts 应可注入")
	}
	if _, err := p.collect(context.Background(), pc); err != nil {
		t.Fatalf("collect: %v", err)
	}
	// 没有注入时使用真实采集器（darwin 上部分信息缺失，但不应报错）。
	p2 := healthyPhase(nil)
	if _, err := p2.collect(context.Background(), pc); err != nil {
		t.Fatalf("真实采集不应报错: %v", err)
	}
}

func TestDataDirsIncludeRuntimeAndEtcd(t *testing.T) {
	pc := testContext(t)
	dirs := dataDirs(pc)
	want := []string{pc.Paths.EtcdData(), pc.Paths.Kubelet(), pc.Paths.Resolve("/var/lib/containerd")}
	for _, w := range want {
		found := false
		for _, d := range dirs {
			if d == w {
				found = true
			}
		}
		if !found {
			t.Errorf("dataDirs 缺少 %s（现有 %v）", w, dirs)
		}
	}
}

func TestDryRunDoesNotChangeHost(t *testing.T) {
	pc := testContext(t)
	pc.DryRun = true
	before, _ := os.ReadDir(pc.Paths.Kubemono())
	if err := healthyPhase(healthyFacts()).Run(context.Background(), pc); err != nil {
		t.Fatalf("dry-run 预检不应失败: %v", err)
	}
	after, _ := os.ReadDir(pc.Paths.Kubemono())
	if len(before) != len(after) {
		t.Fatal("dry-run 不应写任何文件")
	}
}

func TestExternalEtcdSkipsManagedEtcdPorts(t *testing.T) {
	pc := testContext(t)
	pc.Cluster.ControlPlane.Etcd.External = clusterconfig.ExternalEtcdSpec{Endpoints: []string{"https://127.0.0.1:2379"}}
	pc.Cluster.ControlPlane.Etcd.External = clusterconfig.ExternalEtcdSpec{
		Endpoints: []string{"https://10.0.0.31:2379"},
		CAFile:    "/tmp/ca.crt",
		CertFile:  "/tmp/client.crt",
		KeyFile:   "/tmp/client.key",
	}
	opts := healthyPhase(healthyFacts()).options(pc, healthyFacts())
	for _, p := range opts.Ports {
		switch p.Port {
		case constants.EtcdClientPort, constants.EtcdPeerPort, constants.EtcdMetricsPort:
			t.Fatalf("外部 etcd 不应检查受管 etcd 端口 %d", p.Port)
		}
	}
	// 同机外部 etcd 占用 2379 也必须能通过预检。
	p := healthyPhase(healthyFacts())
	p.PortChecker = etcdBusyPorts{}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("外部 etcd 占用 2379 不应导致预检失败: %v", err)
	}
}

type etcdBusyPorts struct{}

func (etcdBusyPorts) Free(_ context.Context, _ string, port int) (bool, error) {
	return port != constants.EtcdClientPort, nil
}
