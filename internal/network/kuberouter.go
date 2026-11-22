package network

import (
	"errors"
	"fmt"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/profile"
	"kubemono/internal/render"
	"kubemono/internal/util/fsutil"
)

// 集群级常量。kube-router 的 DaemonSet 是 的部署形态：hostNetwork、
// kubeconfig 指向 127.0.0.1:6443，因此不依赖 ClusterIP（避免鸡生蛋问题）。
const (
	kubeRouterName          = "kube-router"
	kubeRouterConfigMapName = "kube-router"
	kubeRouterSAName        = "kube-router"
	kubeRouterRoleName      = "kube-router"
	// kubeRouterArgsKey 是 ConfigMap 里承载 argv 快照的键。
	kubeRouterArgsKey = "args"
	// kubeRouterUser 是 kubeconfig 里的用户名（仅用于展示：身份来自 SA token）。
	kubeRouterUser = "kube-router"
	// KubeRouterConfigDir 是 kube-router ConfigMap 在 Pod 内的挂载点；其中存放
	// ServiceAccount 的 kubeconfig。
	KubeRouterConfigDir = "/var/lib/kube-router-config"
)

// errNoCluster 表示在没有集群配置的情况下尝试渲染。
var errNoCluster = errors.New("缺少集群配置（*clusterconfig.Cluster）")

// kubeRouter 渲染 CNI/路由/网络策略。Service 代理固定由 kube-proxy 承担，
// kube-router 的 --run-service-proxy 永远为 false。
type kubeRouter struct{}

// kubeRouterCNIPlan 渲染 kube-router 的 CNI 部分：写入 /etc/cni/net.d 的
// conflist，以及集群级的 DaemonSet/RBAC/ConfigMap。
func kubeRouterCNIPlan(c *render.Context, assignedNodeCIDR string) (*Plan, error) {
	cfg := networkSpec(c)

	nodeCIDR, err := NodeCIDR(cfg.PodCIDR, maskSize(cfg), assignedNodeCIDR)
	if err != nil {
		return nil, err
	}
	hostMTU := c.HostMTU
	if hostMTU <= 0 {
		hostMTU = DefaultHostMTU
	}
	conflist, err := RenderCNIConfig(c, nodeCIDR, SuggestedMTU(c, hostMTU))
	if err != nil {
		return nil, err
	}
	plan := &Plan{}
	plan.Files = append(plan.Files, render.File{
		Path:      CNIConfigPath(c),
		Content:   fsutil.EnsureTrailingNewline(conflist),
		Mode:      0o644,
		Kind:      render.KindConf,
		Component: constants.ComponentKubeRouter,
		OnChange:  render.ActionNone,
	})

	kr := &kubeRouter{}
	objects, err := kr.Objects(c)
	if err != nil {
		return nil, err
	}
	plan.Objects = objects.Objects()
	plan.Healths = append(plan.Healths, kr.Health(c)...)
	return plan, nil
}

// Objects 构建 kube-router 的集群级对象；是否部署由 network.plugin 决定。
func (k *kubeRouter) Objects(c *render.Context) (*render.ObjectSet, error) {
	if !kubeRouterDeployed(c) {
		return &render.ObjectSet{}, nil
	}
	in, err := k.input(c)
	if err != nil {
		return nil, err
	}
	return in.objects(c.Release), nil
}

// Health 实现 Proxy。
func (k *kubeRouter) Health(*render.Context) []Health { return []Health{k.health()} }

// health 返回 kube-router 的本机健康端点。
func (k *kubeRouter) health() Health {
	return Health{
		Component: constants.ComponentKubeRouter,
		Scheme:    "http",
		Address:   fmt.Sprintf("127.0.0.1:%d", constants.KubeRouterHealthzPort),
		Path:      "/healthz",
	}
}

// clusterRole 返回 kube-router 的最小权限 ClusterRole。
// 资源清单来自 kube-router v2.4.1 的实际需求：它的 reflector 会 watch
// pods/endpoints/namespaces/networkpolicies/endpointslices；只给 nodes+services
// 时 DaemonSet 会因 "cannot list resource ..." 一直 CrashLoopBackOff，CNI 起不来。
func kubeRouterClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: kubeRouterRoleName, Labels: kubeRouterLabels()},
		// 最小权限：只读路由与 Service 需要的元数据，并写自己的 Event。
		// 不使用通配 verbs/resources（测试会断言这一点）。
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"nodes", "services", "pods", "endpoints", "namespaces"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"discovery.k8s.io"},
				Resources: []string{"endpointslices"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"networking.k8s.io"},
				Resources: []string{"networkpolicies"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "patch"},
			},
		},
	}
}

