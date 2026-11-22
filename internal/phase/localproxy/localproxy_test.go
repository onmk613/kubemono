package localproxy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/loadbalancer"
	"kubemono/internal/phase"
	localproxyphase "kubemono/internal/phase/localproxy"
	"kubemono/internal/render"
	"kubemono/internal/util/output"
)

// clusterDoc 为 phase 测试构建一份通过校验的集群配置。
func clusterDoc(masters [][2]string) string {
	var b strings.Builder
	b.WriteString("cluster:\n  hosts:\n")
	for _, m := range masters {
		b.WriteString("    - { name: " + m[0] + ", address: " + m[1] + ", roles: [master] }\n")
	}
	b.WriteString("    - { name: w1, address: 10.0.0.11, roles: [worker] }\n")
	b.WriteString(`network:
  plugin: kube-router
controlPlane: {}
`)
	return b.String()
}

// testContext 以临时目录为根构建一个 phase 上下文。
func testContext(t *testing.T, doc, nodeName string) *phase.Context {
	t.Helper()
	cluster, err := clusterconfig.LoadCluster([]byte(doc))
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	host, ok := cluster.Cluster.Hosts.FindHost(nodeName)
	if !ok {
		t.Fatalf("host %s 不在配置里", nodeName)
	}
	return &phase.Context{
		Cluster:   cluster,
		ClusterID: "test-cluster",
		Host:      host,
		BOM:       bom.MustLoad(),
		Paths:     constants.NewPaths(t.TempDir()),
		Release:   "v1.31.4-km.1",
		Report:    output.Discard(),
		Now:       func() time.Time { return time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC) },
	}
}

// fakeHTTP 总是返回配置好的状态码。
type fakeHTTP struct {
	status int
	err    error
	urls   []string
}

// Get 实现 loadbalancer.HTTPGetter。
func (f *fakeHTTP) Get(url string) (int, error) {
	f.urls = append(f.urls, url)
	if f.err != nil {
		return 0, f.err
	}
	return f.status, nil
}

// TestAppliesOnlyOnWorkers 断言 ：master 不需要本地代理。
func TestAppliesOnlyOnWorkers(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}, {"m2", "10.0.0.2"}})
	p := &localproxyphase.Phase{}
	if !p.Applies(testContext(t, doc, "w1")) {
		t.Error("worker 上本地代理应当适用")
	}
	if p.Applies(testContext(t, doc, "m1")) {
		t.Error("master 上本地代理不适用（直连本机 apiserver）")
	}
}

// TestRunRemovesFilesWhenExternalEndpointsSet 断言切换到外部入口后 phase 会清理遗留文件。
func TestRunRemovesFilesWhenExternalEndpointsSet(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}})
	c := testContext(t, doc, "w1")
	p := &localproxyphase.Phase{}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	configPath := loadbalancer.LocalProxyConfigPath(renderCtx(c))
	manifestPath := c.Paths.ManifestPath(constants.ComponentLocalProxy)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("首次运行应写配置：%v", err)
	}
	c.Cluster.ControlPlane.ExternalEndpoints = []string{"vip.example.com:6443"}
	if !p.Applies(c) {
		t.Fatal("设置 externalEndpoints 后 phase 仍需运行以清理遗留文件")
	}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("清理 Run: %v", err)
	}
	for _, path := range []string{configPath, manifestPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("关闭本地代理后应删除 %s：%v", path, err)
		}
	}
	done, err := p.Check(context.Background(), c)
	if err != nil || !done {
		t.Fatalf("清理后 Check 应为 done：done=%v err=%v", done, err)
	}
}

// TestRunOnMasterIsANoop 断言 master 上 Run 不落盘任何文件。
func TestRunOnMasterIsANoop(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}})
	c := testContext(t, doc, "m1")
	p := &localproxyphase.Phase{}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(loadbalancer.LocalProxyConfigPath(renderCtx(c))); !os.IsNotExist(err) {
		t.Errorf("master 上不应写 haproxy 配置：%v", err)
	}
}

// TestRunWritesStaticPodInBinaryMode 断言 binary 模式也写静态 Pod 清单，不写 unit。
func TestRunWritesStaticPodInBinaryMode(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}, {"m2", "10.0.0.2"}})
	c := testContext(t, doc, "w1")
	p := &localproxyphase.Phase{}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(loadbalancer.LocalProxyConfigPath(renderCtx(c))); err != nil {
		t.Errorf("haproxy.cfg 未落盘：%v", err)
	}
	if _, err := os.Stat(c.Paths.ManifestPath(constants.ComponentLocalProxy)); err != nil {
		t.Errorf("haproxy 静态 Pod 清单未落盘：%v", err)
	}
	if _, err := os.Stat(c.Paths.Resolve("/etc/systemd/system/haproxy.service")); !os.IsNotExist(err) {
		t.Errorf("binary 模式也不应再写 haproxy.service：%v", err)
	}
}

// TestRunIdempotent 断言内容没变时重复执行是幂等的。
func TestRunIdempotent(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}})
	c := testContext(t, doc, "w1")
	p := &localproxyphase.Phase{}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	if _, err := os.Stat(c.Paths.ManifestPath(constants.ComponentLocalProxy)); err != nil {
		t.Errorf("第二次 Run 后清单应当仍在：%v", err)
	}
}

