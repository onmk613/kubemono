package network

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubemono/internal/constants"
	"kubemono/internal/render"
	"kubemono/internal/util/fsutil"
)

// daemonSetInput 描述一个"hostNetwork DaemonSet + ServiceAccount + RBAC +
// 内嵌 kubeconfig 的 ConfigMap + 健康端点"的组件。
//
// kube-proxy 与 kube-router 是同一套骨架：SA →（可选 ClusterRole）→
// ClusterRoleBinding → ConfigMap → DaemonSet → Health。两者共用同一套骨架；
// config-hash 的输入口径必须一致（kube-proxy 含 kubeconfig，kube-router 也必须含），
// 否则改 apiserver 端点时 kube-router 不会滚动。
type daemonSetInput struct {
	// Name 是组件名：DaemonSet 与 ConfigMap 都用它。
	Name string
	// Labels 是选择器标签。
	Labels map[string]string
	// ServiceAccount 是 SA 名（也是内嵌 kubeconfig 的用户名）。
	ServiceAccount string
	// BindingName 是 ClusterRoleBinding 的名字（可以与角色名不同，
	// 例如 kube-proxy 的绑定叫 kube-proxy、指向内置 system:node-proxier）。
	BindingName string
	// RoleRef 是 ClusterRoleBinding 指向的角色名。
	RoleRef string
	// Role 非 nil 时同时创建它（调用方负责填好 TypeMeta/ObjectMeta）。
	Role *rbacv1.ClusterRole
	// ConfigKey 是 ConfigMap 里承载组件内容的键（args 或 config.conf）。
	ConfigKey string
	// Config 是 ConfigMap 里那条内容。
	Config string
	// Kubeconfig 是内嵌的 ServiceAccount kubeconfig（DaemonSetKubeconfigKey）。
	Kubeconfig string
	// Hash 是触发滚动的配置摘要，见 daemonSetHash。
	Hash string
	// PodSpec 是渲染好的 Pod 定义（模板已内联 Hash）。
	PodSpec corev1.PodSpec
	// Health 是本机健康端点。
	Health Health
}

// daemonSetHash 计算"配置变了就滚动"的摘要。
// 输入必须覆盖这份 ConfigMap 的全部内容与 argv：只哈希 flags、漏掉 kubeconfig
// 会导致 apiserver 端点变化不触发滚动。
func daemonSetHash(flags []string, config, kubeconfig string) string {
	return fsutil.SHA256Bytes([]byte(strings.Join(flags, "\n") + "\n" + config + "\n" + kubeconfig))
}

// objects 组装 SA →（ClusterRole）→ ClusterRoleBinding → ConfigMap → DaemonSet。
func (in daemonSetInput) objects(release string) *render.ObjectSet {
	labels := ManagedLabels(in.Labels)
	sa := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: in.ServiceAccount, Namespace: constants.KubeSystemNamespace, Labels: labels},
	}
	// 内置 system:node-proxier 已包含所需权限；kubemono 只创建绑定，
	// 不复制一份可能与上游漂移的规则。
	binding := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: in.BindingName, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: in.RoleRef},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: in.ServiceAccount, Namespace: constants.KubeSystemNamespace},
		},
	}
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: constants.KubeSystemNamespace, Labels: labels},
		Data: map[string]string{
			in.ConfigKey: in.Config,
			"release":    release,
			// hostNetwork Pod 用自己的 SA token 访问本机 apiserver。
			DaemonSetKubeconfigKey: in.Kubeconfig,
		},
	}
	podSpec := in.PodSpec
	podSpec.ServiceAccountName = in.ServiceAccount
	// hostNetwork 下必须显式声明 DNS 策略；apiserver 可能通过 DNS 名访问。
	podSpec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	ds := newDaemonSet(in.Name, in.Labels, podSpec, in.Hash, 1)
	// selector 必须保持稳定（DaemonSet 的 selector 不可变）；managed-by 只加到
	// 对象与 Pod 模板的 metadata labels 上，供 prune 列出。
	ds.Labels = labels
	ds.Spec.Template.Labels = labels

	set := &render.ObjectSet{}
	set.Add(render.NewClusterObject("ServiceAccount", constants.KubeSystemNamespace, in.ServiceAccount, sa))
	if in.Role != nil {
		in.Role.Labels = labels
		set.Add(render.NewClusterObject("ClusterRole", "", in.Role.Name, in.Role))
	}
	set.Add(
		render.NewClusterObject("ClusterRoleBinding", "", in.BindingName, binding),
		render.NewClusterObject("ConfigMap", constants.KubeSystemNamespace, in.Name, cm),
		render.NewClusterObject("DaemonSet", ds.Namespace, ds.Name, ds),
	)
	return set
}
