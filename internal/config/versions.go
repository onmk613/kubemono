package config

import (
	"maps"
	"strings"

	"kubemono/internal/constants"
)

// VersionOverrides 把集群的版本固定展开成"组件名 -> 版本"。
// kubernetesVersion 是 k8s 组件的唯一入口：非空时展开成全部 k8s 组件，
// images.componentVersions 只承担非 k8s 组件。k8s 组件的整组、同版本
// 由 bom.WithVersionOverrides 校验；版本范围不限制。
func (c *Cluster) VersionOverrides() map[string]string {
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.Images.ComponentVersions)+len(constants.KubernetesComponentNames()))
	maps.Copy(out, c.Images.ComponentVersions)
	if version := strings.TrimSpace(c.KubernetesVersion); version != "" {
		for _, name := range constants.KubernetesComponentNames() {
			out[name] = version
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
