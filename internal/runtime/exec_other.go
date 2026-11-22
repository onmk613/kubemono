//go:build !linux

package runtime

import (
	"context"
	"fmt"
	"time"

	"kubemono/internal/systemd"
)

// unsupportedCommandRunner 在非 linux 平台拒绝执行外部命令。
// 运行时管理只在 linux 目标上生效（GOOS=linux 构建）；在开发机（darwin）上
// 返回明确错误，而不是意外调用本机命令。
type unsupportedCommandRunner struct{}

// Run 实现 CommandRunner。
func (unsupportedCommandRunner) Run(_ context.Context, name string, _ ...string) (CommandResult, error) {
	return CommandResult{ExitCode: -1}, fmt.Errorf("%w: 当前平台不支持执行 %s（运行时管理只在 linux 目标上生效）", ErrCommandNotFound, name)
}

// defaultCommandRunner 返回非 linux 平台上的占位实现。
func defaultCommandRunner() CommandRunner { return unsupportedCommandRunner{} }

// defaultServiceManager 在非 linux 平台也返回 systemd.Manager：
// execx.Default() 会明确拒绝执行宿主机命令，错误由统一入口给出。
func defaultServiceManager() ServiceManager { return systemd.Manager{Timeout: 30 * time.Second} }
