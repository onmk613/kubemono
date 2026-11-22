//go:build !linux

package execx

import (
	"context"
	"fmt"
	"runtime"
	"strings"
)

// platformRunner 在非 linux 平台上明确拒绝执行。
type platformRunner struct{}

// Run 实现 Runner：绝不在开发机上执行 systemctl/ctr 之类的命令。
func (platformRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("在 %s 上不能执行 %s %s：宿主机命令只在 linux 节点上运行（当前为开发/测试主机）",
		runtime.GOOS, name, strings.Join(args, " "))
}
