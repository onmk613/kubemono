// Package loadbalancer 实现 kubemono 的集群内访问路径：
// worker 上的本地代理（haproxy）把 127.0.0.1:6443 转发到某个 master。
// 集群外部访问不在这里实现：kubemono 不部署 kube-vip；HA 入口由
// controlPlane.externalEndpoints 提供（VIP/DNS）。
// 期望状态用 render.Spec 表达（haproxy 配置、静态 Pod 清单、systemd unit）。
// 本包只负责渲染；真正的主机动作由消费该状态的 phase 执行。
package loadbalancer

import (
	"fmt"

	"kubemono/internal/constants"
	"kubemono/internal/render"
)

// HTTPGetter 发起 GET 请求，是健康检查的可注入接缝。
type HTTPGetter interface {
	Get(url string) (status int, err error)
}

// imageFor 返回 BOM 组件的镜像引用。
func imageFor(c *render.Context, component string) (string, error) {
	img, err := c.Image(component)
	if err != nil {
		return "", fmt.Errorf("无法解析组件 %s 的镜像：%w", component, err)
	}
	return img, nil
}

// 文件与集群对象使用的组件名。
const (
	// localProxyName 是 worker 代理的 BOM 组件名与 Pod 名。
	localProxyName = constants.ComponentLocalProxy
	// haproxyConfigPath 是约定的配置文件位置。
	haproxyConfigPath = "/etc/haproxy/haproxy.cfg"
	// haproxyStatsPort 是本地统计端点（仅 127.0.0.1）。
	haproxyStatsPort = 8404

	// LocalProxyStatsPort 是本地代理的 HTTP 统计端口，phase 用它做存活探测。
	LocalProxyStatsPort = haproxyStatsPort
	// haproxyBOMComponent 是携带 haproxy 镜像与 OS 包的 BOM 组件。
	// 文件与对象的属主仍是 constants.ComponentLocalProxy。
	haproxyBOMComponent = "haproxy"
)

// Plan 是负载均衡在一台节点上的完整期望状态。
type Plan struct {
	// Files 是本机文件，按路径排序。
	// haproxy 没有集群级对象、也没有独立健康端点（健康由 phase/localproxy 直接探
	// 8404），因此 Plan 只有文件：那两组"只写不读"的字段已经删除。
	Files []render.File
}

// Build 计算负载均衡在一台节点上的完整期望状态。
// 只有 worker 需要本地代理；master 直连本机 apiserver。
func Build(c *render.Context) (*Plan, error) {
	if c == nil || c.Cluster == nil {
		return nil, fmt.Errorf("loadbalancer: 缺少集群配置")
	}
	plan := &Plan{}
	spec := &render.Spec{}

	if !c.IsMaster() {
		lp, err := NewLocalProxy(c)
		if err != nil {
			return nil, err
		}
		if lp.Applies(c) {
			if err := lp.Render(c, spec); err != nil {
				return nil, err
			}
		}
	}

	plan.Files = spec.Files
	return plan, nil
}
