// Package addons 渲染内置 addon：coredns。
package addons

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
)

func labels(name string) map[string]string {
	return map[string]string{"k8s-app": name}
}

// CoreDNSBlockBegin/End 界定 Corefile 中由 kubemono 管理的部分。
const (
	CoreDNSBlockBegin = "# BEGIN kubemono managed block"
	CoreDNSBlockEnd   = "# END kubemono managed block"
)

// CoreDNS 渲染 coredns addon。
type CoreDNS struct {
	// ClusterDNS 是每个 kubelet 指向的 Service IP。
	ClusterDNS string
	// Domain 是集群 DNS 域名，例如 cluster.local。
	Domain string
	// Replicas 是期望的副本数。
	Replicas int32
	// Upstream 是上游解析器（默认 /etc/resolv.conf）。
	Upstream []string
	// Hosts 是形式为 "IP host [alias]" 的条目。
	Hosts []string
	// Image 是 coredns 镜像引用。
	Image string
	// LogFormat 是 CoreDNS 进程日志格式（text/json）；空值交给 CoreDNS 默认。
	LogFormat string
	// Quiet 关闭 CoreDNS 启动时的版本/初始化输出。
	Quiet bool
	// RequireSpread 表示副本数不超过可调度 worker 数，必须按节点硬反亲和。
	// 单 worker 集群只能共置，退回 preferred，避免第二个副本 Pending。
	RequireSpread bool
}

// Name 返回组件名。
func (CoreDNS) Name() string { return constants.ComponentCoreDNS }

// DesiredReplicas 返回期望副本数（<=0 按 2 处理）。
func (c CoreDNS) DesiredReplicas() int32 {
	if c.Replicas <= 0 {
		return 2
	}
	return c.Replicas
}

// Objects 渲染该 addon 的全部对象（返回顺序即应用顺序）。
//
// client 用于合并"必须保留的集群现状"：ServiceAccount 的 Secrets/ImagePullSecrets
// 由 apiserver 填写，Corefile 里 kubemono 受管区块之外是用户扩展。这些属于
// "期望状态该长什么样"，所以放在渲染侧；写入仍然只走 clusterapply（唯一 apply 层）。
// client 为 nil 时按纯期望值渲染（删除遗留对象只需要对象身份）。
func (c CoreDNS) Objects(ctx context.Context, client kubernetes.Interface) (*render.ObjectSet, error) {
	sa := c.serviceAccount()
	cm := c.configMap()
	if client != nil {
		if err := preserveServiceAccount(ctx, client, sa); err != nil {
			return nil, err
		}
		if err := preserveCorefile(ctx, client, cm); err != nil {
			return nil, err
		}
	}
	ns := constants.KubeSystemNamespace
	set := &render.ObjectSet{}
	set.Add(
		render.NewClusterObject("ServiceAccount", ns, sa.Name, sa),
		render.NewClusterObject("ClusterRole", "", "system:coredns", c.clusterRole()),
		render.NewClusterObject("ClusterRoleBinding", "", "system:coredns", c.clusterRoleBinding()),
		render.NewClusterObject("ConfigMap", ns, cm.Name, cm),
		render.NewClusterObject("Deployment", ns, "coredns", c.deployment()),
		render.NewClusterObject("Service", ns, "kube-dns", c.service()),
	)
	return set, nil
}

