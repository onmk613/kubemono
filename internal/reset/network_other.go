//go:build !linux

package reset

import (
	"context"
	"fmt"
	"runtime"
	"strings"
)

// unsupportedNetworkCleaner 是 linux 之外的原生网络清理桩。
// 网络清理需要 netlink/iptables/ipvs，只在 linux 上有意义；
// 其他平台明确报错，绝不尝试执行任何命令。
type unsupportedNetworkCleaner struct{}

// NewNetworkCleaner 在非 linux 平台上返回一个永远报错的清理器。
func NewNetworkCleaner() NetworkCleaner { return unsupportedNetworkCleaner{} }

// CleanNetwork 实现 NetworkCleaner。
func (unsupportedNetworkCleaner) CleanNetwork(_ context.Context, targets NetworkTargets) error {
	return fmt.Errorf("在 %s 上不能清理网络规则（%s）：原生网络清理只在 linux 上实现",
		runtime.GOOS, strings.Join(targets.Describe(), "、"))
}
