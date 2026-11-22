// 本文件生成一个节点完整的本机期望状态：把 render.DefaultBuilders（运行时/
// 控制面/etcd/kubelet）与网络层（kube-router CNI、worker 本地代理）合并成
// 同一份 Spec。只有 install 使用，原先的 internal/desired 包已合并进来。
package install

import (
	"fmt"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/loadbalancer"
	"kubemono/internal/network"
	"kubemono/internal/render"
)

// Build 生成一个节点完整的本机期望状态。
// nodeCIDR 是 kube-controller-manager 已分配给本节点的 Pod 子网；为空时由
// network.Build 按规则推导。
func buildDesired(c *render.Context, nodeCIDR string) (*render.Spec, error) {
	spec, err := render.Render(c, render.DefaultBuilders()...)
	if err != nil {
		return nil, err
	}
	if c == nil || c.Host == nil || !c.Host.IsWorker() {
		return spec, nil
	}
	if c.Cluster == nil {
		return nil, fmt.Errorf("desired: 缺少集群配置")
	}
	if c.Cluster.Network.Plugin == clusterconfig.PluginKubeRouter {
		plan, err := network.Build(c, nodeCIDR)
		if err != nil {
			return nil, fmt.Errorf("desired: 渲染网络层失败: %w", err)
		}
		for _, f := range plan.Files {
			mergeFile(spec, f)
		}
	}
	lbPlan, err := loadbalancer.Build(c)
	if err != nil {
		return nil, fmt.Errorf("desired: 渲染本地代理失败: %w", err)
	}
	for _, f := range lbPlan.Files {
		mergeFile(spec, f)
	}
	spec.Sort()
	return spec, nil
}

// mergeFile 追加文件；同路径已存在时保留先写入的内容（network.Build 与
// DefaultBuilders 不应出现同路径，出现时由 Validate 兜底报错）。
func mergeFile(spec *render.Spec, f render.File) {
	if _, ok := spec.Find(f.Path); ok {
		return
	}
	spec.Add(f)
}