// TestDryRunDoesNotWrite 断言 dry-run 不落盘。
func TestDryRunDoesNotWrite(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}})
	c := testContext(t, doc, "w1")
	c.DryRun = true
	p := &localproxyphase.Phase{}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(loadbalancer.LocalProxyConfigPath(renderCtx(c))); !os.IsNotExist(err) {
		t.Errorf("dry-run 不应写 haproxy 配置：%v", err)
	}
}

// TestCheckComparesBackendsAndHealth 断言 Check 同时比对后端列表与健康端点。
func TestCheckComparesBackendsAndHealth(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}, {"m2", "10.0.0.2"}})
	c := testContext(t, doc, "w1")
	http := &fakeHTTP{status: 200}
	p := &localproxyphase.Phase{HTTP: http}

	// 还没落盘：Check 应当返回 false，且不去探测健康（先行短路）。
	done, err := p.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Error("未落盘时 Check 应当返回 false")
	}
	if len(http.urls) != 0 {
		t.Errorf("文件不一致时不应探测健康端点：%v", http.urls)
	}

	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	done, err = p.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Error("落盘且健康时 Check 应当返回 true")
	}
	containsAny(t, http.urls, "/stats")
}

// TestCheckDetectsBackendDrift 断言后端列表与 desired 不一致时 Check 返回 false。
func TestCheckDetectsBackendDrift(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}})
	c := testContext(t, doc, "w1")
	http := &fakeHTTP{status: 200}
	p := &localproxyphase.Phase{HTTP: http}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 手工把配置里的后端改掉（模拟"master 列表变了但本机还没同步"）。
	configPath := loadbalancer.LocalProxyConfigPath(renderCtx(c))
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(content), "server m1 10.0.0.1:6443 check", "server m1 10.9.9.9:6443 check", 1)
	if err := os.WriteFile(configPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err := p.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Error("后端列表漂移时 Check 应当返回 false")
	}
}

// TestCheckUnhealthyProxy 断言健康检查失败时 Check 返回 false。
func TestCheckUnhealthyProxy(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}})
	c := testContext(t, doc, "w1")
	p := &localproxyphase.Phase{HTTP: &fakeHTTP{status: 503}}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	done, err := p.Check(context.Background(), c)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Error("健康检查失败时 Check 应当返回 false")
	}
}

// TestRunRendersStaticPodManifest 断言 staticpod 模式渲染清单而不是 unit。
func TestRunRendersStaticPodManifest(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}})
	c := testContext(t, doc, "w1")
	p := &localproxyphase.Phase{}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(c.Paths.ManifestPath(constants.ComponentLocalProxy)); err != nil {
		t.Errorf("应当渲染静态 Pod 清单：%v", err)
	}
	if _, err := os.Stat(c.Paths.Resolve("/etc/systemd/system/haproxy.service")); !os.IsNotExist(err) {
		t.Errorf("staticpod 模式不应渲染 unit：%v", err)
	}
}

// renderCtx 把 phase 上下文转换成 render 上下文。
func renderCtx(c *phase.Context) *render.Context {
	return &render.Context{
		Cluster: c.Cluster, Host: c.Host, Node: c.Node, BOM: c.BOM, Paths: c.Paths,
		Release: c.Release, ClusterID: c.ClusterID,
	}
}

// containsAny 断言至少有一个元素包含该子串。
func containsAny(t *testing.T, list []string, sub string) {
	t.Helper()
	for _, v := range list {
		if strings.Contains(v, sub) {
			return
		}
	}
	t.Errorf("%v 里应当有一个元素包含 %q", list, sub)
}

// TestRunKeepsFilesOfOtherComponents 与 network phase 同类：本机 desired 快照是
// 整机共享的，本地代理只能比较自己负责的文件，否则会把别的组件的文件
// （控制面清单、kubelet 配置……）判成"应删除"并真的删掉。
func TestRunKeepsFilesOfOtherComponents(t *testing.T) {
	doc := clusterDoc([][2]string{{"m1", "10.0.0.1"}})
	c := testContext(t, doc, "w1")

	foreign := []render.File{
		{Path: c.Paths.KubeletConfig(), Content: []byte("kubelet-config"), Mode: 0o644,
			Component: constants.ComponentKubelet, Kind: render.KindConfig},
		{Path: c.Paths.ManifestPath(constants.ComponentAPIServer), Content: []byte("apiserver-pod"), Mode: 0o644,
			Component: constants.ComponentAPIServer, Kind: render.KindManifest},
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

	p := &localproxyphase.Phase{}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range foreign {
		if _, err := os.Stat(f.Path); err != nil {
			t.Errorf("local-proxy phase 删掉了别的组件的文件 %s：%v", f.Path, err)
		}
	}
}

func TestExternalEndpointsSkipLocalProxy(t *testing.T) {
	doc := strings.Replace(clusterDoc([][2]string{{"m1", "10.0.0.1"}, {"m2", "10.0.0.2"}}),
		"controlPlane: {}", "controlPlane:\n  externalEndpoints: [vip.example.com:6443]", 1)
	c := testContext(t, doc, "w1")
	p := &localproxyphase.Phase{}
	if err := p.Run(context.Background(), c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(loadbalancer.LocalProxyConfigPath(c.RenderContext())); !os.IsNotExist(err) {
		t.Fatalf("设置 externalEndpoints 后不应写 haproxy 配置：%v", err)
	}
}
