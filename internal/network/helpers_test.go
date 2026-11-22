package network_test

import (
	"fmt"
	"strings"
	"testing"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
)

// testCluster 从 YAML 构造集群配置，补默认值并校验。
func testCluster(t *testing.T, yamlDoc string) *clusterconfig.Cluster {
	t.Helper()
	c, err := clusterconfig.LoadCluster([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	return c
}

// clusterYAML 渲染一份最小但完整的集群配置，按参数覆盖 network。
// 它刻意把 network 块完整写出，让每个测试明确声明自己覆盖的是哪种组合。
func clusterYAML(plugin string) string {
	return fmt.Sprintf(`cluster:
  hosts:
    - { name: m1, address: 10.0.0.1, roles: [master] }
    - { name: m2, address: 10.0.0.2, roles: [master] }
    - { name: m3, address: 10.0.0.3, roles: [master] }
    - { name: w1, address: 10.0.0.11, roles: [worker] }
network:
  plugin: %s
  podCIDR: 10.244.0.0/16
  serviceCIDR: 10.96.0.0/12
  nodeCIDRMaskSize: 24
`, plugin)
}

// testContext 为指定主机构造渲染上下文。
func testContext(t *testing.T, cluster *clusterconfig.Cluster, nodeName string, root string) *render.Context {
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

// fileOf 返回某个渲染文件的内容。
func fileOf(t *testing.T, spec *render.Spec, path string) string {
	t.Helper()
	f, ok := spec.Find(path)
	if !ok {
		t.Fatalf("渲染结果里没有 %s；实际文件：%v", path, pathsOf(spec))
	}
	return string(f.Content)
}

// pathsOf 返回 spec 中已排序的路径列表。
func pathsOf(spec *render.Spec) []string {
	out := make([]string, 0, len(spec.Files))
	for _, f := range spec.Files {
		out = append(out, f.Path)
	}
	return out
}

// hasFile 报告 spec 是否包含该路径。
func hasFile(spec *render.Spec, path string) bool {
	_, ok := spec.Find(path)
	return ok
}

// containsAll 断言 s 包含每一个子串。
func containsAll(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s 里应当包含 %q：\n%s", what, sub, s)
		}
	}
}

// containsNone 断言 s 不包含任何给定子串。
func objectYAML(t *testing.T, o render.Object) string {
	t.Helper()
	data, err := o.Data()
	if err != nil {
		t.Fatalf("Data(%s): %v", o.Ref(), err)
	}
	return string(data)
}

// objectOf 返回对象集合中某个对象的 YAML。
func objectOf(t *testing.T, set *render.ObjectSet, kind, name string) string {
	t.Helper()
	o, ok := set.Find(kind, name)
	if !ok {
		t.Fatalf("对象集合里没有 %s/%s；实际：%v", kind, name, set.Refs())
	}
	return objectYAML(t, o)
}