// preserveServiceAccount 保留 apiserver 管理的 Secrets/ImagePullSecrets，
// 并合并已有 labels（其它控制器可能加过）。
func preserveServiceAccount(ctx context.Context, client kubernetes.Interface, sa *corev1.ServiceAccount) error {
	live, err := client.CoreV1().ServiceAccounts(sa.Namespace).Get(ctx, sa.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sa.Secrets = live.Secrets
	sa.ImagePullSecrets = live.ImagePullSecrets
	sa.Labels = mergeLabels(live.Labels, sa.Labels)
	return nil
}

// preserveCorefile 只替换 kubemono 受管区块，保留用户在 Corefile 里加的扩展段。
func preserveCorefile(ctx context.Context, client kubernetes.Interface, cm *corev1.ConfigMap) error {
	live, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	merged, err := MergeCorefile(live.Data["Corefile"], cm.Data["Corefile"])
	if err != nil {
		return err
	}
	cm.Data["Corefile"] = merged
	cm.Labels = mergeLabels(live.Labels, cm.Labels)
	return nil
}

// mergeLabels 把 src 合并进 dst（dst 为 nil 时新建）。
func mergeLabels(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (c CoreDNS) serviceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace, Labels: labels("kube-dns")},
	}
}

func (c CoreDNS) clusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "system:coredns", Labels: labels("kube-dns")},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"endpoints", "services", "pods", "namespaces"}, Verbs: []string{"list", "watch"}},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"list", "watch"}},
		},
	}
}

func (c CoreDNS) clusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "system:coredns", Labels: labels("kube-dns")},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:coredns"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "coredns", Namespace: constants.KubeSystemNamespace}},
	}
}

func (c CoreDNS) configMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace, Labels: labels("kube-dns")},
		Data:       map[string]string{"Corefile": c.corefile()},
	}
}

func (c CoreDNS) deployment() *appsv1.Deployment {
	replicas := c.Replicas
	if replicas <= 0 {
		replicas = 2
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: constants.KubeSystemNamespace, Labels: labels("kube-dns")},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels("kube-dns")},
			Strategy: appsv1.DeploymentStrategy{
				Type:          appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: intstrPtr(1)},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels("kube-dns")},
				Spec:       c.podSpec(),
			},
		},
	}
}

func (c CoreDNS) podSpec() corev1.PodSpec {
	// 只容忍 NoSchedule 污点（控制面/专用节点仍可跑 CoreDNS），但不容忍
	// NoExecute：否则节点 unreachable 后副本永远不会被驱逐，两个副本若恰好
	// 同节点，DNS 会一直不恢复。普通 Pod 的默认 NoExecute 容忍仍然生效。
	tolerations := []corev1.Toleration{{
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	term := corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: labels("kube-dns")},
		TopologyKey:   "kubernetes.io/hostname",
	}
	anti := &corev1.PodAntiAffinity{}
	if c.RequireSpread {
		anti.RequiredDuringSchedulingIgnoredDuringExecution = []corev1.PodAffinityTerm{term}
	} else {
		anti.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.WeightedPodAffinityTerm{{
			Weight:          100,
			PodAffinityTerm: term,
		}}
	}
	return corev1.PodSpec{
		PriorityClassName:        "system-cluster-critical",
		ServiceAccountName:       "coredns",
		Tolerations:              tolerations,
		Affinity:                 &corev1.Affinity{PodAntiAffinity: anti},
		Containers:               []corev1.Container{c.container()},
		Volumes:                  []corev1.Volume{c.configVolume()},
		DNSPolicy:                corev1.DNSDefault,
		HostNetwork:              false,
		DeprecatedServiceAccount: "coredns",
	}
}

// args 返回 CoreDNS 进程 argv：Corefile 路径 + 两个显式开关。
// 不开放通用 extraArgs：conf/version/plugins 等 flag 会破坏 Corefile 或让容器直接退出。
func (c CoreDNS) args() []string {
	args := []string{"-conf", "/etc/coredns/Corefile"}
	if c.LogFormat != "" {
		args = append(args, "-log-format="+c.LogFormat)
	}
	if c.Quiet {
		args = append(args, "-quiet")
	}
	return args
}

