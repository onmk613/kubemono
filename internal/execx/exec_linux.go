//go:build linux

package execx

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// platformRunner 用 os/exec 执行命令（仅 linux）。
type platformRunner struct{}

// Run 实现 Runner。
func (platformRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("执行 %s %s 失败: %w（输出：%s）",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
