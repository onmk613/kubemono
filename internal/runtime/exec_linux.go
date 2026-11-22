//go:build linux

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"kubemono/internal/systemd"
)

// ExecRunner 通过 os/exec 执行外部命令（仅 linux 目标）。
type ExecRunner struct{}

// Run 实现 CommandRunner。
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (CommandResult, error) {
	if name == "" {
		return CommandResult{ExitCode: -1}, errors.New("命令名为空")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return CommandResult{ExitCode: -1}, fmt.Errorf("%w: %s", ErrCommandNotFound, name)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	res := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, fmt.Errorf("命令 %s %s 退出码 %d：%s", name, strings.Join(args, " "), res.ExitCode, strings.TrimSpace(stderr.String()))
		}
		return res, fmt.Errorf("执行命令 %s 失败：%w", name, runErr)
	}
	return res, nil
}

// defaultCommandRunner 返回 linux 上的默认命令执行器。
func defaultCommandRunner() CommandRunner { return ExecRunner{} }

// defaultServiceManager 返回 linux 上的默认服务管理器。
func defaultServiceManager() ServiceManager { return systemd.Manager{Timeout: 30 * time.Second} }
