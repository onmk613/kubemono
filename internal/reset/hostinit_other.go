//go:build !linux

package reset

import (
	"context"
	"fmt"
	"runtime"

	"kubemono/internal/constants"
)

// unsupportedHostInitReverter 是 linux 之外的主机初始化还原桩。
// 还原会调用 modprobe / sysctl / swapoff 等只在 linux 上存在的命令，
// 因此这里直接报错；--keep-hostinit 时 reset 不会调用它。
type unsupportedHostInitReverter struct{}

// newHostInitReverter 在非 linux 平台上返回一个永远报错的实现。
func newHostInitReverter() HostInitReverter { return unsupportedHostInitReverter{} }

// Revert 实现 HostInitReverter。
func (unsupportedHostInitReverter) Revert(context.Context, constants.Paths) error {
	return fmt.Errorf("在 %s 上不能还原主机初始化改动：这些动作只在 linux 上有意义", runtime.GOOS)
}
