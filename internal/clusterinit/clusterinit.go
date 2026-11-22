// Package clusterinit 渲染并应用 init 节点一次性创建的集群级资源。
// 这里只做“声明式资源”的部分：引导 RBAC、集群状态 ConfigMap 与 cluster-info。
// 加入凭证（bootstrap token / certificateKey / 证书包）由 internal/join/token
// 在需要时单独签发，不属于 init 的声明式资源。
package clusterinit

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"kubemono/internal/clusterapply"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	joincerts "kubemono/internal/join/certs"
	"kubemono/internal/render"
	"kubemono/internal/util/fsutil"
)

// Options 是资源集的输入。
type Options struct {
	// Cluster 是正在安装的配置 generation。
	Cluster *clusterconfig.Cluster
	// ClusterID 是不可变的集群身份。
	ClusterID string
	// Release 是 kubemono release，例如 v1.31.4-km.1。
	Release string
	// PublishClusterID 表示本次渲染把 clusterID 写进 cluster-info。
	// 只有 init 节点为 true：repair 要保留 token create 写入的身份与签名；
	// 重新 init 则需要用新的 clusterID 覆盖旧身份。
	PublishClusterID bool
	// Paths 解析磁盘上的路径（读取 CA 用于 cluster-info）。
	Paths constants.Paths
	// CACertPEM 可选地注入集群 CA（测试用）。
	CACertPEM []byte
}

// ResourceSet 是 cluster-init 创建的全部内容。
// 命名空间不由 kubemono 创建：所有对象都落在 kube-system / kube-public。
type ResourceSet struct {
	ClusterRoles        []*rbacv1.ClusterRole
	ClusterRoleBindings []*rbacv1.ClusterRoleBinding
	Roles               []*rbacv1.Role
	RoleBindings        []*rbacv1.RoleBinding
	ConfigMaps          []*corev1.ConfigMap
}

// Build 渲染每一个集群级资源。
func Build(opts Options) (*ResourceSet, error) {
	if opts.Cluster == nil {
		return nil, fmt.Errorf("clusterinit: 集群配置为空")
	}
	if opts.ClusterID == "" {
		return nil, fmt.Errorf("clusterinit: clusterID 不能为空（它是集群身份的唯一依据）")
	}
	if opts.Release == "" {
		return nil, fmt.Errorf("clusterinit: release 不能为空")
	}
	rs := &ResourceSet{}
	rs.ClusterRoles = clusterRoles()
	rs.ClusterRoleBindings = clusterRoleBindings()
	rs.Roles = []*rbacv1.Role{}
	rs.RoleBindings = []*rbacv1.RoleBinding{}
	// master 加入者用 bootstrap token 读取加密证书包；worker 令牌不授予该权限。
	certRole, certBinding := joinCertsRole()
	rs.Roles = append(rs.Roles, certRole)
	rs.RoleBindings = append(rs.RoleBindings, certBinding)
	rs.ConfigMaps = []*corev1.ConfigMap{clusterStateConfigMap(opts)}
	// cluster-info 是加入方的信任起点：没有它，令牌就无法校验 CA。
	// 它要求集群 CA 已存在（certs phase 先跑）；CA 不可用时由调用方（phase）
	// 明确报错，而不是在这里失败——Build 需要能被纯单元测试驱动。
	if len(opts.CACertPEM) > 0 || fsutil.FileExists(opts.Paths.CertPath(constants.CACertName)) {
		ci, err := clusterInfoConfigMap(opts)
		if err != nil {
			return nil, err
		}
		rs.ConfigMaps = append(rs.ConfigMaps, ci)
	}
	return rs, nil
}

// clusterRoles 返回 kubemono 拥有的 ClusterRole。
func clusterRoles() []*rbacv1.ClusterRole {
	return []*rbacv1.ClusterRole{
		{
			// 引导令牌只需要读 cluster-info：这是"用令牌校验 CA"的机制基础。
			ObjectMeta: metav1.ObjectMeta{Name: "kubemono:bootstrap:cluster-info"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups:     []string{""},
				Resources:     []string{"configmaps"},
				ResourceNames: []string{constants.ClusterInfoName},
				Verbs:         []string{"get"},
			}},
		},
		{
			// 集群管理员：对外分发的 admin kubeconfig 用它（可撤销）。
			ObjectMeta: metav1.ObjectMeta{Name: "kubemono:cluster-admins"},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
				{NonResourceURLs: []string{"*"}, Verbs: []string{"*"}},
			},
		},
	}
}