func (c CoreDNS) container() corev1.Container {
	return corev1.Container{
		Name:            "coredns",
		Image:           c.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            c.args(),
		Ports: []corev1.ContainerPort{
			{ContainerPort: 53, Name: "dns", Protocol: corev1.ProtocolUDP},
			{ContainerPort: 53, Name: "dns-tcp", Protocol: corev1.ProtocolTCP},
			{ContainerPort: 9153, Name: "metrics", Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config-volume", MountPath: "/etc/coredns", ReadOnly: true},
		},
		LivenessProbe:  probe("/health", 8080),
		ReadinessProbe: probe("/ready", 8181),
	}
}

func (c CoreDNS) configVolume() corev1.Volume {
	return corev1.Volume{
		Name: "config-volume",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "coredns"},
				Items:                []corev1.KeyToPath{{Key: "Corefile", Path: "Corefile"}},
			},
		},
	}
}

func (c CoreDNS) service() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: constants.KubeSystemNamespace, Labels: labels("kube-dns")},
		Spec: corev1.ServiceSpec{
			Selector:  labels("kube-dns"),
			ClusterIP: c.ClusterDNS,
			Ports: []corev1.ServicePort{
				{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)},
				{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(53)},
				{Name: "metrics", Port: 9153, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(9153)},
			},
		},
	}
}

func probe(path string, port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)}},
		InitialDelaySeconds: 60,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		SuccessThreshold:    1,
		FailureThreshold:    5,
	}
}

// corefile 渲染 Corefile 的受管区块。
func (c CoreDNS) corefile() string {
	upstream := c.Upstream
	if len(upstream) == 0 {
		upstream = []string{"/etc/resolv.conf"}
	}
	var b strings.Builder
	b.WriteString(CoreDNSBlockBegin + "\n")
	b.WriteString(".:53 {\n")
	fmt.Fprintf(&b, "    errors\n    health {\n        lameduck 5s\n    }\n    ready\n")
	fmt.Fprintf(&b, "    kubernetes %s in-addr.arpa ip6.arpa {\n", c.Domain)
	b.WriteString("        pods insecure\n        fallthrough in-addr.arpa ip6.arpa\n        ttl 30\n    }\n")
	b.WriteString("    prometheus :9153\n")
	fmt.Fprintf(&b, "    forward . %s {\n        max_concurrent 1000\n    }\n", strings.Join(upstream, " "))
	b.WriteString("    cache 30\n    loop\n    reload\n    loadbalance\n")
	if len(c.Hosts) > 0 {
		b.WriteString("    hosts {\n")
		hosts := append([]string(nil), c.Hosts...)
		sort.Strings(hosts)
		for _, h := range hosts {
			fmt.Fprintf(&b, "        %s\n", strings.TrimSpace(h))
		}
		b.WriteString("        fallthrough\n    }\n")
	}
	b.WriteString("}\n")
	b.WriteString(CoreDNSBlockEnd + "\n")
	return b.String()
}

// MergeCorefile 保留受管区块之外的用户扩展。
// 重新执行 addon phase 时必须保留用户对 Corefile 的扩展段：
//   - 没有受管区块且用户内容为空：结果就是受管区块；
//   - 没有受管区块、用户内容不声明 .:53：受管区块在前，用户扩展在后；
//   - 只有一个完整受管区块：只替换区块内内容；
//   - 标记缺失/重复/顺序错误，或旧 Corefile 已经声明 .:53：
//     返回错误而不是拼接。两个 .:53 server block 会让 CoreDNS 启动失败
//     （"zone already defined"），全集群 DNS 一起挂。
func MergeCorefile(existing, managed string) (string, error) {
	if existing == managed {
		return managed, nil
	}
	begins := strings.Count(existing, CoreDNSBlockBegin)
	ends := strings.Count(existing, CoreDNSBlockEnd)
	switch {
	case begins == 0 && ends == 0:
		user := strings.TrimSpace(existing)
		if user == "" {
			return managed, nil
		}
		if declaresManagedZone(user) {
			return "", fmt.Errorf("现有 Corefile 已有 .:53 区块但没有 %q 标记：拒绝合并（会产生重复 zone）。"+
				"请手工把 kubemono 受管段包进 BEGIN/END 标记，或移走现有 Corefile 后重跑 addon", CoreDNSBlockBegin)
		}
		return managed + "\n" + user + "\n", nil
	case begins == 1 && ends == 1:
		begin := strings.Index(existing, CoreDNSBlockBegin)
		end := strings.Index(existing, CoreDNSBlockEnd)
		if end < begin {
			return "", fmt.Errorf("解析 Corefile 失败：%q 在 %q 之前，标记顺序错误，拒绝合并", CoreDNSBlockEnd, CoreDNSBlockBegin)
		}
		before := existing[:begin]
		after := strings.TrimPrefix(existing[end+len(CoreDNSBlockEnd):], "\n")
		return before + managed + after, nil
	default:
		return "", fmt.Errorf("解析 Corefile 失败：受管区块标记不完整（BEGIN=%d，END=%d），拒绝合并，请手工修复后重跑 addon", begins, ends)
	}
}

