package render

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// MergeConfigPatch 把用户提供的 configPatch 深度合并进渲染后的组件配置（YAML）。
// 补丁中的键覆盖渲染出的默认值；嵌套 map 递归合并。
// 这些字段（kubelet.configPatch）在渲染的最后一步合并，既保留 profile
// 随版本变化的默认值，又让运维人员的覆盖真正生效。
func MergeConfigPatch(rendered string, patch map[string]any) (string, error) {
	if len(patch) == 0 {
		return rendered, nil
	}
	var base map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &base); err != nil {
		return "", fmt.Errorf("解析组件配置失败: %w", err)
	}
	mergeMaps(base, patch)
	out, err := yaml.Marshal(base)
	if err != nil {
		return "", fmt.Errorf("序列化合并后的组件配置失败: %w", err)
	}
	return string(out), nil
}

// mergeMaps 把 src 深度合并进 dst（src 优先）。
// map 值递归合并；其他类型直接覆盖。这样 configPatch 可以只写要改的键，
// 而不会把 kubemono 生成的默认值整段删掉。
func mergeMaps(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				mergeMaps(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}