// clusterRoleBindings 返回上面那些 ClusterRole 以及内置引导角色的绑定
func clusterRoleBindings() []*rbacv1.ClusterRoleBinding {
	out := []*rbacv1.ClusterRoleBinding{
		// 引导令牌：只允许发起 TLS Bootstrap（两个分组都不含其它权限）。
		binding("kubemono:bootstrap:worker", "system:node-bootstrapper", rbacv1.GroupKind, constants.GroupBootstrappersWorker),
		binding("kubemono:bootstrap:master", "system:node-bootstrapper", rbacv1.GroupKind, constants.GroupBootstrappersMaster),
		// 客户端证书自动审批：system:nodes 只能为自己申请（selfnodeclient）。
		binding("kubemono:nodeclient:autoapprove", "system:certificates.k8s.io:certificatesigningrequests:nodeclient",
			rbacv1.GroupKind, constants.GroupBootstrappersWorker),
		binding("kubemono:nodeclient:autoapprove-master", "system:certificates.k8s.io:certificatesigningrequests:nodeclient",
			rbacv1.GroupKind, constants.GroupBootstrappersMaster),
		binding("kubemono:selfnodeclient", "system:certificates.k8s.io:certificatesigningrequests:selfnodeclient",
			rbacv1.GroupKind, constants.GroupSystemNodes),
		// apiserver 访问 kubelet：把 apiserver-kubelet-client 绑到内置角色。
		binding("kubemono:apiserver-kubelet-admin", "system:kubelet-api-admin",
			rbacv1.UserKind, constants.UserAPIServerKubeletClient),
		// 引导分组读取 cluster-info（令牌校验 CA 的前提，）。
		binding("kubemono:bootstrap:cluster-info", "kubemono:bootstrap:cluster-info",
			rbacv1.GroupKind, constants.GroupBootstrappersWorker),
		binding("kubemono:bootstrap:cluster-info-master", "kubemono:bootstrap:cluster-info",
			rbacv1.GroupKind, constants.GroupBootstrappersMaster),
		// kubemono 管理员。
		binding("kubemono:cluster-admins", "cluster-admin", rbacv1.GroupKind, constants.GroupClusterAdmins),
	}
	return out
}

// joinCertsRole 允许 master 引导分组读取 kube-system/kubemono-certs（只读、单资源）。
func joinCertsRole() (*rbacv1.Role, *rbacv1.RoleBinding) {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "kubemono:bootstrap:certs", Namespace: constants.KubeSystemNamespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: []string{joincerts.SecretName},
			Verbs:         []string{"get"},
		}},
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kubemono:bootstrap:certs", Namespace: constants.KubeSystemNamespace},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     role.Name,
		},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: constants.GroupBootstrappersMaster},
		},
	}
	return role, rb
}

func binding(name, roleName, kind, subject string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind: kind, APIGroup: rbacv1.GroupName, Name: subject,
		}},
	}
}

// clusterStateConfigMap 渲染 kube-system/kubemono-cluster。
func clusterStateConfigMap(opts Options) *corev1.ConfigMap {
	spec := opts.Cluster
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.ClusterStateName,
			Namespace: constants.KubeSystemNamespace,
		},
		Data: map[string]string{
			"clusterID":    opts.ClusterID,
			"release":      opts.Release,
			"etcd":         spec.ControlPlane.Etcd.Mode(),
			"plugin":       spec.Network.Plugin,
			"serviceProxy": clusterconfig.ServiceProxyMode(spec),
			"podCIDR":      spec.Network.PodCIDR,
			"serviceCIDR":  spec.Network.ServiceCIDR,
			"dnsDomain":    spec.Network.DNSDomain,
			"clusterDNS":   spec.Network.ClusterDNS,
		},
	}
}

// preserveClusterInfo 保留已有 cluster-info 里期望没有的键。
// 重新执行 cluster-init（例如 init 或 repair）时只渲染静态部分，不能把
// token create 写入的 clusterID 及其 JWS、各令牌的 kubeconfig 签名抹掉；
// 否则后续 join 会报 "目标集群没有公布 clusterID"。
func preserveClusterInfo(ctx context.Context, client kubernetes.Interface, cms []*corev1.ConfigMap) error {
	for _, cm := range cms {
		if cm.Name != constants.ClusterInfoName {
			continue
		}
		existing, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(ctx, cm.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("读取 %s/%s 失败: %w", cm.Namespace, cm.Name, err)
		}
		for key, value := range existing.Data {
			if _, ok := cm.Data[key]; !ok {
				cm.Data[key] = value
			}
		}
	}
	return nil
}

// Names 返回集合里每个对象的名称（Check 与测试使用）。
func (rs *ResourceSet) Names() []string {
	var out []string
	for _, r := range rs.ClusterRoles {
		out = append(out, "clusterrole/"+r.Name)
	}
	for _, b := range rs.ClusterRoleBindings {
		out = append(out, "clusterrolebinding/"+b.Name)
	}
	for _, r := range rs.Roles {
		out = append(out, "role/"+r.Namespace+"/"+r.Name)
	}
	for _, b := range rs.RoleBindings {
		out = append(out, "rolebinding/"+b.Namespace+"/"+b.Name)
	}
	for _, c := range rs.ConfigMaps {
		out = append(out, "configmap/"+c.Namespace+"/"+c.Name)
	}
	sort.Strings(out)
	return out
}

