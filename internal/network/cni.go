package network

import (
	"fmt"

	"kubemono/internal/profile"
	"kubemono/internal/render"
)

// CNIConfigFileName 是 kube-router 的 CNI conflist 文件名。
// 含 plugins 列表的 CNI 配置必须使用 .conflist 扩展名，否则 containerd 的
// libcni 会按单插件 .conf 解析而报 "cni plugin not initialized"。
// kube-router 的 CNI 管理已关闭（--enable-cni=false），不会再覆盖该文件。
const CNIConfigFileName = "10-kuberouter.conflist"

// CNIConfigPath 返回 kube-router 的 CNI conflist 所在路径。
func CNIConfigPath(c *render.Context) string {
	dir := ""
	if c != nil {
		dir = c.Paths.CNIConfig()
	}
	return dir + "/" + CNIConfigFileName
}

// RenderCNIConfig 为一个节点渲染 kube-router 的 CNI conflist。
// plugin=none 时本函数不会被调用：网络层不写任何 CNI 配置。
func RenderCNIConfig(c *render.Context, nodeCIDR string, mtu int) ([]byte, error) {
	p, err := render.LoadProfile(c)
	if err != nil {
		return nil, err
	}
	rendered, err := p.RenderTemplate(profile.TmplKubeRouterCNIConfig, profile.KubeRouterCNIData{
		Name:     "kubemono",
		NodeCIDR: nodeCIDR,
		MTU:      mtu,
	})
	if err != nil {
		return nil, fmt.Errorf("渲染 CNI 配置失败：%w", err)
	}
	return []byte(rendered), nil
}
