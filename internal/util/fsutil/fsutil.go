package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// WriteFileAtomic 原子地把 data 写入 path，并按需创建父目录。
// 临时文件与目标文件同目录，保证 rename 是同一文件系统内的原子操作；
// static pod 清单、unit 文件、kubeconfig 都依赖这一点。
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	return WriteStreamingAtomic(path, mode, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteStreamingAtomic 原子地把流式内容写入 path：调用方通过 write 把内容写进
// 临时文件，本函数负责完整的“建目录→临时文件→fsync→close→rename”仪式与
// 失败清理。下载与打包这类“边产生边落盘”的场景用它，替代各包自拼的
// 三十行样板，行为差异（目录、mode）由参数表达。
func WriteStreamingAtomic(path string, mode os.FileMode, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".part*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 失败路径上清理临时文件；成功时它已经被 rename 掉了。
		if _, err := os.Stat(tmpName); err == nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	return syncDir(dir)
}

// syncDir 对目录执行 fsync，使 rename 在崩溃后仍然生效。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil // 目录不可打开不是致命问题（例如某些网络文件系统） / not being able to open the directory is not fatal (e.g. some network filesystems)
	}
	defer func() { _ = d.Close() }()
	_ = d.Sync()
	return nil
}

// WriteFileIfChanged 仅在内容（或权限）不同时写入。
// 返回是否真的写入，便于调用方决定是否需要重启组件。
func WriteFileIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil {
		info, statErr := os.Stat(path)
		if statErr == nil && string(existing) == string(data) && info.Mode().Perm() == mode.Perm() {
			return false, nil
		}
	}
	if err := WriteFileAtomic(path, data, mode); err != nil {
		return false, err
	}
	return true, nil
}

// FileExists 报告常规文件是否存在。
func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// SHA256File 返回文件的 "sha256:<hex>" 摘要。
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// SHA256Bytes 返回字节切片的 "sha256:<hex>" 摘要。
func SHA256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CopyFile 原子地把 src 复制到 dst，并保留文件权限。
func CopyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return WriteFileAtomic(dst, data, info.Mode().Perm())
}

// CopyFileStreaming 流式地把 src 复制到 dst 并设置 mode，按需创建父目录。
// 写入走 WriteStreamingAtomic（临时文件 + rename），大文件（containerd 这类
// 几十 MiB 的二进制）不需要整体读进内存。runtime 安装二进制与 release 打包
// 共用这一份“open→copy→chmod→close”实现。
func CopyFileStreaming(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	if err := WriteStreamingAtomic(dst, mode, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	}); err != nil {
		return err
	}
	return nil
}

// Backup 把文件复制到备份目录，并保留相对布局。
// 变更前备份是 回滚的基础：备份只放在 /usr/local/kubemono/backup，
// 绝不放回 manifests 目录（kubelet 会加载该目录下的一切）。
func Backup(src, backupRoot, timestamp string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("backup: %s is a directory", src)
	}
	name := filepath.Base(src)
	dst := filepath.Join(backupRoot, timestamp, name)
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := WriteFileAtomic(dst, data, info.Mode().Perm()); err != nil {
		return "", err
	}
	return dst, nil
}

// DefaultBackupKeep 是时间戳备份目录的默认保留份数。
const DefaultBackupKeep = 10

// timestampDirPattern 匹配 Timestamp 生成的目录名。
var timestampDirPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)

// PruneTimestampedDirs 只保留最新的 keep 个 <root>/<timestamp> 备份目录。
// 非时间戳目录（用户自己放的）不碰；删除失败返回聚合错误，调用方通常只需告警。
func PruneTimestampedDirs(root string, keep int) error {
	if keep <= 0 {
		keep = DefaultBackupKeep
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && timestampDirPattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var errs []error
	for _, name := range names[min(keep, len(names)):] {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			errs = append(errs, fmt.Errorf("删除旧备份 %s 失败: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Timestamp 返回 kubemono 备份时间戳格式（UTC、可排序）。
func Timestamp(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// RemoveIfExists 删除文件，忽略 "not exist" 错误。
func RemoveIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// EnsureTrailingNewline 保证字节切片以单个换行结尾（空切片变为 "\n"）。
// systemd 与 kubelet 对无收尾换行的配置会告警甚至拒绝；network、render
// 共用这一份检查，避免各内联一份。
func EnsureTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return append(b, '\n')
	}
	return b
}
