package render

import (
	"fmt"

	"kubemono/internal/profile"
)

// LoadProfile 加载内嵌的组件 profile。
// 单独的 nil/BOM 守卫是为了在"上下文不完整"时给出比模板渲染更早、更准确的
// 报错；网络与负载均衡渲染器共用这一份，避免各写一份守卫。
func LoadProfile(c *Context) (*profile.Profile, error) {
	if c == nil || c.BOM == nil {
		return nil, fmt.Errorf("缺少 BOM，无法加载 profile")
	}
	return profile.Load()
}
