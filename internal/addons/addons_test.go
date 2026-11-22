package addons

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
)

func testCluster() *clusterconfig.Cluster {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}}}
	clusterconfig.SetDefaults(c)
	return c
}

func TestAllHonoursEnabledFlags(t *testing.T) {
	cfg := testCluster()
	images := map[string]string{constants.ComponentCoreDNS: "registry.k8s.io/coredns/coredns:v1.11.3"}
	list, err := All(cfg, images)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name() != constants.ComponentCoreDNS {
		t.Fatalf("默认应只有一个 coredns，实际 %v", list)
	}
	// 关闭后为空，且不报错。
	off := false
	cfg.Addons.CoreDNS.Enabled = &off
	if list, err = All(cfg, images); err != nil || len(list) != 0 {
		t.Fatalf("关闭应返回空列表: %v %v", list, err)
	}
}

// typedObject 从渲染结果里取出一个 typed 对象（与 clusterapply 的解码方式一致）。
func typedObject[T any](t *testing.T, set *render.ObjectSet, kind, name string) *T {
	t.Helper()
	o, ok := set.Find(kind, name)
	if !ok {
		t.Fatalf("渲染结果里缺少 %s/%s；实际：%v", kind, name, set.Refs())
	}
	data, err := o.Data()
	if err != nil {
		t.Fatalf("%s/%s Data: %v", kind, name, err)
	}
	var out T
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析 %s/%s: %v", kind, name, err)
	}
	return &out
}

func TestCoreDNSCorefile(t *testing.T) {
	cfg := testCluster()
	cfg.Network.ClusterDNS = "10.96.0.10"
	cfg.Network.DNSDomain = "cluster.local"
	cfg.Addons.CoreDNS.Upstream = []string{"1.1.1.1", "8.8.8.8"}
	cfg.Addons.CoreDNS.Hosts = []string{"10.0.0.9 registry.local"}
	list, err := All(cfg, map[string]string{constants.ComponentCoreDNS: "img"})
	if err != nil {
		t.Fatal(err)
	}
	core := list[0]
	set, err := core.Objects(context.Background(), nil)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	cm := typedObject[corev1.ConfigMap](t, set, "ConfigMap", "coredns")
	svc := typedObject[corev1.Service](t, set, "Service", "kube-dns")
	dep := typedObject[appsv1.Deployment](t, set, "Deployment", "coredns")
	corefile := cm.Data["Corefile"]
	for _, want := range []string{
		CoreDNSBlockBegin, CoreDNSBlockEnd, "kubernetes cluster.local",
		"forward . 1.1.1.1 8.8.8.8", "hosts {", "10.0.0.9 registry.local", "prometheus :9153",
	} {
		if !strings.Contains(corefile, want) {
			t.Errorf("Corefile 缺少 %q:\n%s", want, corefile)
		}
	}
	// Service 的 ClusterIP 必须是 kubelet 配置里公布的 clusterDNS。
	if svc.Spec.ClusterIP != "10.96.0.10" || svc.Name != "kube-dns" {
		t.Fatalf("kube-dns Service 不正确: %s %s", svc.Name, svc.Spec.ClusterIP)
	}
	// 副本数与镜像来自配置/BOM。
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Fatalf("副本数 = %v", dep.Spec.Replicas)
	}
	// 必须容忍控制面污点，否则单 master 集群里 coredns 会 Pending。
	if len(dep.Spec.Template.Spec.Tolerations) == 0 {
		t.Fatal("coredns 必须容忍控制面污点")
	}
}

