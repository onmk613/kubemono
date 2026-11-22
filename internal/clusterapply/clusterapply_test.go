package clusterapply

import (
	"context"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"kubemono/internal/render"
)

const ns = "kube-system"

func saObj(labels map[string]string) render.Object {
	return render.NewClusterObject("ServiceAccount", ns, "kube-router", &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-router", Namespace: ns, Labels: labels},
	})
}

func cmObj(data map[string]string) render.Object {
	return render.NewClusterObject("ConfigMap", ns, "kube-router", &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-router", Namespace: ns},
		Data:       data,
	})
}

func crbObj(subject string) render.Object {
	return render.NewClusterObject("ClusterRoleBinding", "", "kube-router", &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-router"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "system:node-proxier"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: subject, Namespace: ns}},
	})
}

// dsObjWithHash 构造一个带 Pod 模板注解（kubemono.io/config-hash）的 DaemonSet，
// 用来复现"只有配置哈希变化、其余字段都没变"的场景。
func dsObjWithHash(image, hash string) render.Object {
	return render.NewClusterObject("DaemonSet", ns, "kube-router", &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-router", Namespace: ns},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-router"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"k8s-app": "kube-router"},
					Annotations: map[string]string{"kubemono.io/config-hash": hash},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "kube-router", Image: image}},
				},
			},
		},
	})
}

func dsObj(image string) render.Object {
	return render.NewClusterObject("DaemonSet", ns, "kube-router", &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-router", Namespace: ns},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-router"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"k8s-app": "kube-router"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "kube-router", Image: image}},
				},
			},
		},
	})
}

// TestApplyIsIdempotent: 内容一致时不得写入（changed=false），
// 否则每次 install 都会产生一次无意义的 Update 与滚动。
// TestApplyRewritesDaemonSetWhenConfigHashChanges 守住"配置变了要滚动"：
// 只有 Pod 模板注解 kubemono.io/config-hash 变化时，Apply 必须判定 changed=true
// 并写回新注解，否则 kube-proxy/kube-router 会带着旧配置继续跑，而 Check 报"已完成"。
func TestApplyRewritesDaemonSetWhenConfigHashChanges(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	image := "registry.example/kube-router:v1"

	if changed, err := Apply(ctx, client, dsObjWithHash(image, "hash-old")); err != nil || !changed {
		t.Fatalf("首次 Apply = %v, %v，期望 true,nil", changed, err)
	}
	changed, err := Apply(ctx, client, dsObjWithHash(image, "hash-new"))
	if err != nil {
		t.Fatalf("第二次 Apply: %v", err)
	}
	if !changed {
		t.Fatal("只有 config-hash 变化时 Apply 也必须重写 DaemonSet（否则不会滚动更新）")
	}
	cur, err := client.AppsV1().DaemonSets(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cur.Spec.Template.Annotations["kubemono.io/config-hash"]; got != "hash-new" {
		t.Fatalf("config-hash = %q，期望 hash-new", got)
	}
}

// TestApplyRejectsChangedClusterIP 守住不可变字段：
// 期望 ClusterIP 与集群里不一致时必须明确报错，而不是静默保留旧值——
// 静默保留会让改 clusterDNS 后 kube-dns 仍指向旧 IP，集群 DNS 直接断掉。
func TestApplyRejectsChangedClusterIP(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	svc := func(ip string) render.Object {
		return render.NewClusterObject("Service", "kube-system", "kube-dns", &corev1.Service{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"},
			Spec: corev1.ServiceSpec{
				ClusterIP: ip,
				Ports:     []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}},
			},
		})
	}

	if _, err := Apply(ctx, client, svc("10.96.0.10")); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	if _, err := Apply(ctx, client, svc("10.96.0.10")); err != nil {
		t.Fatalf("相同 ClusterIP 必须幂等: %v", err)
	}
	_, err := Apply(ctx, client, svc("10.96.0.99"))
	if err == nil {
		t.Fatal("ClusterIP 变化必须报错，不能静默沿用旧值")
	}
	if !strings.Contains(err.Error(), "clusterIP") || !strings.Contains(err.Error(), "10.96.0.99") {
		t.Fatalf("错误信息应说明不可变字段与新期望值，实际: %v", err)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	for _, o := range []render.Object{saObj(nil), cmObj(map[string]string{"config.conf": "a"}), crbObj("kube-router"), dsObj("registry/kube-router:v1.31.4")} {
		changed, err := Apply(ctx, client, o)
		if err != nil {
			t.Fatalf("首次 Apply %s: %v", o.Ref(), err)
		}
		if !changed {
			t.Fatalf("首次 Apply %s 应报告 changed=true", o.Ref())
		}
		changed, err = Apply(ctx, client, o)
		if err != nil {
			t.Fatalf("重复 Apply %s: %v", o.Ref(), err)
		}
		if changed {
			t.Fatalf("内容未变时 Apply %s 应报告 changed=false", o.Ref())
		}
	}
}