// declaresManagedZone 判断内容里是否已经有 .:53 server block。
func declaresManagedZone(corefile string) bool {
	for _, line := range strings.Split(corefile, "\n") {
		line = strings.TrimSpace(line)
		if line == ".:53 {" || strings.HasPrefix(line, ".:53{") {
			return true
		}
	}
	return false
}

// All 返回集群配置中启用的 addon。
// workerCount 返回配置中会运行 kubelet 的节点数；纯控制面 master 不计入。
func workerCount(cfg *clusterconfig.Cluster) int {
	if cfg == nil {
		return 0
	}
	n := 0
	for i := range cfg.Cluster.Hosts {
		if cfg.Cluster.Hosts[i].IsWorker() {
			n++
		}
	}
	return n
}

func All(cfg *clusterconfig.Cluster, images map[string]string) ([]CoreDNS, error) {
	if cfg == nil {
		return nil, fmt.Errorf("addons: 缺少集群配置")
	}
	var out []CoreDNS
	if enabled(cfg.Addons.CoreDNS.Enabled) {
		replicas := int32(0)
		if cfg.Addons.CoreDNS.Replicas != nil {
			replicas = *cfg.Addons.CoreDNS.Replicas
		}
		effectiveReplicas := replicas
		if effectiveReplicas <= 0 {
			effectiveReplicas = 2
		}
		out = append(out, CoreDNS{
			ClusterDNS:    cfg.Network.ClusterDNS,
			Domain:        cfg.Network.DNSDomain,
			Replicas:      replicas,
			Upstream:      cfg.Addons.CoreDNS.Upstream,
			Hosts:         cfg.Addons.CoreDNS.Hosts,
			Image:         images[constants.ComponentCoreDNS],
			LogFormat:     cfg.Addons.CoreDNS.LogFormat,
			Quiet:         cfg.Addons.CoreDNS.Quiet,
			RequireSpread: effectiveReplicas > 1 && int(effectiveReplicas) <= workerCount(cfg),
		})
	}
	return out, nil
}

// Disabled 返回配置中明确关闭的内置 addon。
// addon phase 用这些对象做“关闭后删除旧对象”的期望集合；镜像字段留空即可，
// 删除路径只需要 Kind/Namespace/Name。
func Disabled(cfg *clusterconfig.Cluster) []CoreDNS {
	if cfg == nil {
		return nil
	}
	var out []CoreDNS
	if !enabled(cfg.Addons.CoreDNS.Enabled) {
		replicas := int32(0)
		if cfg.Addons.CoreDNS.Replicas != nil {
			replicas = *cfg.Addons.CoreDNS.Replicas
		}
		out = append(out, CoreDNS{
			ClusterDNS: cfg.Network.ClusterDNS,
			Domain:     cfg.Network.DNSDomain,
			Replicas:   replicas,
			Upstream:   cfg.Addons.CoreDNS.Upstream,
			Hosts:      cfg.Addons.CoreDNS.Hosts,
		})
	}
	return out
}

func enabled(v *bool) bool { return v != nil && *v }

func intstrPtr(v int32) *intstr.IntOrString { s := intstr.FromInt32(v); return &s }
