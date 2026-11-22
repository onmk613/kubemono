//go:build !windows

package identity

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// oNoFollow 是尽力而为："写到路径本身，绝不穿过符号链接"。
var oNoFollow = syscall.O_NOFOLLOW

// openNoFollow 以只读方式打开 path，且不跟随末尾的符号链接。
func openNoFollow(path string) (*os.File, error) {
	if oNoFollow == 0 {
		return os.Open(path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// isSymlink 报告 path 是否存在并且是符号链接。
func isSymlink(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return fi.Mode()&fs.ModeSymlink != 0, nil
}
