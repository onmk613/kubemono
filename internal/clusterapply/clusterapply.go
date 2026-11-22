// Package clusterapply 把 render.Object（渲染出来的集群级对象）幂等地应用到
// apiserver，并支持查询与删除。
//
// 为什么单独成包：`network`、`addon`、`cluster-init` 等 phase 都要写同一批对象，
// Check 也要只读地确认它们存在；而渲染层 internal/render 只能是纯数据（不能依赖
// client-go），所以"把对象写进集群"的这份唯一实现放在这里。
//
// 幂等性判据是对象内容（按 kind 做字段级比较；未知类型保守写入）：
// 内容没变就不写，因此同一个 phase 重复执行不会产生无意义的 Update。
package clusterapply

import (
	"errors"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"kubemono/internal/render"
)

// ErrNoClient 表示调用方没有提供 apiserver 客户端。
var ErrNoClient = errors.New("没有可用的 apiserver 客户端")

// Refs 返回一组对象的 ref（用于错误信息和日志）。
func Refs(objects []render.Object) []string {
	out := make([]string, 0, len(objects))
	for _, o := range objects {
		out = append(out, o.Ref())
	}
	return out
}

// JoinRefs 为错误信息渲染对象 ref。
func JoinRefs(objects []render.Object) string { return strings.Join(Refs(objects), ", ") }

// needsWrite 判断是否必须写入集群。
// 判据是内容比较：desired 与 live 在受管字段上一致就不写。没有比较器的类型
// 保守地写入（宁可多一次 Update，也不要把漂移当成已收敛）。
func needsWrite(cur, desired metav1.Object) bool {
	matched, comparable := liveContentMatches(cur, desired)
	return !comparable || !matched
}

// contentMatcher 比较同一 kind 的现行对象与期望对象。
// comparable=false 表示该类型没有可靠的内容比较，调用方保守地写入。
type contentMatcher func(cur, desired metav1.Object) (matched, comparable bool)

// typedContent 把 "同类型指针 + 内容相等函数" 收敛成 contentMatcher。
// 类型不匹配不算错误，只表示无法比较（例如 apiserver 返回了别的 kind）。
func typedContent[T any](equal func(cur, desired *T) bool) contentMatcher {
	return func(cur, desired metav1.Object) (bool, bool) {
		c, ok := any(cur).(*T)
		if !ok {
			return false, false
		}
		d, ok := any(desired).(*T)
		if !ok {
			return false, false
		}
		return equal(c, d), true
	}
}

// liveContentMatchers 是 kind → 比较器的唯一表。
// 新增对象类型只需要加一行，避免扩展 type-switch 时复制类型断言与返回值。
var liveContentMatchers = map[reflect.Type]contentMatcher{
	reflect.TypeOf(&corev1.ConfigMap{}): typedContent(func(c, d *corev1.ConfigMap) bool {
		return reflect.DeepEqual(c.Data, d.Data) && reflect.DeepEqual(c.BinaryData, d.BinaryData) &&
			mapContainsAll(c.Labels, d.Labels)
	}),
	reflect.TypeOf(&corev1.ServiceAccount{}): typedContent(func(c, d *corev1.ServiceAccount) bool {
		return reflect.DeepEqual(c.AutomountServiceAccountToken, d.AutomountServiceAccountToken) &&
			reflect.DeepEqual(c.ImagePullSecrets, d.ImagePullSecrets) &&
			mapContainsAll(c.Labels, d.Labels)
	}),
	reflect.TypeOf(&rbacv1.ClusterRole{}): typedContent(func(c, d *rbacv1.ClusterRole) bool {
		return reflect.DeepEqual(c.Rules, d.Rules) && mapContainsAll(c.Labels, d.Labels)
	}),
	reflect.TypeOf(&rbacv1.ClusterRoleBinding{}): typedContent(func(c, d *rbacv1.ClusterRoleBinding) bool {
		return reflect.DeepEqual(c.RoleRef, d.RoleRef) && reflect.DeepEqual(c.Subjects, d.Subjects) &&
			mapContainsAll(c.Labels, d.Labels)
	}),
	reflect.TypeOf(&appsv1.DaemonSet{}): typedContent(daemonSetContentMatches),
	reflect.TypeOf(&appsv1.Deployment{}): typedContent(func(c, d *appsv1.Deployment) bool {
		return apiequality.Semantic.DeepDerivative(d.Spec, c.Spec) && mapContainsAll(c.Labels, d.Labels)
	}),
	reflect.TypeOf(&corev1.Service{}): typedContent(func(c, d *corev1.Service) bool {
		return apiequality.Semantic.DeepDerivative(d.Spec, c.Spec) && mapContainsAll(c.Labels, d.Labels)
	}),
	reflect.TypeOf(&corev1.Namespace{}): typedContent(func(c, d *corev1.Namespace) bool {
		return mapContainsAll(c.Labels, d.Labels)
	}),
	reflect.TypeOf(&rbacv1.Role{}): typedContent(func(c, d *rbacv1.Role) bool {
		return reflect.DeepEqual(c.Rules, d.Rules) && mapContainsAll(c.Labels, d.Labels)
	}),
	reflect.TypeOf(&rbacv1.RoleBinding{}): typedContent(func(c, d *rbacv1.RoleBinding) bool {
		return reflect.DeepEqual(c.RoleRef, d.RoleRef) && reflect.DeepEqual(c.Subjects, d.Subjects) &&
			mapContainsAll(c.Labels, d.Labels)
	}),
}