// objects 返回 Check/Apply 共用的对象列表。
func (rs *ResourceSet) objects() []render.Object {
	out := make([]render.Object, 0,
		len(rs.ClusterRoles)+len(rs.ClusterRoleBindings)+
			len(rs.Roles)+len(rs.RoleBindings)+len(rs.ConfigMaps))
	for _, role := range rs.ClusterRoles {
		out = append(out, render.NewClusterObject("ClusterRole", "", role.Name, role))
	}
	for _, b := range rs.ClusterRoleBindings {
		out = append(out, render.NewClusterObject("ClusterRoleBinding", "", b.Name, b))
	}
	for _, role := range rs.Roles {
		out = append(out, render.NewClusterObject("Role", role.Namespace, role.Name, role))
	}
	for _, b := range rs.RoleBindings {
		out = append(out, render.NewClusterObject("RoleBinding", b.Namespace, b.Name, b))
	}
	for _, cm := range rs.ConfigMaps {
		out = append(out, render.NewClusterObject("ConfigMap", cm.Namespace, cm.Name, cm))
	}
	return out
}

// Check 表示每个资源是否已与期望内容一致。
// 不能只判存在：Apply 承诺"已存在的对象会被更新到期望状态"，若 Check 只看
// 存在性，RBAC/ConfigMap 漂移后 Run 会被永远跳过，node repair 不收敛。
// 内容比较统一走 clusterapply.Matches（与 Apply 的写入判据同源，含准入策略
// 等受管字段）；NotFound 是预期的"未完成"（false,nil 供续跑），其它错误
// （如 Forbidden、网络问题）必须上抛，避免权限问题表现为"每次重跑且无诊断"。
func (rs *ResourceSet) Check(ctx context.Context, client kubernetes.Interface) (bool, error) {
	if client == nil {
		return false, fmt.Errorf("clusterinit: 缺少 Kubernetes 客户端")
	}
	for _, o := range rs.objects() {
		// cluster-info 里有 token create 动态写入的 JWS/clusterID 键：只比较
		// 本包管理的 kubeconfig/release，不能因为多出令牌签名就判定漂移，
		// 否则每次 node repair 都会重跑 cluster-init。
		if o.Kind() == "ConfigMap" && o.Name() == constants.ClusterInfoName {
			matched, err := rs.clusterInfoMatches(ctx, client)
			if err != nil {
				return false, fmt.Errorf("cluster-init: 检查 %s 失败: %w", o.Ref(), err)
			}
			if !matched {
				return false, nil
			}
			continue
		}
		matched, err := clusterapply.Matches(ctx, client, o)
		if err != nil {
			return false, fmt.Errorf("cluster-init: 检查 %s 失败: %w", o.Ref(), err)
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

// clusterInfoMatches 只比较 cluster-init 管理的 cluster-info 键。
// 逐令牌 JWS 与集群身份由 token create 写入，属于其他组件的状态。
func (rs *ResourceSet) clusterInfoMatches(ctx context.Context, client kubernetes.Interface) (bool, error) {
	var desired *corev1.ConfigMap
	for _, cm := range rs.ConfigMaps {
		if cm.Name == constants.ClusterInfoName {
			desired = cm
			break
		}
	}
	if desired == nil {
		return true, nil
	}
	live, err := client.CoreV1().ConfigMaps(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for key, value := range desired.Data {
		if live.Data[key] != value {
			return false, nil
		}
	}
	return true, nil
}

// Apply 幂等地创建或更新每一个资源。
// 幂等是硬要求：`install` 可以在任意时刻重复执行，集群初始化不能因为
// "对象已存在" 而失败。已存在对象会被更新到期望状态（人工改动会被纠正）。
func (rs *ResourceSet) Apply(ctx context.Context, client kubernetes.Interface, report func(string, ...any)) error {
	if client == nil {
		return fmt.Errorf("clusterinit: 缺少 Kubernetes 客户端（需要 admin kubeconfig）")
	}
	if report == nil {
		report = func(string, ...any) {}
	}
	if err := preserveClusterInfo(ctx, client, rs.ConfigMaps); err != nil {
		return err
	}
	for _, o := range rs.objects() {
		changed, err := clusterapply.Apply(ctx, client, o)
		if err != nil {
			return fmt.Errorf("应用 %s %s: %w", o.Kind(), o.Name(), err)
		}
		if changed {
			report("已应用 %s %s", o.Kind(), o.Name())
		}
	}
	return nil
}

// Summary 渲染资源集的简短可读摘要。
func (rs *ResourceSet) Summary() string {
	kinds := map[string]int{}
	for _, name := range rs.Names() {
		kind, _, _ := strings.Cut(name, "/")
		kinds[kind]++
	}
	var parts []string
	for _, k := range []string{"clusterrole", "clusterrolebinding", "role", "rolebinding", "configmap"} {
		if n := kinds[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n))
		}
	}
	return strings.Join(parts, " ")
}
