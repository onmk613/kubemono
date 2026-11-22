package clusterapply

import (
	"context"
	"fmt"

	"kubemono/internal/render"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// adapter 把一个 kind 的 CRUD 收敛为闭包；所有 kind 共用同一套
// get → compare → create/update 流程。
type adapter struct {
	get    func(ctx context.Context, name string) (metav1.Object, error)
	create func(ctx context.Context, obj metav1.Object) error
	update func(ctx context.Context, obj metav1.Object) error
	delete func(ctx context.Context, name string) error
}

// resolved 是一次"按 render.Object 定位集群对象"的结果。
// Apply / Matches / Exists 共用同一段前导（取 Data、按 kind 解码、找 adapter、
// Get 现行对象），避免各写一份导致漂移（例如 Service 的 ClusterIP 处理
// 只加在 Apply 里，Matches 于是永远报漂移）。
type resolved struct {
	desired metav1.Object
	adapter adapter
	live    metav1.Object
	found   bool
}

// resolve 把 render.Object 解析成期望对象 + adapter + 现行对象。
// 对象不存在不算错误，用 found=false 表示。
func resolve(ctx context.Context, client kubernetes.Interface, o render.Object) (*resolved, error) {
	if client == nil {
		return nil, ErrNoClient
	}
	data, err := o.Data()
	if err != nil {
		return nil, err
	}
	desired, err := decodeByKind(o.Kind(), data)
	if err != nil {
		return nil, err
	}
	a, err := adapterFor(client, o.Kind(), o.Namespace())
	if err != nil {
		return nil, err
	}
	live, err := a.get(ctx, o.Name())
	if apierrors.IsNotFound(err) {
		return &resolved{desired: desired, adapter: a}, nil
	}
	if err != nil {
		return nil, err
	}
	return &resolved{desired: desired, adapter: a, live: live, found: true}, nil
}

// Apply 创建或更新一个集群级对象。
func Apply(ctx context.Context, client kubernetes.Interface, o render.Object) (bool, error) {
	r, err := resolve(ctx, client, o)
	if err != nil {
		return false, err
	}
	if !r.found {
		return true, r.adapter.create(ctx, r.desired)
	}
	// Service 的 ClusterIP 是不可变字段：更新必须沿用已有值，否则 apiserver 会
	// 直接拒绝。但"沿用"不能是静默覆盖——那会让配置里的新 IP 被悄悄丢掉
	// （改 network.clusterDNS 时 kube-dns 仍指向旧 IP，而 kubelet 已指向新 IP，
	// 集群 DNS 全断，命令还成功退出）。两边都有值时不一致就明确报错，
	// 让操作者决定重建 Service 还是改回配置。
	if err := reconcileImmutableFields(r.desired, r.live); err != nil {
		return false, err
	}
	if !needsWrite(r.live, r.desired) {
		return false, nil
	}
	r.desired.SetResourceVersion(r.live.GetResourceVersion())
	return true, r.adapter.update(ctx, r.desired)
}

// reconcileImmutableFields 把不可变字段对齐到集群现有值；无法对齐时报错。
func reconcileImmutableFields(desired, live metav1.Object) error {
	svc, ok := desired.(*corev1.Service)
	if !ok {
		return nil
	}
	liveSvc, ok := live.(*corev1.Service)
	if !ok {
		return nil
	}
	if svc.Spec.ClusterIP != "" && liveSvc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != liveSvc.Spec.ClusterIP {
		return fmt.Errorf(
			"service %s/%s 的 clusterIP 不可变（期望 %s，集群里是 %s）：请先删除该 service 后重跑，或把配置改回原值",
			svc.Namespace, svc.Name, svc.Spec.ClusterIP, liveSvc.Spec.ClusterIP)
	}
	svc.Spec.ClusterIP = liveSvc.Spec.ClusterIP
	svc.Spec.ClusterIPs = liveSvc.Spec.ClusterIPs
	return nil
}

// adapterForObject 返回处理该对象的 adapter，并挡住 nil client。
// 只做 CRUD/存在性检查的入口共用它，避免每处重复写"nil 检查 + adapterFor"。
func adapterForObject(client kubernetes.Interface, o render.Object) (adapter, error) {
	if client == nil {
		return adapter{}, ErrNoClient
	}
	return adapterFor(client, o.Kind(), o.Namespace())
}

// Delete 删除一个对象，忽略 "not found"。
func Delete(ctx context.Context, client kubernetes.Interface, o render.Object) error {
	a, err := adapterForObject(client, o)
	if err != nil {
		return err
	}
	return ignoreNotFound(a.delete(ctx, o.Name()))
}

// Exists 报告对象是否存在（只读）。
// 返回原始错误：调用方用 apierrors.IsNotFound 区分"不存在"（预期的未完成）
// 与"读失败"（如 Forbidden，应该报错而不是假装未完成反复重跑）。
func Exists(ctx context.Context, client kubernetes.Interface, o render.Object) error {
	a, err := adapterForObject(client, o)
	if err != nil {
		return err
	}
	_, err = a.get(ctx, o.Name())
	return err
}

// Matches 报告现有对象是否与期望文档一致（只读）。
func Matches(ctx context.Context, client kubernetes.Interface, o render.Object) (bool, error) {
	r, err := resolve(ctx, client, o)
	if err != nil {
		return false, err
	}
	if !r.found {
		return false, nil
	}
	// 与 Apply 走同一套不可变字段处理，否则 Check 与 Run 会得出相反结论。
	if err := reconcileImmutableFields(r.desired, r.live); err != nil {
		return false, err
	}
	return !needsWrite(r.live, r.desired), nil
}

func ignoreNotFound(err error) error {
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// resourceClient 是 client-go typed client 的公共 CRUD 子集。
// 10 种对象共用同一个 adapter 工厂，区别只在 client 构造与类型参数。
type resourceClient[T any] interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*T, error)
	Create(ctx context.Context, obj *T, opts metav1.CreateOptions) (*T, error)
	Update(ctx context.Context, obj *T, opts metav1.UpdateOptions) (*T, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// newAdapter 把一个 typed client 收敛成统一的 adapter。
func newAdapter[T any](api resourceClient[T]) adapter {
	return adapter{
		get: func(ctx context.Context, name string) (metav1.Object, error) {
			obj, err := api.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return asObject(obj)
		},
		create: func(ctx context.Context, obj metav1.Object) error {
			typed, ok := any(obj).(*T)
			if !ok {
				return fmt.Errorf("对象类型不匹配：期望 *%T，实际 %T", new(T), obj)
			}
			_, err := api.Create(ctx, typed, metav1.CreateOptions{})
			return err
		},
		update: func(ctx context.Context, obj metav1.Object) error {
			typed, ok := any(obj).(*T)
			if !ok {
				return fmt.Errorf("对象类型不匹配：期望 *%T，实际 %T", new(T), obj)
			}
			_, err := api.Update(ctx, typed, metav1.UpdateOptions{})
			return err
		},
		delete: func(ctx context.Context, name string) error {
			return api.Delete(ctx, name, metav1.DeleteOptions{})
		},
	}
}

// asObject 把 typed 指针转换成 metav1.Object。
func asObject[T any](obj *T) (metav1.Object, error) {
	out, ok := any(obj).(metav1.Object)
	if !ok {
		return nil, fmt.Errorf("类型 %T 不是 Kubernetes 对象", obj)
	}
	return out, nil
}

// decodeObject 把 YAML 解码成 typed 对象。
func decodeObject[T any](kind string, data []byte) (metav1.Object, error) {
	obj := new(T)
	if err := yaml.Unmarshal(data, obj); err != nil {
		return nil, fmt.Errorf("解析 %s 失败：%w", kind, err)
	}
	return asObject(obj)
}

// registration 同时登记解码与 CRUD 构造，新增对象类型只改这一处。
type registration struct {
	decode  func([]byte) (metav1.Object, error)
	adapter func(kubernetes.Interface, string) (adapter, error)
}

func registerKind[T any](kind string, client func(kubernetes.Interface, string) resourceClient[T]) registration {
	return registration{
		decode: func(data []byte) (metav1.Object, error) { return decodeObject[T](kind, data) },
		adapter: func(c kubernetes.Interface, ns string) (adapter, error) {
			return newAdapter(client(c, ns)), nil
		},
	}
}

var objectRegistry = map[string]registration{
	"Namespace": registerKind("Namespace", func(c kubernetes.Interface, _ string) resourceClient[corev1.Namespace] {
		return c.CoreV1().Namespaces()
	}),
	"ServiceAccount": registerKind("ServiceAccount", func(c kubernetes.Interface, ns string) resourceClient[corev1.ServiceAccount] {
		return c.CoreV1().ServiceAccounts(ns)
	}),
	"ConfigMap": registerKind("ConfigMap", func(c kubernetes.Interface, ns string) resourceClient[corev1.ConfigMap] {
		return c.CoreV1().ConfigMaps(ns)
	}),
	"Service": registerKind("Service", func(c kubernetes.Interface, ns string) resourceClient[corev1.Service] {
		return c.CoreV1().Services(ns)
	}),
	"Deployment": registerKind("Deployment", func(c kubernetes.Interface, ns string) resourceClient[appsv1.Deployment] {
		return c.AppsV1().Deployments(ns)
	}),
	"DaemonSet": registerKind("DaemonSet", func(c kubernetes.Interface, ns string) resourceClient[appsv1.DaemonSet] {
		return c.AppsV1().DaemonSets(ns)
	}),
	"ClusterRole": registerKind("ClusterRole", func(c kubernetes.Interface, _ string) resourceClient[rbacv1.ClusterRole] {
		return c.RbacV1().ClusterRoles()
	}),
	"ClusterRoleBinding": registerKind("ClusterRoleBinding", func(c kubernetes.Interface, _ string) resourceClient[rbacv1.ClusterRoleBinding] {
		return c.RbacV1().ClusterRoleBindings()
	}),
	"Role": registerKind("Role", func(c kubernetes.Interface, ns string) resourceClient[rbacv1.Role] {
		return c.RbacV1().Roles(ns)
	}),
	"RoleBinding": registerKind("RoleBinding", func(c kubernetes.Interface, ns string) resourceClient[rbacv1.RoleBinding] {
		return c.RbacV1().RoleBindings(ns)
	}),
}

// adapterFor 返回某个 kind 的 CRUD 闭包。
func adapterFor(client kubernetes.Interface, kind, ns string) (adapter, error) {
	reg, ok := objectRegistry[kind]
	if !ok {
		return adapter{}, fmt.Errorf("不支持的集群对象类型 %s", kind)
	}
	return reg.adapter(client, ns)
}

// decodeByKind 按 kind 解码对象。
func decodeByKind(kind string, data []byte) (metav1.Object, error) {
	reg, ok := objectRegistry[kind]
	if !ok {
		return nil, fmt.Errorf("不支持的集群对象类型 %s", kind)
	}
	return reg.decode(data)
}
