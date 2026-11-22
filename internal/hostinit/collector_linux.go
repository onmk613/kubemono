//go:build linux

package hostinit

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// Run 执行命令并返回合并后的输出。
func (osCollector) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("执行 %s %s 失败: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// StatFS 返回路径所在文件系统的可用字节数与可用 inode 数。
func (osCollector) StatFS(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s 失败: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Ffree), nil
}
