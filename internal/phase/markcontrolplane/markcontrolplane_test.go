package markcontrolplane

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/phase"
	"kubemono/internal/util/output"
)

func testPhase(t *testing.T) (*Phase, *phase.Context, *fake.Clientset) {
	t.Helper()
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
	}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost("m1")
	b := bom.MustLoad()
	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "m1"}})
	pc := &phase.Context{
		Cluster: cluster,
		Host:    host,
		BOM:     b,
		Paths:   constants.NewPaths(t.TempDir()),
		Release: b.Release,
		Report:  output.Discard(),
		Client:  client,
		Now:     func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	return &Phase{Client: client, Sleep: func(time.Duration) {}}, pc, client
}

// TestRunAddsEmptyValuedControlPlaneLabel 覆盖真机踩到的坑：控制面标签的值是空串，
// 而 Go 的 map 对不存在的 key 也返回 ""。只比值（`node.Labels[k] != v`）会把
// "标签缺失"当成"已是最新"，于是 Run 从不调用 Update，节点永远没有
// node-role.kubernetes.io/control-plane（kubectl 的 ROLES 一直是 <none>，
// e2e 的 nodes-ready 也会因此失败）。
func TestRunAddsEmptyValuedControlPlaneLabel(t *testing.T) {
	p, pc, client := testPhase(t)

	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	node, err := client.CoreV1().Nodes().Get(context.Background(), "m1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := node.Labels[LabelControlPlane]; !ok {
		t.Fatalf("Run 之后必须带上 %s 标签，实际 labels=%v", LabelControlPlane, node.Labels)
	}

	// 幂等：再跑一次不应报错，标签仍在。
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	if done, err := p.Check(context.Background(), pc); err != nil || !done {
		t.Fatalf("第二次之后 Check 应为 true，实际 done=%v err=%v", done, err)
	}
}

// TestCheckReportsMissingLabel 断言 Check 在标签缺失时报"未完成"。
func TestCheckReportsMissingLabel(t *testing.T) {
	p, pc, client := testPhase(t)
	if done, err := p.Check(context.Background(), pc); err != nil || done {
		t.Fatalf("缺标签时 Check 应为 false，实际 done=%v err=%v", done, err)
	}
	node, err := client.CoreV1().Nodes().Get(context.Background(), "m1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[LabelControlPlane] = ""
	if _, err := client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if done, err := p.Check(context.Background(), pc); err != nil || !done {
		t.Fatalf("标签就位后 Check 应为 true，实际 done=%v err=%v", done, err)
	}
}

// 纯 worker 也会注册 Node，必须执行标签 phase；纯 master 不注册 Node，跳过。
func TestAppliesOnWorkerButNotPureMaster(t *testing.T) {
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "w1", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}, Labels: map[string]string{"zone": "a"}},
	}
	clusterconfig.SetDefaults(cluster)
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"m1", false},
		{"w1", true},
	} {
		host, ok := cluster.Cluster.Hosts.FindHost(tc.name)
		if !ok {
			t.Fatal(tc.name)
		}
		pc := &phase.Context{Cluster: cluster, Host: host}
		if got := (&Phase{}).Applies(pc); got != tc.want {
			t.Fatalf("%s Applies = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Run 必须把 hosts[].labels 写到 worker Node，且不能凭空加 control-plane 标签。
func TestRunSetsWorkerLabels(t *testing.T) {
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "w1", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}, Labels: map[string]string{"zone": "a"}},
	}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost("w1")
	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}})
	pc := &phase.Context{Cluster: cluster, Host: host, Client: client, Report: output.Discard()}
	p := &Phase{Client: client, Sleep: func(time.Duration) {}}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	node, err := client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if node.Labels["zone"] != "a" {
		t.Fatalf("worker label zone 未写入: %v", node.Labels)
	}
	if _, ok := node.Labels[LabelControlPlane]; ok {
		t.Fatalf("纯 worker 不应有 control-plane 标签: %v", node.Labels)
	}
}