// TestMergeCorefileKeepsUserExtensions 是本包最重要的一条契约。
func TestMergeCorefileKeepsUserExtensions(t *testing.T) {
	managed := CoreDNSBlockBegin + "\n.:53 {\n    forward . 1.1.1.1\n}\n" + CoreDNSBlockEnd + "\n"
	cases := []struct {
		name     string
		existing string
		want     []string
		notWant  []string
		wantErr  bool
	}{
		{"全新安装", "", []string{CoreDNSBlockBegin, "1.1.1.1"}, nil, false},
		{
			name:     "用户扩展段保留",
			existing: managed + "\nimport /etc/coredns/custom/*.server\n",
			want:     []string{"import /etc/coredns/custom/*.server", CoreDNSBlockBegin},
		},
		{
			name:     "旧受管区块被替换",
			existing: CoreDNSBlockBegin + "\n.:53 {\n    forward . 9.9.9.9\n}\n" + CoreDNSBlockEnd + "\n# 用户备注\n",
			want:     []string{"1.1.1.1", "# 用户备注"},
			notWant:  []string{"9.9.9.9"},
		},
		{
			name:     "用户扩展只监听其它端口",
			existing: ":5353 {\n    whoami\n}\n",
			want:     []string{":5353", CoreDNSBlockBegin},
		},
		{
			name:     "旧 Corefile 已声明 .:53 但没有标记",
			existing: ".:53 {\n    forward . 114.114.114.114\n}\n",
			wantErr:  true,
		},
		{
			name:     "标记不完整",
			existing: CoreDNSBlockBegin + "\n.:53 {\n    forward . 1.1.1.1\n}\n",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MergeCorefile(tc.existing, managed)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当拒绝合并，实际得到:\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MergeCorefile: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("合并结果缺少 %q:\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("合并结果不应包含 %q:\n%s", nw, got)
				}
			}
		})
	}
}

func containerArgs(t *testing.T, c CoreDNS) string {
	t.Helper()
	set, err := c.Objects(context.Background(), nil)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	dep := typedObject[appsv1.Deployment](t, set, "Deployment", "coredns")
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("Deployment 没有容器")
	}
	return strings.Join(dep.Spec.Template.Spec.Containers[0].Args, " ")
}

