package network

import (
	"fmt"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/profile"
	"kubemono/internal/render"
)

// kube-proxy 的集群级对象名。Service 代理统一由 kube-proxy 承担：
// kube-router 只做 CNI，不用 --run-service-proxy 自动替换 kube-proxy。
const (
	kubeProxyName          = "kube-proxy"
	kubeProxyConfigMapName = "kube-proxy"
	kubeProxySAName        = "kube-proxy"
	kubeProxyBindingName   = "kube-proxy"
	// kubeProxyClusterRoleName 是 apiserver 引导时创建的内置 ClusterRole。
	kubeProxyClusterRoleName = "system:node-proxier"
	// kubeProxyUser 是 kubeconfig 里的用户名（身份来自 SA token）。
	kubeProxyUser = "kube-proxy"
	// KubeProxyConfigDir 是 kube-proxy ConfigMap 在 Pod 内的挂载点。
	KubeProxyConfigDir = "/var/lib/kube-proxy"
	// KubeProxyConfigKey 是 KubeProxyConfiguration 在 ConfigMap 中的键。
	KubeProxyConfigKey = "config.conf"
)

// kubeProxy 渲染 kube-proxy 的 ConfigMap/RBAC/DaemonSet。
type kubeProxy struct{}

// kubeProxyPlan 渲染 kube-proxy 的集群级对象与健康端点。
// 它在所有节点上都运行，因此纯控制面 master 也必须有探活地址。
func kubeProxyPlan(c *render.Context) (*Plan, error) {
	plan := &Plan{}
	if !kubeProxyEnabled(c) {
		return plan, nil
	}
	kp := &kubeProxy{}
	objects, err := kp.Objects(c)
	if err != nil {
		return nil, err
	}
	plan.Objects = objects.Objects()
	plan.Healths = append(plan.Healths, kp.Health(c)...)
	return plan, nil
}

// Objects 构建 kube-proxy 的 ServiceAccount/ClusterRoleBinding/ConfigMap/DaemonSet。
func (k *kubeProxy) Objects(c *render.Context) (*render.ObjectSet, error) {
	if !kubeProxyEnabled(c) {
		return &render.ObjectSet{}, nil
	}
	in, err := k.input(c)
	if err != nil {
		return nil, err
	}
	return in.objects(c.Release), nil
}

// Health 返回 kube-proxy 的本机健康端点。
func (k *kubeProxy) Health(*render.Context) []Health {
	return []Health{{
		Component: constants.ComponentKubeProxy,
		Scheme:    "http",
		Address:   fmt.Sprintf("127.0.0.1:%d", constants.KubeProxyHealthzPort),
		Path:      "/healthz",
	}}
}

// input 渲染 kube-proxy 的 ConfigMap 内容与 Pod 定义，并算出配置摘要。
// 只渲染一次 flags/config，避免 Objects 与 podSpec 重复调用。
func (k *kubeProxy) input(c *render.Context) (daemonSetInput, error) {
	data, err := k.flagsData(c)
	if err != nil {
		return daemonSetInput{}, err
	}
	p, err := render.LoadProfile(c)
	if err != nil {
		return daemonSetInput{}, err
	}
	flags, err := p.RenderFlags(constants.ComponentKubeProxy, data)
	if err != nil {
		return daemonSetInput{}, fmt.Errorf("渲染 kube-proxy 参数失败：%w", err)
	}
	config, err := k.configWithData(c, data)
	if err != nil {
		return daemonSetInput{}, err
	}
	// 配置、argv 或 kubeconfig 端点任一变化都要滚动更新。
	kubeconfig := string(ServiceAccountKubeconfig(kubeProxyUser, daemonSetAPIServer(c)))
	hash := daemonSetHash(flags, config, kubeconfig)
	data.ConfigHash = hash
	rendered, err := p.RenderTemplate(profile.TmplKubeProxyPodspec, data)
	if err != nil {
		return daemonSetInput{}, fmt.Errorf("渲染 kube-proxy Pod 定义失败：%w", err)
	}
	podSpec, err := parsePodSpec(rendered)
	if err != nil {
		return daemonSetInput{}, err
	}
	return daemonSetInput{
		Name:           kubeProxyName,
		Labels:         kubeProxyLabels(),
		ServiceAccount: kubeProxySAName,
		BindingName:    kubeProxyBindingName,
		RoleRef:        kubeProxyClusterRoleName,
		ConfigKey:      KubeProxyConfigKey,
		Config:         config,
		Kubeconfig:     kubeconfig,
		Hash:           hash,
		PodSpec:        podSpec,
		Health:         k.Health(c)[0],
	}, nil
}

// flagsData 构建 kube-proxy 的模板数据。
func (k *kubeProxy) flagsData(c *render.Context) (*profile.KubeProxyFlags, error) {
	common, err := baseFlagsData(c)
	if err != nil {
		return nil, err
	}
	data := &profile.KubeProxyFlags{CommonFlags: common}
	if err := fillKubeProxy(data, c, networkSpec(c)); err != nil {
		return nil, err
	}
	return data, nil
}

// configWithData 用已经算好的模板数据渲染 KubeProxyConfiguration。
func (k *kubeProxy) configWithData(c *render.Context, data *profile.KubeProxyFlags) (string, error) {
	p, err := render.LoadProfile(c)
	if err != nil {
		return "", err
	}
	rendered, err := p.RenderTemplate(profile.TmplKubeProxyConfig, data)
	if err != nil {
		return "", fmt.Errorf("渲染 kube-proxy 配置失败：%w", err)
	}
	return rendered, nil
}

// kubeProxyLabels 返回 kube-proxy 稳定的选择器标签。
func kubeProxyLabels() map[string]string {
	return map[string]string{"k8s-app": kubeProxyName, "app.kubernetes.io/name": kubeProxyName}
}

// fillKubeProxy 填充 kube-proxy 的模板数据。
func fillKubeProxy(data *profile.KubeProxyFlags, c *render.Context, cfg clusterconfig.NetworkSpec) error {
	image, err := c.Image(constants.ComponentKubeProxy)
	if err != nil {
		return fmt.Errorf("无法解析组件 %s 的镜像：%w", constants.ComponentKubeProxy, err)
	}
	// 镜像里的可执行文件是 /usr/local/bin/kube-proxy；用绝对路径避免依赖 PATH。
	data.Binary = "/usr/local/bin/" + constants.ComponentKubeProxy
	data.Image = image
	data.ClusterCIDR = cfg.PodCIDR
	data.KubeProxyMode = cfg.KubeProxy.Mode
	if data.KubeProxyMode == "" {
		data.KubeProxyMode = clusterconfig.KubeProxyModeIPVS
	}
	data.KubeProxyScheduler = cfg.KubeProxy.Scheduler
	if data.KubeProxyScheduler == "" {
		data.KubeProxyScheduler = clusterconfig.KubeProxyDefaultScheduler
	}
	data.KubeProxyConfigFile = KubeProxyConfigDir + "/" + KubeProxyConfigKey
	data.KubeProxyKubeconfigFile = KubeProxyConfigDir + "/" + DaemonSetKubeconfigKey
	data.KubeProxyConfigMapName = kubeProxyConfigMapName
	data.KubeProxyHealthzPort = constants.KubeProxyHealthzPort
	data.KubeProxyMetricsPort = constants.KubeProxyMetricsPort
	return nil
}

// kubeProxyEnabled 报告是否必须部署 kube-proxy。
func kubeProxyEnabled(c *render.Context) bool {
	return c != nil && c.Cluster != nil && c.Cluster.KubeProxyEnabled()
}