// TestRunKeepsExistingLabelsAndAddsDesired 固定"只增不删"语义：
// Node 上已有的标签（kubelet/云厂商/旧配置）不会被本 phase 删除。
func TestRunKeepsExistingLabelsAndAddsDesired(t *testing.T) {
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "w1", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}, Labels: map[string]string{"zone": "a"}},
	}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost("w1")
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "w1",
			Labels: map[string]string{"old": "1", "kubernetes.io/hostname": "w1"},
		},
	})
	pc := &phase.Context{Cluster: cluster, Host: host, Client: client, Report: output.Discard()}
	p := &Phase{Client: client, Sleep: func(time.Duration) {}}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	node, err := client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if node.Labels["old"] != "1" || node.Labels["kubernetes.io/hostname"] != "w1" {
		t.Fatalf("已有标签被误删: %v", node.Labels)
	}
	if node.Labels["zone"] != "a" {
		t.Fatalf("新 label 未写入: %v", node.Labels)
	}
}

// TestRunReconcilesTaints 固定 taint 热更新：同 key+effect 改值走 Node API，
// 非 kubemono 管理的 taint 保持不变；配置里删除 taint 不会主动移除。
func TestRunReconcilesTaints(t *testing.T) {
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "w1", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleWorker},
			Taints: []string{"dedicated=infra:NoSchedule"}},
	}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost("w1")
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "cloud", Value: "provider", Effect: corev1.TaintEffectNoExecute},
		}},
	})
	pc := &phase.Context{Cluster: cluster, Host: host, Client: client, Report: output.Discard()}
	p := &Phase{Client: client, Sleep: func(time.Duration) {}}

	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	node, _ := client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if !hasTaint(node.Spec.Taints, corev1.Taint{Key: "dedicated", Value: "infra", Effect: corev1.TaintEffectNoSchedule}) {
		t.Fatalf("desired taint 未写入: %v", node.Spec.Taints)
	}
	if !hasTaint(node.Spec.Taints, corev1.Taint{Key: "cloud", Value: "provider", Effect: corev1.TaintEffectNoExecute}) {
		t.Fatalf("非受管 taint 被误删: %v", node.Spec.Taints)
	}

	host.Taints = []string{"dedicated=gpu:NoSchedule"}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	node, _ = client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if !hasTaint(node.Spec.Taints, corev1.Taint{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}) {
		t.Fatalf("taint 值未更新: %v", node.Spec.Taints)
	}
	if hasTaint(node.Spec.Taints, corev1.Taint{Key: "dedicated", Value: "infra", Effect: corev1.TaintEffectNoSchedule}) {
		t.Fatalf("旧 taint 值未替换: %v", node.Spec.Taints)
	}

	host.Taints = nil
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("third Run: %v", err)
	}
	node, _ = client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if !hasTaint(node.Spec.Taints, corev1.Taint{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}) {
		t.Fatalf("配置删除后 taint 不应被主动移除: %v", node.Spec.Taints)
	}
	if !hasTaint(node.Spec.Taints, corev1.Taint{Key: "cloud", Value: "provider", Effect: corev1.TaintEffectNoExecute}) {
		t.Fatalf("非受管 taint 被误删: %v", node.Spec.Taints)
	}
}

func TestRejectsInvalidTaintEffect(t *testing.T) {
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "w1", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}, Taints: []string{"bad:Sometimes"}},
	}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost("w1")
	pc := &phase.Context{Cluster: cluster, Host: host, Client: fake.NewSimpleClientset(), Report: output.Discard()}
	err := (&Phase{Client: pc.Client, Sleep: func(time.Duration) {}}).Run(context.Background(), pc)
	if err == nil || !strings.Contains(err.Error(), "taint effect") {
		t.Fatalf("非法 effect 应报错，实际 %v", err)
	}
}
