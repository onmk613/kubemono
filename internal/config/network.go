package config

// NetworkSpec 配置 Pod 与 Service 网络。
type NetworkSpec struct {
	Plugin           string         `json:"plugin" yaml:"plugin"`
	PodCIDR          string         `json:"podCIDR,omitempty" yaml:"podCIDR,omitempty"`
	ServiceCIDR      string         `json:"serviceCIDR,omitempty" yaml:"serviceCIDR,omitempty"`
	ClusterDNS       string         `json:"clusterDNS,omitempty" yaml:"clusterDNS,omitempty"`
	DNSDomain        string         `json:"dnsDomain,omitempty" yaml:"dnsDomain,omitempty"`
	NodeCIDRMaskSize *int32         `json:"nodeCIDRMaskSize,omitempty" yaml:"nodeCIDRMaskSize,omitempty"`
	KubeRouter       KubeRouterSpec `json:"kubeRouter,omitempty" yaml:"kubeRouter,omitempty"`
	KubeProxy        KubeProxySpec  `json:"kubeProxy,omitempty" yaml:"kubeProxy,omitempty"`
}

// KubeRouterSpec 配置 kube-router。
type KubeRouterSpec struct {
	RunRouter     *bool             `json:"runRouter,omitempty" yaml:"runRouter,omitempty"`
	RunFirewall   *bool             `json:"runFirewall,omitempty" yaml:"runFirewall,omitempty"`
	EnableOverlay *bool             `json:"enableOverlay,omitempty" yaml:"enableOverlay,omitempty"`
	OverlayType   string            `json:"overlayType,omitempty" yaml:"overlayType,omitempty"`
	ClusterASN    *int64            `json:"clusterAsn,omitempty" yaml:"clusterAsn,omitempty"`
	MTU           int               `json:"mtu,omitempty" yaml:"mtu,omitempty"`
	ExtraArgs     map[string]string `json:"extraArgs,omitempty" yaml:"extraArgs,omitempty"`
}

// KubeProxySpec 配置 kube-proxy。
// kube-proxy 以 hostNetwork DaemonSet 运行，使用自己的 ServiceAccount token
// 访问 apiserver；kube-router 只负责 CNI，不承担 Service 代理。
type KubeProxySpec struct {
	// Enabled 控制是否部署 kube-proxy；默认 true。
	// 使用 Cilium/Calico 等自带 kube-proxy replacement 的 CNI 时置为 false。
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// Mode 是代理模式：ipvs（默认）、iptables、nftables。
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
	// Scheduler 是 IPVS 调度器，默认 rr；仅 mode=ipvs 时有效。
	Scheduler string `json:"scheduler,omitempty" yaml:"scheduler,omitempty"`
}

// KubeProxyEnabled 报告是否部署 kubemono 管理的 kube-proxy。
// 未显式配置时默认为 true；kube-router 只负责 CNI，不接管 Service 代理。
func (c *Cluster) KubeProxyEnabled() bool {
	if c == nil {
		return false
	}
	if c.Network.KubeProxy.Enabled == nil {
		return true
	}
	return *c.Network.KubeProxy.Enabled
}

// ServiceProxyMode 返回实际生效的 Service 代理实现。
func ServiceProxyMode(c *Cluster) string {
	if c.KubeProxyEnabled() {
		return "kube-proxy"
	}
	return "external"
}