// TestApplyUpdatesChangedContent: 内容变化必须落盘（否则修复流程会静默保留旧配置）。
func TestApplyUpdatesChangedContent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	if _, err := Apply(ctx, client, cmObj(map[string]string{"config.conf": "old"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	changed, err := Apply(ctx, client, cmObj(map[string]string{"config.conf": "new"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Fatal("ConfigMap 内容变化应报告 changed=true")
	}
	got, err := client.CoreV1().ConfigMaps(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data["config.conf"] != "new" {
		t.Fatalf("ConfigMap 内容未更新: %v", got.Data)
	}
}

// TestApplyDaemonSetIgnoresStatusNoise: DaemonSet 的幂等比对只看 spec，
// status 由控制器写入，不能让它把对象判成"已变化"。
func TestApplyDaemonSetIgnoresStatusNoise(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	o := dsObj("registry/kube-router:v1.31.4")
	if _, err := Apply(ctx, client, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cur, err := client.AppsV1().DaemonSets(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	cur.Status.NumberReady = 3
	cur.Status.CurrentNumberScheduled = 3
	if _, err := client.AppsV1().DaemonSets(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update status: %v", err)
	}
	changed, err := Apply(ctx, client, o)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if changed {
		t.Fatal("只有 status 变化时不应重写 DaemonSet")
	}
}

// TestDeleteIsIdempotent: 对象不存在时删除必须成功（remove 可重复执行）。
func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	objs := []render.Object{saObj(nil), cmObj(nil), crbObj("kube-router"), dsObj("img")}
	for _, o := range objs {
		if _, err := Apply(ctx, client, o); err != nil {
			t.Fatalf("Apply %s: %v", o.Ref(), err)
		}
	}
	// 逆序删除（调用方的约定）。
	for _, obj := range slices.Backward(objs) {
		if err := Delete(ctx, client, obj); err != nil {
			t.Fatalf("Delete %s: %v", obj.Ref(), err)
		}
		if err := Delete(ctx, client, obj); err != nil {
			t.Fatalf("重复 Delete %s: %v", obj.Ref(), err)
		}
	}
	for _, o := range objs {
		if err := Exists(ctx, client, o); err == nil {
			t.Fatalf("%s 删除后仍存在", o.Ref())
		}
	}
}

// TestExists: 只读查询要给出与 Apply 相同的 kind 分派。
func TestExists(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	o := saObj(nil)
	if err := Exists(ctx, client, o); err == nil {
		t.Fatal("尚未创建时应返回错误")
	}
	if _, err := Apply(ctx, client, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := Exists(ctx, client, o); err != nil {
		t.Fatalf("创建后 Exists 应成功: %v", err)
	}
}

// TestUnsupportedKind: 未知类型必须明确报错，而不是被当成无操作跳过。
func TestUnsupportedKind(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	o := render.NewClusterObject("Pod", ns, "x", &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns},
	})
	if _, err := Apply(ctx, client, o); err == nil || !strings.Contains(err.Error(), "不支持的集群对象类型") {
		t.Fatalf("Apply err = %v", err)
	}
	if err := Delete(ctx, client, o); err == nil || !strings.Contains(err.Error(), "不支持的集群对象类型") {
		t.Fatalf("Delete err = %v", err)
	}
	if err := Exists(ctx, client, o); err == nil || !strings.Contains(err.Error(), "不支持的集群对象类型") {
		t.Fatalf("Exists err = %v", err)
	}
}

// TestNilClient: 没有客户端时必须报错，绝不能静默什么都不做。
func TestNilClient(t *testing.T) {
	ctx := context.Background()
	o := saObj(nil)
	if _, err := Apply(ctx, nil, o); err != ErrNoClient {
		t.Fatalf("Apply err = %v, want ErrNoClient", err)
	}
	if err := Delete(ctx, nil, o); err != ErrNoClient {
		t.Fatalf("Delete err = %v, want ErrNoClient", err)
	}
	if err := Exists(ctx, nil, o); err != ErrNoClient {
		t.Fatalf("Exists err = %v, want ErrNoClient", err)
	}
}

// TestRefs: 错误信息里的对象引用顺序必须稳定。
func TestRefs(t *testing.T) {
	objs := []render.Object{saObj(nil), crbObj("kube-router")}
	refs := Refs(objs)
	if len(refs) != 2 || refs[0] != "ServiceAccount/kube-system/kube-router" || refs[1] != "ClusterRoleBinding/kube-router" {
		t.Fatalf("refs = %v", refs)
	}
	if JoinRefs(objs) != strings.Join(refs, ", ") {
		t.Fatalf("JoinRefs = %q", JoinRefs(objs))
	}
	if len(Refs(nil)) != 0 {
		t.Fatal("空列表应返回空切片")
	}
}

func TestApplyIgnoresAPIDefaultedFields(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	o := dsObj("registry/kube-router:v1.31.4")
	if _, err := Apply(ctx, client, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cur, err := client.AppsV1().DaemonSets(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 apiserver 的默认化：desired 里没有 DNSPolicy，但存量对象有。
	cur.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	cur.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	if _, err := client.AppsV1().DaemonSets(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	changed, err := Apply(ctx, client, o)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("apiserver 补默认字段后不应每次都重写 DaemonSet")
	}
}

func TestMatchesUsesContentComparison(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	o := cmObj(map[string]string{"config.conf": "a"})
	if ok, err := Matches(ctx, client, o); err != nil || ok {
		t.Fatalf("对象不存在时 Matches = %v, %v，期望 false,nil", ok, err)
	}
	if _, err := Apply(ctx, client, o); err != nil {
		t.Fatal(err)
	}
	if ok, err := Matches(ctx, client, o); err != nil || !ok {
		t.Fatalf("刚 Apply 后 Matches = %v, %v，期望 true,nil", ok, err)
	}
	if ok, err := Matches(ctx, client, cmObj(map[string]string{"config.conf": "b"})); err != nil || ok {
		t.Fatalf("desired 变化后 Matches = %v, %v，期望 false,nil", ok, err)
	}
}

func TestMatchesDetectsOutOfBandConfigMapDrift(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	o := cmObj(map[string]string{"config.conf": "a"})
	if _, err := Apply(ctx, client, o); err != nil {
		t.Fatal(err)
	}
	cur, err := client.CoreV1().ConfigMaps(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Data["config.conf"] = "tampered"
	if _, err := client.CoreV1().ConfigMaps(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Matches(ctx, client, o); err != nil || ok {
		t.Fatalf("带外修改后 Matches = %v, %v，期望 false,nil", ok, err)
	}
}

func TestMatchesDetectsOutOfBandDaemonSetImageDrift(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	o := dsObj("registry.example/kube-router:v1")
	if _, err := Apply(ctx, client, o); err != nil {
		t.Fatal(err)
	}
	cur, err := client.AppsV1().DaemonSets(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Spec.Template.Spec.Containers[0].Image = "evil.example/kube-router:latest"
	if _, err := client.AppsV1().DaemonSets(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Matches(ctx, client, o); err != nil || ok {
		t.Fatalf("带外修改 image 后 Matches = %v, %v，期望 false,nil", ok, err)
	}
}

// TestApplyRepairsOutOfBandDrift: phase 的 Check 发现漂移后，Run 调用的 Apply
// 必须真的把内容改回来。只看 fingerprint 注解会漏掉带外 edit，
// 于是 Check 说"要修"、Apply 说"不用修"，命令以 0 退出但漂移永远存在。
func TestApplyRepairsOutOfBandDrift(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	cm := cmObj(map[string]string{"config.conf": "desired"})
	if _, err := Apply(ctx, client, cm); err != nil {
		t.Fatalf("首次 Apply ConfigMap: %v", err)
	}
	cur, err := client.CoreV1().ConfigMaps(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Data["config.conf"] = "tampered"
	if _, err := client.CoreV1().ConfigMaps(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Matches(ctx, client, cm); err != nil || ok {
		t.Fatalf("带外修改后 Matches = %v, %v，期望 false,nil", ok, err)
	}
	changed, err := Apply(ctx, client, cm)
	if err != nil {
		t.Fatalf("修复 Apply ConfigMap: %v", err)
	}
	if !changed {
		t.Fatal("带外漂移后 Apply 必须报告 changed=true")
	}
	got, err := client.CoreV1().ConfigMaps(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data["config.conf"] != "desired" {
		t.Fatalf("ConfigMap 漂移未被修复: %q", got.Data["config.conf"])
	}

	ds := dsObj("registry.example/kube-router:v1")
	if _, err := Apply(ctx, client, ds); err != nil {
		t.Fatalf("首次 Apply DaemonSet: %v", err)
	}
	curDS, err := client.AppsV1().DaemonSets(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	curDS.Spec.Template.Spec.Containers[0].Image = "evil.example/kube-router:latest"
	if _, err := client.AppsV1().DaemonSets(ns).Update(ctx, curDS, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Matches(ctx, client, ds); err != nil || ok {
		t.Fatalf("带外修改 image 后 Matches = %v, %v，期望 false,nil", ok, err)
	}
	changed, err = Apply(ctx, client, ds)
	if err != nil {
		t.Fatalf("修复 Apply DaemonSet: %v", err)
	}
	if !changed {
		t.Fatal("DaemonSet image 漂移后 Apply 必须报告 changed=true")
	}
	gotDS, err := client.AppsV1().DaemonSets(ns).Get(ctx, "kube-router", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if gotDS.Spec.Template.Spec.Containers[0].Image != "registry.example/kube-router:v1" {
		t.Fatalf("DaemonSet image 漂移未被修复: %q", gotDS.Spec.Template.Spec.Containers[0].Image)
	}
}
