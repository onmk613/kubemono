package addon

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"kubemono/internal/addons"
	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	"kubemono/internal/util/output"
)

// objOf 把一个 typed 对象包装成 render.Object（与生产渲染一致）。
func objOf(kind, namespace, name string, v any) render.Object {
	return render.NewClusterObject(kind, namespace, name, v)
}

// compOf 构造一个测试用的组件。
func compOf(name string, replicas int32, objs ...render.Object) component {
	set := &render.ObjectSet{}
	set.Add(objs...)
	return component{name: name, replicas: replicas, objects: set}
}

func testDeployment(name string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: constants.KubeSystemNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  name,
					Image: "example.invalid/" + name + ":v1",
				}}},
			},
		},
	}
}

// testPhase 注入现成的组件集合（只测 phase 逻辑，不走渲染）。
func testPhase(client *fake.Clientset, comps ...component) *Phase {
	if comps == nil {
		comps = []component{}
	}
	return &Phase{Client: client, components: comps}
}

// testPhaseWithCoreDNS 用真实 CoreDNS 渲染（含"保留集群现状"的合并）。
func testPhaseWithCoreDNS(t *testing.T, client *fake.Clientset, list ...addons.CoreDNS) *Phase {
	t.Helper()
	p := &Phase{Client: client}
	comps, err := p.componentsFor(context.Background(), client, list)
	if err != nil {
		t.Fatalf("渲染 CoreDNS: %v", err)
	}
	p.components = comps
	return p
}

func markDeploymentReady(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	ctx := context.Background()
	dep, err := client.AppsV1().Deployments(constants.KubeSystemNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("读取 Deployment %s: %v", name, err)
	}
	dep.Generation = 1
	dep.Status.ObservedGeneration = 1
	dep.Status.AvailableReplicas = 1
	dep.Status.Replicas = 1
	dep.Status.ReadyReplicas = 1
	dep.Status.UpdatedReplicas = 1
	if _, err := client.AppsV1().Deployments(constants.KubeSystemNamespace).UpdateStatus(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("更新 Deployment 状态: %v", err)
	}
}

// TestRunAppliesAndCheckReportsReady 覆盖 addon phase 的真实 apply 路径：
// Run 写入对象，Check 只有在对象存在且 Deployment 可用时才返回 true。
func TestRunAppliesAndCheckReportsReady(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	p := testPhase(client, compOf("coredns", 1,
		objOf("ServiceAccount", constants.KubeSystemNamespace, "coredns",
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace}}),
		objOf("ConfigMap", constants.KubeSystemNamespace, "coredns",
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace}, Data: map[string]string{"Corefile": "managed"}}),
		objOf("Deployment", constants.KubeSystemNamespace, "coredns", testDeployment("coredns")),
	))
	c := &phase.Context{}

	if done, err := p.Check(ctx, c); err != nil || done {
		t.Fatalf("Run 之前 Check = (%v,%v)，期望 false", done, err)
	}
	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := client.CoreV1().ServiceAccounts(constants.KubeSystemNamespace).Get(ctx, "coredns", metav1.GetOptions{}); err != nil {
		t.Fatalf("ServiceAccount 未创建: %v", err)
	}
	if _, err := client.AppsV1().Deployments(constants.KubeSystemNamespace).Get(ctx, "coredns", metav1.GetOptions{}); err != nil {
		t.Fatalf("Deployment 未创建: %v", err)
	}
	if done, err := p.Check(ctx, c); err != nil || done {
		t.Fatalf("Deployment 未就绪时 Check = (%v,%v)，期望 false", done, err)
	}
	markDeploymentReady(t, client, "coredns")
	if done, err := p.Check(ctx, c); err != nil || !done {
		t.Fatalf("对象存在且 Deployment 可用时 Check = (%v,%v)", done, err)
	}
}

