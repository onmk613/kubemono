package config

import "strings"

// ImagesSpec 汇总按组件的镜像覆盖与版本覆盖。
type ImagesSpec struct {
	// Components 按组件覆盖完整镜像引用；key 必须是 BOM 中声明了 image 的组件。
	// 值是写全的镜像地址（含 registry host 与 tag），例如
	// registry.example.com/kubemono/coredns/coredns:v1.14.6。
	Components map[string]string `json:"components,omitempty" yaml:"components,omitempty"`
	// ComponentVersions 覆盖本集群使用的非 k8s 组件版本；任意版本都允许，非默认值只告警。
	// k8s 组件只能通过顶层 kubernetesVersion 整组固定，kubemono 自身不可覆盖。
	ComponentVersions map[string]string `json:"componentVersions,omitempty" yaml:"componentVersions,omitempty"`
}

// ImageOverrides 把集群配置里的按组件镜像覆盖展开成"组件名 -> 完整镜像引用"。
// 没有覆盖时返回 nil；空 image 由校验拒绝，这里再兜一层，避免把空值带进 BOM。
func (c *Cluster) ImageOverrides() map[string]string {
	if c == nil || len(c.Images.Components) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Images.Components))
	for name, raw := range c.Images.Components {
		if image := strings.TrimSpace(raw); image != "" {
			out[name] = image
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
