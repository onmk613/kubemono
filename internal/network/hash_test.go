package network_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"kubemono/internal/network"
)

// daemonSetConfigHash 取渲染出的 DaemonSet 的 kubemono.io/config-hash 注解。
func daemonSetConfigHash(t *testing.T, plan *network.Plan, name string) string {
	t.Helper()
	for _, o := range plan.Objects {
		if o.Kind() != "DaemonSet" || o.Name() != name {
			continue
		}
		data, err := o.Data()
		if err != nil {
			t.Fatalf("DaemonSet %s Data: %v", name, err)
		}
		var ds appsv1.DaemonSet
		if err := yaml.Unmarshal(data, &ds); err != nil {
			t.Fatalf("解析 DaemonSet %s: %v", name, err)
		}
		return ds.Spec.Template.Annotations["kubemono.io/config-hash"]
	}
	t.Fatalf("渲染结果里没有 DaemonSet %s", name)
	return ""
}

// TestRouterConfigHashCoversAPIServerEndpoint 守住"配置变了要滚动"：
// 必须同时哈希 argv 与 ConfigMap 里的 kubeconfig，否则改 master 地址 /
// externalEndpoints 时 DaemonSet 不滚动，kube-router 继续用旧端点。
func TestRouterConfigHashCoversAPIServerEndpoint(t *testing.T) {
	cluster := testCluster(t, clusterYAML("kube-router"))
	plan1, err := network.Build(testContext(t, cluster, "w1", t.TempDir()), "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	h1 := daemonSetConfigHash(t, plan1, "kube-router")
	if h1 == "" {
		t.Fatal("DaemonSet 缺少 kubemono.io/config-hash 注解")
	}

	// 只加 externalEndpoints：argv 不变，但内嵌 kubeconfig 的 server 从本地
	// haproxy 变成外部入口。外部入口必须参与 config-hash，否则 DaemonSet 不会滚动。
	mutated := *cluster
	mutated.ControlPlane.ExternalEndpoints = []string{"https://api.example.com:6443"}
	plan2, err := network.Build(testContext(t, &mutated, "w1", t.TempDir()), "")
	if err != nil {
		t.Fatalf("Build(mutated): %v", err)
	}
	h2 := daemonSetConfigHash(t, plan2, "kube-router")
	if h1 == h2 {
		t.Fatal("apiserver 端点变化必须改变 config-hash，否则 DaemonSet 不会滚动")
	}
}