// TestCheckDetectsReplicaDrift 固定"期望 1 副本、实际 2 副本"的缩放场景：
// 只看 AvailableReplicas 会把 2 >= 1 误判为完成，副本永远缩不下去。
func TestCheckDetectsReplicaDrift(t *testing.T) {
	ctx := context.Background()
	actual := testDeployment("coredns")
	two := int32(2)
	actual.Spec.Replicas = &two
	client := fake.NewSimpleClientset(actual)
	dep, err := client.AppsV1().Deployments(constants.KubeSystemNamespace).Get(ctx, "coredns", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dep.Generation = 2
	dep.Status.ObservedGeneration = 2
	dep.Status.Replicas = 2
	dep.Status.UpdatedReplicas = 2
	dep.Status.ReadyReplicas = 2
	dep.Status.AvailableReplicas = 2
	if _, err := client.AppsV1().Deployments(constants.KubeSystemNamespace).UpdateStatus(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	p := testPhase(client, compOf("coredns", 1, objOf("Deployment", constants.KubeSystemNamespace, "coredns", testDeployment("coredns"))))
	if done, err := p.Check(ctx, &phase.Context{}); err != nil || done {
		t.Fatalf("副本数与期望不符时 Check = (%v,%v)，期望 false", done, err)
	}
}

// TestCheckDetectsConfigMapContentDrift 固定：对象内容变化必须让 Check=false，
// 否则 phase runner 会跳过 Run，内容变更就是空操作。
func TestCheckDetectsConfigMapContentDrift(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	p := testPhase(client, compOf("coredns", 1,
		objOf("ConfigMap", constants.KubeSystemNamespace, "coredns",
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace}, Data: map[string]string{"payload": "desired"}}),
		objOf("Deployment", constants.KubeSystemNamespace, "coredns", testDeployment("coredns")),
	))
	if err := p.Run(ctx, &phase.Context{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	markDeploymentReady(t, client, "coredns")
	if done, err := p.Check(ctx, &phase.Context{}); err != nil || !done {
		t.Fatalf("内容一致且 ready 时 Check = (%v,%v)", done, err)
	}
	cm, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Get(ctx, "coredns", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data["payload"] = "drifted"
	if _, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if done, err := p.Check(ctx, &phase.Context{}); err != nil || done {
		t.Fatalf("带外修改后 Check = (%v,%v)，期望 false", done, err)
	}
	if err := p.Run(ctx, &phase.Context{}); err != nil {
		t.Fatalf("修复 Run: %v", err)
	}
	cm, _ = client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Get(ctx, "coredns", metav1.GetOptions{})
	if cm.Data["payload"] != "desired" {
		t.Fatalf("内容未收敛：%+v", cm.Data)
	}
}

// TestRunRemovesDisabledAddons 固定"关闭 addon 必须删除旧对象"，
// 而不是只停止 apply、让旧 Deployment 继续运行。
func TestRunRemovesDisabledAddons(t *testing.T) {
	ctx := context.Background()
	disabled := false
	cfg := &clusterconfig.Cluster{}
	cfg.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(cfg)
	cfg.Addons.CoreDNS.Enabled = &disabled

	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace}},
	)
	p := &Phase{Client: client}
	c := &phase.Context{Cluster: cfg, BOM: bom.MustLoad()}
	if done, err := p.Check(ctx, c); err != nil || done {
		t.Fatalf("禁用 addon 仍有遗留对象时 Check = (%v,%v)", done, err)
	}
	if err := p.Run(ctx, c); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Get(ctx, "coredns", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("CoreDNS ConfigMap 未被删除: %v", err)
	}
	if _, err := client.AppsV1().Deployments(constants.KubeSystemNamespace).Get(ctx, "coredns", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("CoreDNS Deployment 未被删除: %v", err)
	}
	if done, err := p.Check(ctx, c); err != nil || !done {
		t.Fatalf("清理后 Check = (%v,%v)", done, err)
	}
}

// TestCoreDNSMergesUserCorefile 固定 CoreDNS 的合并语义（现在发生在渲染侧）：
// 受管区块被替换，区块外的用户内容保留；合并结果与集群一致时 Check=true。
func TestCoreDNSMergesUserCorefile(t *testing.T) {
	ctx := context.Background()
	existing := "user-before\n" + addons.CoreDNSBlockBegin + "\nold-managed\n" + addons.CoreDNSBlockEnd + "\nuser-after\n"
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace},
		Data:       map[string]string{"Corefile": existing},
	})
	p := testPhaseWithCoreDNS(t, client, addons.CoreDNS{
		ClusterDNS: "10.96.0.10", Domain: "cluster.local", Replicas: 1,
		Upstream: []string{"/etc/resolv.conf"}, Image: "example.invalid/coredns:test",
	})
	if err := p.Run(ctx, &phase.Context{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Get(ctx, "coredns", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := cm.Data["Corefile"]
	for _, want := range []string{"user-before", "user-after", addons.CoreDNSBlockBegin} {
		if !strings.Contains(got, want) {
			t.Fatalf("Corefile 缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old-managed") {
		t.Fatalf("Corefile 仍含旧受管块:\n%s", got)
	}
	markDeploymentReady(t, client, "coredns")
	if done, err := p.Check(ctx, &phase.Context{}); err != nil || !done {
		t.Fatalf("受管区块最新且用户段保留时 Check = (%v,%v)", done, err)
	}
}

// TestCheckRealCoreDNSContent 覆盖真实 CoreDNS addon 的全部对象类型
// （ServiceAccount/ClusterRole/Binding/ConfigMap/Deployment/Service），
// 防止内容比较只在最小对象集上成立。
func TestCheckRealCoreDNSContent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	p := testPhaseWithCoreDNS(t, client, addons.CoreDNS{
		ClusterDNS: "10.96.0.10",
		Domain:     "cluster.local",
		Replicas:   1,
		Upstream:   []string{"/etc/resolv.conf"},
		Image:      "example.invalid/coredns:test",
	})
	if err := p.Run(ctx, &phase.Context{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	markDeploymentReady(t, client, "coredns")
	if done, err := p.Check(ctx, &phase.Context{}); err != nil || !done {
		t.Fatalf("真实 CoreDNS 对象 ready 后 Check = (%v,%v)", done, err)
	}
	if err := p.Run(ctx, &phase.Context{}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
}

// TestAddonsUseImageOverride 固定按组件镜像覆盖：addon 渲染必须与其它组件
// 使用同一份生效 BOM，没有全局仓库前缀。
func TestAddonsUseImageOverride(t *testing.T) {
	cluster := clusterconfig.DefaultCluster()
	effective, err := bom.MustLoad().WithImageOverrides(map[string]string{
		"coredns": "registry.example.com:5000/mirror/coredns/coredns:" + bom.MustLoad().ComponentVersion("coredns"),
	})
	if err != nil {
		t.Fatalf("WithImageOverrides: %v", err)
	}
	c := &phase.Context{Cluster: cluster, BOM: effective, Report: output.Discard()}
	comps, err := (&Phase{}).enabledComponents(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("enabledComponents: %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("组件数 = %d", len(comps))
	}
	o, ok := comps[0].objects.Find("Deployment", "coredns")
	if !ok {
		t.Fatal("缺少 coredns Deployment")
	}
	data, err := o.Data()
	if err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := yaml.Unmarshal(data, &dep); err != nil {
		t.Fatal(err)
	}
	image := dep.Spec.Template.Spec.Containers[0].Image
	if !strings.HasPrefix(image, "registry.example.com:5000/mirror/") {
		t.Fatalf("coredns image = %q，未使用按组件镜像覆盖", image)
	}
}
