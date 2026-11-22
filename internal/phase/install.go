package phase

import (
	"context"

	"kubemono/internal/kubelet"
)

// InstallComponent 从离线物料把组件的宿主二进制直接复制到 /usr/local/bin。
//
// override 是各 phase 自己的测试注入点；为 nil 时调用 internal/kubelet 里的
// 唯一实现。controlplane 与 etcd 两个 phase 共用这一份包装。
func InstallComponent(ctx context.Context, c *Context, override func(context.Context, *Context, string) error, component string) error {
	if override != nil {
		return override(ctx, c, component)
	}
	return kubelet.InstallComponent(ctx, c.Paths, c.Release, c.BOM,
		func(format string, args ...any) { c.Reporter().Infof(format, args...) }, component)
}
