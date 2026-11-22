package network

import (
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/profile"
	"kubemono/internal/render"
)

// baseFlagsData 返回网络组件模板数据的公共部分。
// ServiceCIDR/ClusterDomain 不在网络组件的模板字段里（kube-proxy.config 用
// PodCIDR）；公共结构保留字段，但两个网络组件都不引用它们。
func baseFlagsData(c *render.Context) (profile.CommonFlags, error) {
	if c == nil || c.Cluster == nil {
		return profile.CommonFlags{}, errNoCluster
	}
	return profile.CommonFlags{
		Hostname: c.NodeName(),
	}, nil
}

// maskSize 返回 network.nodeCIDRMaskSize，未设置时使用文档规定的默认值。
func maskSize(cfg clusterconfig.NetworkSpec) int32 {
	if cfg.NodeCIDRMaskSize == nil {
		return constants.DefaultNodeCIDRMaskSize
	}
	return *cfg.NodeCIDRMaskSize
}
