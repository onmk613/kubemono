package config

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	"kubemono/internal/constants"
)

// ExtraArgsSite 描述一个组件的 extraArgs 配置位置：新增组件时只改这张表，
// 渲染（render.ComponentExtraArgs）与校验（validateExtraArgs）都从它派生。
type ExtraArgsSite struct {
	// Component 是组件常量。
	Component string
	// Path 是 extraArgs 在 cluster.yaml 里的字段路径（校验错误定位用）。
	Path *field.Path
	// Args 返回该组件的 extraArgs。
	Args func(c *Cluster) map[string]string
}

// ExtraArgsSites 返回全部支持 extraArgs 的组件位置。
// 注意 kube-router 不进 render.ComponentExtraArgs 的分发：它的参数由 network
// 渲染，但校验仍需覆盖它。CoreDNS 已不开放通用 extraArgs（见 CoreDNSSpec）。
func (c *Cluster) ExtraArgsSites() []ExtraArgsSite {
	return []ExtraArgsSite{
		{constants.ComponentAPIServer, field.NewPath("controlPlane", "apiServer", "extraArgs"), func(c *Cluster) map[string]string { return c.ControlPlane.APIServer.ExtraArgs }},
		{constants.ComponentControllerManager, field.NewPath("controlPlane", "controllerManager", "extraArgs"), func(c *Cluster) map[string]string { return c.ControlPlane.ControllerManager.ExtraArgs }},
		{constants.ComponentScheduler, field.NewPath("controlPlane", "scheduler", "extraArgs"), func(c *Cluster) map[string]string { return c.ControlPlane.Scheduler.ExtraArgs }},
		{constants.ComponentEtcd, field.NewPath("controlPlane", "etcd", "extraArgs"), func(c *Cluster) map[string]string { return c.ControlPlane.Etcd.Local.ExtraArgs }},
		{constants.ComponentKubelet, field.NewPath("node", "kubelet", "extraArgs"), func(c *Cluster) map[string]string { return c.Node.Kubelet.ExtraArgs }},
		{constants.ComponentKubeRouter, field.NewPath("network", "kubeRouter", "extraArgs"), func(c *Cluster) map[string]string { return c.Network.KubeRouter.ExtraArgs }},
	}
}

// ExtraArgsFor 返回组件的 extraArgs（组件不在此列时为 nil）。
// 这是 render 侧的单一入口；render 与 validate 共用同一张映射，新增组件
// 不会只改到一处。
func (c *Cluster) ExtraArgsFor(component string) map[string]string {
	for _, site := range c.ExtraArgsSites() {
		if site.Component == component {
			return site.Args(c)
		}
	}
	return nil
}
