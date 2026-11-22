package loadbalancer_test

import (
	"strings"
	"testing"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/loadbalancer"
	"kubemono/internal/render"
)

// clusterYAML 渲染一份最小但完整的集群配置。
func clusterYAML() string {
	return (`cluster:
  hosts:
    - { name: m1, address: 10.0.0.1, roles: [master] }
    - { name: m2, address: 10.0.0.2, roles: [master] }
    - { name: m3, address: 10.0.0.3, roles: [master] }
    - { name: w1, address: 10.0.0.11, roles: [worker] }
    - { name: w2, address: 10.0.0.12, roles: [worker] }
network:
  plugin: kube-router
`)
}

// testCluster 加载并校验一份集群配置。
func testCluster(t *testing.T, doc string) *clusterconfig.Cluster {
	t.Helper()
	c, err := clusterconfig.LoadCluster([]byte(doc))
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	return c
}

// testContext 为给定根目录下的某个主机构造渲染上下文。
func testContext(t *testing.T, cluster *clusterconfig.Cluster, nodeName, root string) *render.Context {
	t.Helper()
	host, ok := cluster.Cluster.Hosts.FindHost(nodeName)
	if !ok {
		t.Fatalf("host %s 不在配置里", nodeName)
	}
	return &render.Context{
		Cluster:   cluster,
		Host:      host,
		BOM:       bom.MustLoad(),
		Paths:     constants.NewPaths(root),
		Release:   "v1.31.4-km.1",
		ClusterID: "test-cluster",
	}
}

// addMaster 向克隆出的配置追加一个 master。
func addMaster(t *testing.T, cluster *clusterconfig.Cluster, name, address string) *clusterconfig.Cluster {
	t.Helper()
	data, err := clusterconfig.MarshalCluster(cluster)
	if err != nil {
		t.Fatalf("MarshalCluster: %v", err)
	}
	clone, err := clusterconfig.LoadCluster(data)
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	clone.Cluster.Hosts = append(clone.Cluster.Hosts, clusterconfig.Host{
		Name: name, Address: address, Roles: []clusterconfig.Role{clusterconfig.RoleMaster},
	})
	return clone
}

// fileOf 返回某个渲染文件的内容。
func fileOf(t *testing.T, spec *render.Spec, path string) string {
	t.Helper()
	f, ok := spec.Find(path)
	if !ok {
		t.Fatalf("渲染结果里没有 %s；实际：%v", path, pathsOf(spec))
	}
	return string(f.Content)
}

// pathsOf 返回 spec 中排好序的路径列表。
func pathsOf(spec *render.Spec) []string {
	out := make([]string, 0, len(spec.Files))
	for _, f := range spec.Files {
		out = append(out, f.Path)
	}
	return out
}

// containsAll 断言 s 包含全部子串。
func containsAll(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !contains(s, sub) {
			t.Errorf("%s 里应当包含 %q：\n%s", what, sub, s)
		}
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// specOf 把 Plan 的文件列表转成 render.Spec，供测试做差异/查找断言。
// 生产代码不需要这个转换，因此不放回 loadbalancer.Plan 上。
func specOf(p *loadbalancer.Plan) *render.Spec {
	spec := &render.Spec{}
	for _, f := range p.Files {
		spec.Add(f)
	}
	spec.Sort()
	return spec
}