func liveContentMatches(cur, desired metav1.Object) (matched, comparable bool) {
	matcher, ok := liveContentMatchers[reflect.TypeOf(desired)]
	if !ok {
		return false, false
	}
	return matcher(cur, desired)
}

// daemonSetContentMatches 比较 kubemono 实际管理的字段，忽略 apiserver 默认值
// （如 dnsPolicy/restartPolicy/imagePullPolicy）。这样能捕获 `kubectl edit` 改动
// 镜像/参数但 fingerprint 注解保持不变的情况。
func daemonSetContentMatches(cur, desired *appsv1.DaemonSet) bool {
	if cur == nil || desired == nil {
		return false
	}
	if !reflect.DeepEqual(cur.Spec.Selector, desired.Spec.Selector) {
		return false
	}
	if !mapContainsAll(cur.Labels, desired.Labels) {
		return false
	}
	if !mapContainsAll(cur.Spec.Template.Labels, desired.Spec.Template.Labels) {
		return false
	}
	// Pod 模板注解里带着 kubemono.io/config-hash：它是"配置变了要滚动"的唯一触发点
	// （见 internal/network/daemonset.go）。漏掉它，改 KubeProxyConfiguration 之类
	// 只进 ConfigMap 的字段时，ConfigMap 更新了、DaemonSet 却判定"无变化"，
	// 既不滚动也不报错，phase 的 Check 还会报"已完成"。
	if !mapContainsAll(cur.Spec.Template.Annotations, desired.Spec.Template.Annotations) {
		return false
	}
	if cur.Spec.Template.Spec.ServiceAccountName != desired.Spec.Template.Spec.ServiceAccountName {
		return false
	}
	return containersContentEqual(cur.Spec.Template.Spec.Containers, desired.Spec.Template.Spec.Containers)
}

func containersContentEqual(cur, desired []corev1.Container) bool {
	if len(cur) != len(desired) {
		return false
	}
	for i := range desired {
		if cur[i].Name != desired[i].Name || cur[i].Image != desired[i].Image {
			return false
		}
		if !stringSlicesEqual(cur[i].Command, desired[i].Command) ||
			!stringSlicesEqual(cur[i].Args, desired[i].Args) {
			return false
		}
		if !reflect.DeepEqual(cur[i].Env, desired[i].Env) ||
			!reflect.DeepEqual(cur[i].EnvFrom, desired[i].EnvFrom) ||
			!reflect.DeepEqual(cur[i].VolumeMounts, desired[i].VolumeMounts) ||
			!reflect.DeepEqual(cur[i].Resources, desired[i].Resources) {
			return false
		}
		if desired[i].ImagePullPolicy != "" && cur[i].ImagePullPolicy != desired[i].ImagePullPolicy {
			return false
		}
		if !reflect.DeepEqual(normalizeContainerPorts(cur[i].Ports), normalizeContainerPorts(desired[i].Ports)) {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func normalizeContainerPorts(in []corev1.ContainerPort) []corev1.ContainerPort {
	out := make([]corev1.ContainerPort, len(in))
	copy(out, in)
	for i := range out {
		if out[i].Protocol == "" {
			out[i].Protocol = corev1.ProtocolTCP
		}
	}
	return out
}

// mapContainsAll 报告 desired 里的每个键值在 cur 中都存在。
// 标签与注解的比较都用"子集"语义：外部控制器注入的额外键不判为漂移。
func mapContainsAll(cur, desired map[string]string) bool {
	for k, v := range desired {
		if cur[k] != v {
			return false
		}
	}
	return true
}