// input 渲染 kube-router 的 ConfigMap 内容与 Pod 定义，并算出配置摘要。
// hash 现在也包含 kubeconfig：apiserver 端点变化必须触发滚动。
func (k *kubeRouter) input(c *render.Context) (daemonSetInput, error) {
	data, err := k.flagsData(c)
	if err != nil {
		return daemonSetInput{}, err
	}
	p, err := render.LoadProfile(c)
	if err != nil {
		return daemonSetInput{}, err
	}
	flags, err := p.RenderFlags(constants.ComponentKubeRouter, data)
	if err != nil {
		return daemonSetInput{}, fmt.Errorf("渲染 kube-router 参数失败：%w", err)
	}
	argLine := strings.Join(flags, " ")
	kubeconfig := string(ServiceAccountKubeconfig(kubeRouterUser, daemonSetAPIServer(c)))
	hash := daemonSetHash(flags, argLine, kubeconfig)
	// .podspec 模板也会调用 flags 辅助函数，因此必须先把同一个 config-hash 放进去。
	data.ConfigHash = hash
	rendered, err := p.RenderTemplate(profile.TmplKubeRouterPodspec, data)
	if err != nil {
		return daemonSetInput{}, fmt.Errorf("渲染 kube-router Pod 定义失败：%w", err)
	}
	podSpec, err := parsePodSpec(rendered)
	if err != nil {
		return daemonSetInput{}, err
	}
	return daemonSetInput{
		Name:           kubeRouterName,
		Labels:         kubeRouterLabels(),
		ServiceAccount: kubeRouterSAName,
		BindingName:    kubeRouterRoleName,
		RoleRef:        kubeRouterRoleName,
		Role:           kubeRouterClusterRole(),
		ConfigKey:      kubeRouterArgsKey,
		Config:         argLine,
		Kubeconfig:     kubeconfig,
		Hash:           hash,
		PodSpec:        podSpec,
		Health:         k.health(),
	}, nil
}

// flagsData 构建 kube-router Pod 的模板数据。
func (k *kubeRouter) flagsData(c *render.Context) (*profile.KubeRouterFlags, error) {
	common, err := baseFlagsData(c)
	if err != nil {
		return nil, err
	}
	data := &profile.KubeRouterFlags{CommonFlags: common}
	if err := fillKubeRouter(data, c, networkSpec(c)); err != nil {
		return nil, err
	}
	return data, nil
}

// kubeRouterLabels 返回 kube-router 稳定的选择器标签。
func kubeRouterLabels() map[string]string {
	return map[string]string{"k8s-app": kubeRouterName, "app.kubernetes.io/name": kubeRouterName}
}

// fillKubeRouter 填充 kube-router 的模板数据。
// kube-router 只承担 CNI；--run-service-proxy 固定在模板里为 false，
// 不提供配置开关，避免它和 kube-proxy 同时写 Service 规则。
func fillKubeRouter(data *profile.KubeRouterFlags, c *render.Context, cfg clusterconfig.NetworkSpec) error {
	image, err := c.Image(constants.ComponentKubeRouter)
	if err != nil {
		return fmt.Errorf("无法解析组件 %s 的镜像：%w", constants.ComponentKubeRouter, err)
	}
	data.Binary = constants.ComponentKubeRouter
	data.Image = image
	data.ExtraArgs = cfg.KubeRouter.ExtraArgs
	data.KubeRouterRunRouter = boolValue(cfg.KubeRouter.RunRouter, true)
	data.KubeRouterRunFirewall = boolValue(cfg.KubeRouter.RunFirewall, true)
	// 容器内路径（ConfigMap 挂载点），不随测试根/数据目录重定位。
	data.KubeRouterKubeconfigFile = KubeRouterConfigDir + "/" + DaemonSetKubeconfigKey
	data.KubeRouterConfigMapName = kubeRouterConfigMapName
	// kube-router v2.4.1 的端口类 flag：健康检查 20244（附录 A），指标显式关闭
	// （没有消费方）。写成 --healthz-bind-address/--metrics-bind-address 会让
	// DaemonSet 因 unknown flag 崩溃（真机实测）。
	data.KubeRouterHealthPort = constants.KubeRouterHealthzPort
	data.KubeRouterMetricsPort = 0
	if cfg.KubeRouter.ClusterASN != nil {
		data.KubeRouterClusterASN = *cfg.KubeRouter.ClusterASN
	}
	data.KubeRouterEnableOverlay = boolValue(cfg.KubeRouter.EnableOverlay, true)
	data.KubeRouterOverlayType = cfg.KubeRouter.OverlayType
	if data.KubeRouterOverlayType == "" {
		data.KubeRouterOverlayType = "subnet"
	}
	return nil
}
