package loadbalancer_test

import (
	"strings"
	"testing"

	"kubemono/internal/constants"
	"kubemono/internal/loadbalancer"
	"kubemono/internal/render"
)

// TestLocalProxyRendersBackends 断言后端列表来自配置中的 master 列表。
func TestLocalProxyRendersBackends(t *testing.T) {
	cluster := testCluster(t, clusterYAML())
	c := testContext(t, cluster, "w1", t.TempDir())
	plan, err := loadbalancer.Build(c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cfg := fileOf(t, specOf(plan), c.Paths.Resolve("/etc/haproxy/haproxy.cfg"))
	containsAll(t, "haproxy.cfg", cfg,
		"bind 127.0.0.1:6443",
		"server m1 10.0.0.1:6443 check",
		"server m2 10.0.0.2:6443 check",
		"server m3 10.0.0.3:6443 check",
		"option httpchk",
		"http-check expect status 200",
		"check-ssl verify none",
		"stats uri /stats",
		// haproxy 3.3+ 要求 frontend 与 backend 不能同名（3.4.7 实测会拒绝启动）。
		"frontend kube-apiserver",
		"default_backend kube-apiserver-backend",
		"backend kube-apiserver-backend",
	)
	// 端口与后端来自配置，而不是硬编码。
	lp, err := loadbalancer.NewLocalProxy(c)
	if err != nil {
		t.Fatalf("NewLocalProxy: %v", err)
	}
	backends := lp.Backends(c)
	if backends[0].Name != "m1" || backends[0].Address != "10.0.0.1" || backends[0].Port != constants.APIServerPort {
		t.Errorf("后端列表不对：%+v", backends)
	}
	// 静态 Pod 清单必须与配置同时存在。
	manifest := fileOf(t, specOf(plan), c.Paths.ManifestPath(constants.ComponentLocalProxy))
	containsAll(t, "haproxy 清单", manifest, "hostNetwork: true", "127.0.0.1:6443", "/etc/haproxy")
}

// TestLocalProxyBinaryModeUsesStaticPod 断言 binary 模式也统一使用静态 Pod。
func TestLocalProxyBinaryModeUsesStaticPod(t *testing.T) {
	cluster := testCluster(t, clusterYAML())
	c := testContext(t, cluster, "w1", t.TempDir())
	plan, err := loadbalancer.Build(c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := specOf(plan).Find(c.Paths.Resolve("/etc/systemd/system/haproxy.service")); ok {
		t.Error("binary 模式不应再渲染 haproxy.service")
	}
	manifest := fileOf(t, specOf(plan), c.Paths.ManifestPath(constants.ComponentLocalProxy))
	containsAll(t, "haproxy 清单", manifest, "hostNetwork: true", "/etc/haproxy", "127.0.0.1:6443")
}

// TestLocalProxyOnlyOnWorkers 断言 master 直连本机 apiserver。
func TestLocalProxyOnlyOnWorkers(t *testing.T) {
	cluster := testCluster(t, clusterYAML())
	c := testContext(t, cluster, "m1", t.TempDir())
	plan, err := loadbalancer.Build(c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(specOf(plan).Files) != 0 {
		t.Errorf("master（未启用 kube-vip）不应渲染任何文件：%v", pathsOf(specOf(plan)))
	}
}

// TestBackendChangeRebuildsManifest 断言 master 增删会同时改 haproxy.cfg 和
// 静态 Pod 清单：haproxy 只在进程启动时读配置，清单不变 kubelet 就不会重建 pod。
func TestBackendChangeRebuildsManifest(t *testing.T) {
	root := t.TempDir()
	cluster := testCluster(t, clusterYAML())
	first, err := loadbalancer.Build(testContext(t, cluster, "w1", root))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// 加一台 master：haproxy.cfg 与静态 Pod 清单都应变化。
	grown := addMaster(t, cluster, "m4", "10.0.0.4")
	c := testContext(t, grown, "w1", root)
	second, err := loadbalancer.Build(c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	changes := render.ComputeChanges(specOf(first), specOf(second))
	if len(changes) != 2 {
		t.Fatalf("后端变化应当同时改配置和清单，实际 %d：%v", len(changes), describeChanges(changes))
	}
	configPath := c.Paths.Resolve("/etc/haproxy/haproxy.cfg")
	manifestPath := c.Paths.ManifestPath("local-proxy")
	gotPaths := map[string]render.ChangeKind{}
	for _, ch := range changes {
		gotPaths[ch.File.Path] = ch.Kind
	}
	if gotPaths[configPath] != render.ChangeModified {
		t.Errorf("haproxy.cfg 应变更为 modified，实际 %v", gotPaths)
	}
	if gotPaths[manifestPath] != render.ChangeModified {
		t.Errorf("静态 Pod 清单应变更为 modified（否则 kubelet 不重建 pod），实际 %v", gotPaths)
	}
	if got := fileOf(t, specOf(second), configPath); !strings.Contains(got, "server m4 10.0.0.4:6443 check") {
		t.Errorf("新配置里应当包含 m4：\n%s", got)
	}
}

// TestLocalProxyDeterministic 断言同一输入两次渲染哈希一致。
func TestLocalProxyDeterministic(t *testing.T) {
	cluster := testCluster(t, clusterYAML())
	root := t.TempDir()
	first, err := loadbalancer.Build(testContext(t, cluster, "w1", root))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	second, err := loadbalancer.Build(testContext(t, cluster, "w1", root))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if specOf(first).Hash() != specOf(second).Hash() {
		t.Errorf("两次渲染哈希不同：%s vs %s", specOf(first).Hash(), specOf(second).Hash())
	}
}

// TestLocalProxyBackendsRoundTrip 断言后端列表能被解析回来（phase 的 Check 依赖它）。
func TestLocalProxyBackendsRoundTrip(t *testing.T) {
	cluster := testCluster(t, clusterYAML())
	c := testContext(t, cluster, "w1", t.TempDir())
	plan, err := loadbalancer.Build(c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lp, err := loadbalancer.NewLocalProxy(c)
	if err != nil {
		t.Fatalf("NewLocalProxy: %v", err)
	}
	backends := lp.Backends(c)
	cfg := fileOf(t, specOf(plan), c.Paths.Resolve("/etc/haproxy/haproxy.cfg"))
	parsed := loadbalancer.LocalProxyBackendsFromConfig(cfg)
	if !loadbalancer.BackendsEqual(backends, parsed) {
		t.Errorf("解析回来的后端 %+v 与期望 %+v 不一致", parsed, backends)
	}
	if loadbalancer.BackendsEqual(backends, []loadbalancer.Backend{{Name: "m9", Address: "10.9.9.9", Port: 6443}}) {
		t.Error("不同的后端列表不应被判为相同")
	}
	// 顺序无关。
	reordered := []loadbalancer.Backend{backends[2], backends[0], backends[1]}
	if !loadbalancer.BackendsEqual(backends, reordered) {
		t.Error("后端顺序变化不应被判为不同")
	}
}

// describeChanges 渲染变更列表，供失败信息使用。
func describeChanges(changes []render.FileChange) []string {
	out := make([]string, 0, len(changes))
	for _, ch := range changes {
		out = append(out, string(ch.Kind)+" "+ch.File.Path)
	}
	return out
}
