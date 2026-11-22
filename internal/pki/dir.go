package pki

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ensurePrivateDir 确保证书/私钥的父目录存在；只在目录由本次调用创建时
// 收紧到 0700。
//
// 不能无条件 chmod：`kubemono pki issue --out .` 的父目录就是用户当前目录，
// 把共享目录改成 0700 是破坏性副作用。已经存在的目录保持原权限（私钥文件
// 本身仍是 0600）；host-init 创建的 PKI 目录以 0700 建立。
func ensurePrivateDir(path string) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查私钥目录 %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建私钥目录 %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("设置私钥目录权限 %s: %w", dir, err)
	}
	return nil
}
