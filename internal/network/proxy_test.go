package network_test

import (
	"strings"
	"testing"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/network"
	"kubemono/internal/render"
)

// TestCombinationsRenderExpectedFiles 覆盖网络层的组合：
// - plugin=kube-router 时 kube-router 只提供 CNI，kube-proxy 始终提供 Service；
// - plugin=none 时不写 CNI、不创建 kube-router，但仍默认部署 kube-proxy。
func TestCombinationsRenderExpectedFiles(t *testing.T) {
	cases := []struct {
		name       string
		plugin     string
		wantCNI    bool
		wantRouter bool
	}{
		{"kube-router", clusterconfig.PluginKubeRouter, true, true},
		{"none", clusterconfig.PluginNone, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testCluster(t, clusterYAML(tc.plugin))
			c := testContext(t, cluster, "w1", t.TempDir())
			plan, err := network.Build(c, "")
			if err != nil {
				t.Fatalf("network.Build: %v", err)
			}
			spec := plan.Spec()
			objects := objectSet(plan)
			refs := objects.Refs()

			if got := hasFile(spec, network.CNIConfigPath(c)); got != tc.wantCNI {
				t.Errorf("CNI 配置存在=%v，期望 %v；文件：%v", got, tc.wantCNI, pathsOf(spec))
			}
			if got := containsSuffix(refs, "DaemonSet/kube-system/kube-router"); got != tc.wantRouter {
				t.Errorf("kube-router DaemonSet 存在=%v，期望 %v；对象：%v", got, tc.wantRouter, refs)
			}
			if !containsSuffix(refs, "DaemonSet/kube-system/kube-proxy") {
				t.Errorf("kube-proxy DaemonSet 必须存在：%v", refs)
			}
			if !containsSuffix(refs, "ServiceAccount/kube-system/kube-proxy") ||
				!containsSuffix(refs, "ClusterRoleBinding/kube-proxy") ||
				!containsSuffix(refs, "ConfigMap/kube-system/kube-proxy") {
				t.Errorf("kube-proxy 的 SA/RBAC/ConfigMap 必须存在：%v", refs)
			}
			if tc.wantRouter {
				containsAll(t, "CNI 配置", fileOf(t, spec, network.CNIConfigPath(c)), "kubemono0", "host-local", "10.244.1.0/24",
					`"plugins"`, `"portmap"`, `"portMappings"`)
				ds := objectOf(t, objects, "DaemonSet", constants.ComponentKubeRouter)
				containsAll(t, "kube-router DaemonSet", ds, "--run-service-proxy=false", "--run-router=true", "--run-firewall=true",
					"--enable-cni=false", "hostNetwork: true", "system-node-critical")
				for _, sub := range []string{"--run-service-proxy=true", "--service-cluster-ip-range"} {
					if strings.Contains(ds, sub) {
						t.Errorf("kube-router 不应再接管 Service 代理（含 %q）：\n%s", sub, ds)
					}
				}
			}
			config := objectOf(t, objects, "ConfigMap", constants.ComponentKubeProxy)
			containsAll(t, "kube-proxy ConfigMap", config, "mode: ipvs", "scheduler: rr", "clusterCIDRs:", "10.244.0.0/16")
			proxy := objectOf(t, objects, "DaemonSet", constants.ComponentKubeProxy)
			containsAll(t, "kube-proxy DaemonSet", proxy, "hostNetwork: true", "system-node-critical",
				"/var/lib/kube-proxy/config.conf", "--hostname-override=$(NODE_NAME)")
		})
	}
}

// TestKubeProxyModes 断言 mode 直接进入 KubeProxyConfiguration。
func TestKubeProxyModes(t *testing.T) {
	for _, mode := range []string{clusterconfig.KubeProxyModeIPVS, clusterconfig.KubeProxyModeIPTables, clusterconfig.KubeProxyModeNFTables} {
		t.Run(mode, func(t *testing.T) {
			cluster := testCluster(t, clusterYAML(clusterconfig.PluginKubeRouter))
			cluster.Network.KubeProxy.Mode = mode
			c := testContext(t, cluster, "w1", t.TempDir())
			plan, err := network.Build(c, "")
			if err != nil {
				t.Fatalf("network.Build: %v", err)
			}
			config := objectOf(t, objectSet(plan), "ConfigMap", constants.ComponentKubeProxy)
			containsAll(t, "kube-proxy ConfigMap", config, "mode: "+mode)
			if mode != clusterconfig.KubeProxyModeIPVS && strings.Contains(config, "scheduler:") {
				t.Errorf("非 ipvs 模式不应写 scheduler：\n%s", config)
			}
		})
	}
}

// TestKubeProxyCanBeDisabled 断言自带 kube-proxy replacement 的 CNI 可以关掉它。
func TestKubeProxyCanBeDisabled(t *testing.T) {
	cluster := testCluster(t, clusterYAML(clusterconfig.PluginNone))
	cluster.Network.KubeProxy.Enabled = clusterconfig.BoolPtr(false)
	c := testContext(t, cluster, "w1", t.TempDir())
	plan, err := network.Build(c, "")
	if err != nil {
		t.Fatalf("network.Build: %v", err)
	}
	for _, ref := range objectSet(plan).Refs() {
		if strings.Contains(ref, "kube-proxy") {
			t.Errorf("kubeProxy.enabled=false 不应创建 %s", ref)
		}
	}
	for _, h := range plan.Healths {
		if h.Component == constants.ComponentKubeProxy {
			t.Errorf("kubeProxy.enabled=false 不应探活 kube-proxy：%+v", h)
		}
	}
}

