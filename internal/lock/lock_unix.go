//go:build unix

package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock 是一个已持有的文件锁。
type Lock struct {
	f    *os.File
	path string
}

// Acquire 以非阻塞方式获取 path 上的排他锁。
// 锁文件本身不删除：删除已打开的文件会让另一个进程锁到不同的 inode，
// 反而失去互斥；文件内容只记录持有者 PID 便于排障。
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建锁目录 %s 失败: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件 %s 失败: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("%w（%s）", ErrBusy, path)
		}
		return nil, fmt.Errorf("锁定 %s 失败: %w", path, err)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	return &Lock{f: f, path: path}, nil
}

// Close 释放锁。可重复调用。
func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("释放锁 %s 失败: %w", l.path, err)
	}
	return closeErr
}
