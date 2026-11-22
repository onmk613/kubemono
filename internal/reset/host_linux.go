//go:build linux

package reset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"kubemono/internal/util/fsutil"
)

// realHostFS 是 linux 上的真实文件系统/挂载点实现。
// 只被 reset 使用；所有删除都限定在调用方给出的绝对路径上。
type realHostFS struct{}

// NewHostFS 返回当前平台的文件系统端口实现。
func NewHostFS() HostFS { return realHostFS{} }

// Exists 实现 HostFS。
func (realHostFS) Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// CopyTree 实现 HostFS：递归复制目录，保持权限位。
func (realHostFS) CopyTree(ctx context.Context, src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", src, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录，拒绝备份", src)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("创建备份目录 %s 失败: %w", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", src, err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		child, err := entry.Info()
		if err != nil {
			return fmt.Errorf("读取 %s 属性失败: %w", srcPath, err)
		}
		switch {
		case entry.IsDir():
			if err := (realHostFS{}).CopyTree(ctx, srcPath, dstPath); err != nil {
				return err
			}
		case child.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(srcPath)
			if err != nil {
				return fmt.Errorf("读取符号链接 %s 失败: %w", srcPath, err)
			}
			if err := os.Symlink(target, dstPath); err != nil && !os.IsExist(err) {
				return fmt.Errorf("复制符号链接 %s 失败: %w", dstPath, err)
			}
		default:
			data, err := os.ReadFile(srcPath)
			if err != nil {
				return fmt.Errorf("读取 %s 失败: %w", srcPath, err)
			}
			if err := fsutil.WriteFileAtomic(dstPath, data, child.Mode().Perm()); err != nil {
				return fmt.Errorf("写入备份 %s 失败: %w", dstPath, err)
			}
		}
	}
	return nil
}

// RemoveAll 实现 HostFS。
func (realHostFS) RemoveAll(_ context.Context, path string) error {
	if err := safePath(path); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("删除 %s 失败: %w", path, err)
	}
	return nil
}

// ListMounts 实现 HostFS：解析 /proc/self/mountinfo。
func (realHostFS) ListMounts(_ context.Context, prefix string) ([]string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("读取 /proc/self/mountinfo 失败: %w", err)
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mount := unescapeMount(fields[4])
		if prefix != "" && !strings.HasPrefix(mount, prefix) {
			continue
		}
		out = append(out, mount)
	}
	// 深的挂载点先卸载，避免卸载父挂载点后子挂载点路径消失。
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out, nil
}

// Unmount 实现 HostFS：延迟卸载，已经卸载时视为成功。
func (realHostFS) Unmount(_ context.Context, target string) error {
	if err := safePath(target); err != nil {
		return err
	}
	if err := syscall.Unmount(target, syscall.MNT_DETACH); err != nil {
		if err == syscall.EINVAL || err == syscall.ENOENT {
			return nil
		}
		return fmt.Errorf("卸载 %s 失败: %w", target, err)
	}
	return nil
}