// TestPluginNoneWritesNoCNIConfig 断言 ：plugin=none 时网络层不写任何 CNI 配置。
func TestPluginNoneWritesNoCNIConfig(t *testing.T) {
	{
		cluster := testCluster(t, clusterYAML(clusterconfig.PluginNone))
		c := testContext(t, cluster, "w1", t.TempDir())
		plan, err := network.Build(c, "")
		if err != nil {
			t.Fatalf("network.Build: %v", err)
		}
		for _, f := range plan.Files {
			if strings.HasPrefix(f.Path, c.Paths.CNIConfig()) {
				t.Errorf("plugin=none 不应写 %s", f.Path)
			}
		}
	}
}

// TestRenderDeterministic 断言同一输入两次渲染哈希一致（渲染确定性的硬要求）。
func TestRenderDeterministic(t *testing.T) {
	// 只按 plugin 取值：kubemono 没有"部署模式"这个维度；不同部署模式下的
	// 输入完全相同，保留这些维度只会形成假覆盖。
	for _, plugin := range []string{clusterconfig.PluginKubeRouter, clusterconfig.PluginNone} {
		cluster := testCluster(t, clusterYAML(plugin))
		c := testContext(t, cluster, "w1", t.TempDir())
		first, err := network.Build(c, "")
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		second, err := network.Build(c, "")
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if first.Spec().Hash() != second.Spec().Hash() {
			t.Errorf("plugin=%s：两次渲染哈希不同", plugin)
		}
		firstObjects := objectYAMLs(t, objectSet(first))
		secondObjects := objectYAMLs(t, objectSet(second))
		if strings.Join(firstObjects, "\n---\n") != strings.Join(secondObjects, "\n---\n") {
			t.Errorf("plugin=%s：两次渲染的集群对象不同", plugin)
		}
	}
}

// TestPluginNoneRemovesCNIConfig 断言切到 plugin=none 时会删除旧的 CNI 配置。
func TestPluginNoneRemovesCNIConfig(t *testing.T) {
	withRouter := testCluster(t, clusterYAML(clusterconfig.PluginKubeRouter))
	withNone := testCluster(t, clusterYAML(clusterconfig.PluginNone))
	c := testContext(t, withNone, "w1", t.TempDir())
	// 新旧 Spec 必须使用同一个 root，否则路径不同、无法比较。
	root := c.Paths.Root
	oldCtx := testContext(t, withRouter, "w1", root)
	old, err := network.Build(oldCtx, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	plan, err := network.Build(c, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	changes := render.ComputeChanges(old.Spec(), plan.Spec())
	removed := map[string]bool{}
	for _, ch := range changes {
		if ch.Kind == render.ChangeRemoved {
			removed[ch.File.Path] = true
		}
	}
	if !removed[network.CNIConfigPath(c)] {
		t.Errorf("切换到 plugin=none 时应当删除 %s；removed=%v", network.CNIConfigPath(c), removed)
	}
}

// objectYAMLs 返回集合中每个对象的 YAML。
func objectYAMLs(t *testing.T, set *render.ObjectSet) []string {
	t.Helper()
	out := make([]string, 0, set.Len())
	for _, o := range set.Objects() {
		out = append(out, objectYAML(t, o))
	}
	return out
}

// contains 报告字符串切片是否包含指定值。
func TestExtraArgsFlowToKubeRouter(t *testing.T) {
	cluster := testCluster(t, clusterYAML(clusterconfig.PluginKubeRouter))
	cluster.Network.KubeRouter.ExtraArgs = map[string]string{"v": "3"}
	c := testContext(t, cluster, "w1", t.TempDir())
	plan, err := network.Build(c, "")
	if err != nil {
		t.Fatalf("network.Build: %v", err)
	}
	router := objectOf(t, objectSet(plan), "DaemonSet", constants.ComponentKubeRouter)
	containsAll(t, "kube-router DaemonSet", router, "--v=3")
}

// containsSuffix 报告 list 中是否有以 suffix 结尾的元素。
func containsSuffix(list []string, suffix string) bool {
	for _, item := range list {
		if strings.HasSuffix(item, suffix) {
			return true
		}
	}
	return false
}

// TestBuildUsesHostMTU 固定 MTU 自动探测的接线：
// render.Context.HostMTU 必须真正进入 CNI conflist，而不是永远 DefaultHostMTU。
func TestBuildUsesHostMTU(t *testing.T) {
	cluster := testCluster(t, clusterYAML(clusterconfig.PluginKubeRouter))
	c := testContext(t, cluster, "w1", t.TempDir())
	c.HostMTU = 9000
	plan, err := network.Build(c, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	conflist := fileOf(t, plan.Spec(), network.CNIConfigPath(c))
	containsAll(t, "CNI 配置", conflist, `"mtu": 8980`)
}

// objectSet 把 Plan.Objects 包成 render.ObjectSet（Plan 不为测试保留访问器）。
func objectSet(p *network.Plan) *render.ObjectSet {
	set := &render.ObjectSet{}
	set.Add(p.Objects...)
	return set
}