// TestObjectsPreservesLiveState 守住"渲染时合并必须保留的集群现状"：
//   - ServiceAccount 的 Secrets/ImagePullSecrets 由 apiserver 填写，不能被清空；
//   - Corefile 里 kubemono 受管区块之外是用户扩展，必须保留。
//
// 这两件事属于"期望状态怎么算"，因此由渲染侧合并；写入仍只走 clusterapply。
func TestObjectsPreservesLiveState(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	liveSA := &corev1.ServiceAccount{
		ObjectMeta:       metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace},
		Secrets:          []corev1.ObjectReference{{Name: "coredns-token-abcde"}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
	}
	liveCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace},
		Data:       map[string]string{"Corefile": CoreDNSBlockBegin + "\n.:53 {}\n" + CoreDNSBlockEnd + "\nimport /etc/coredns/custom/*.server\n"},
	}
	if _, err := client.CoreV1().ServiceAccounts(constants.KubeSystemNamespace).Create(ctx, liveSA, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Create(ctx, liveCM, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	list, err := All(testCluster(), map[string]string{constants.ComponentCoreDNS: "img"})
	if err != nil {
		t.Fatal(err)
	}
	set, err := list[0].Objects(ctx, client)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	sa := typedObject[corev1.ServiceAccount](t, set, "ServiceAccount", "coredns")
	if len(sa.Secrets) != 1 || sa.Secrets[0].Name != "coredns-token-abcde" {
		t.Fatalf("SA 的 Secrets 未被保留: %+v", sa.Secrets)
	}
	if len(sa.ImagePullSecrets) != 1 || sa.ImagePullSecrets[0].Name != "regcred" {
		t.Fatalf("SA 的 ImagePullSecrets 未被保留: %+v", sa.ImagePullSecrets)
	}
	cm := typedObject[corev1.ConfigMap](t, set, "ConfigMap", "coredns")
	if !strings.Contains(cm.Data["Corefile"], "import /etc/coredns/custom/*.server") {
		t.Fatalf("用户 Corefile 扩展段被覆盖:\n%s", cm.Data["Corefile"])
	}
	if !strings.Contains(cm.Data["Corefile"], CoreDNSBlockBegin) {
		t.Fatalf("缺少 kubemono 受管区块:\n%s", cm.Data["Corefile"])
	}
}

// TestDeploymentFollowsReplicas 校验副本数跟随配置渲染。
func TestDeploymentFollowsReplicas(t *testing.T) {
	cfg := testCluster()
	three := int32(3)
	cfg.Addons.CoreDNS.Replicas = &three
	list, err := All(cfg, map[string]string{constants.ComponentCoreDNS: "img"})
	if err != nil {
		t.Fatal(err)
	}
	set, err := list[0].Objects(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	dep := typedObject[appsv1.Deployment](t, set, "Deployment", "coredns")
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Fatalf("副本数应跟随配置变为 3，实际 %v", dep.Spec.Replicas)
	}
}

// TestCoreDNSProcessOptions 覆盖 coredns 仅有的两个进程级开关。
// CoreDNS 不开放通用 extraArgs：conf/version/plugins 会破坏 Corefile 或让容器退出。
func TestCoreDNSProcessOptions(t *testing.T) {
	cfg := testCluster()
	cfg.Addons.CoreDNS.LogFormat = "json"
	cfg.Addons.CoreDNS.Quiet = true
	list, err := All(cfg, map[string]string{constants.ComponentCoreDNS: "img"})
	if err != nil {
		t.Fatal(err)
	}
	args := containerArgs(t, list[0])
	for _, want := range []string{"-conf", "/etc/coredns/Corefile", "-log-format=json", "-quiet"} {
		if !strings.Contains(args, want) {
			t.Errorf("coredns args 缺少 %q: %s", want, args)
		}
	}
}

func TestCoreDNSPlacement(t *testing.T) {
	tests := []struct {
		name         string
		workers      int
		replicas     int32
		wantRequired bool
	}{
		{"single worker falls back to preferred", 1, 2, false},
		{"two workers spread", 2, 2, true},
		{"three workers spread", 3, 2, true},
		{"more replicas than workers falls back", 2, 3, false},
		{"one replica needs no spread", 3, 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCluster()
			cfg.Cluster.Hosts = []clusterconfig.Host{{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}}}
			for i := 1; i <= tc.workers; i++ {
				cfg.Cluster.Hosts = append(cfg.Cluster.Hosts, clusterconfig.Host{
					Name:    fmt.Sprintf("w%d", i),
					Address: fmt.Sprintf("10.0.0.%d", i+1),
					Roles:   []clusterconfig.Role{clusterconfig.RoleWorker},
				})
			}
			cfg.Addons.CoreDNS.Replicas = &tc.replicas
			list, err := All(cfg, map[string]string{constants.ComponentCoreDNS: "img"})
			if err != nil {
				t.Fatal(err)
			}
			core := list[0]
			if core.RequireSpread != tc.wantRequired {
				t.Fatalf("RequireSpread=%v, want %v", core.RequireSpread, tc.wantRequired)
			}
			set, err := core.Objects(context.Background(), nil)
			if err != nil {
				t.Fatalf("Objects: %v", err)
			}
			dep := typedObject[appsv1.Deployment](t, set, "Deployment", "coredns")
			anti := dep.Spec.Template.Spec.Affinity.PodAntiAffinity
			if tc.wantRequired {
				if len(anti.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
					t.Fatalf("多 worker 必须硬反亲和，实际 %+v", anti)
				}
				if len(anti.PreferredDuringSchedulingIgnoredDuringExecution) != 0 {
					t.Fatalf("硬反亲和时不应再带 preferred: %+v", anti)
				}
			} else if len(anti.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
				t.Fatalf("单 worker/副本过多时应退回 preferred: %+v", anti)
			}
			// NoExecute 不能容忍，否则节点 unreachable 时副本不会被驱逐。
			tols := dep.Spec.Template.Spec.Tolerations
			if len(tols) != 1 || tols[0].Effect != corev1.TaintEffectNoSchedule || tols[0].Operator != corev1.TolerationOpExists {
				t.Fatalf("CoreDNS 只应容忍所有 NoSchedule 污点，实际 %+v", tols)
			}
		})
	}
}
