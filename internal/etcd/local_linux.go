//go:build linux

package etcd

import (
	"os"
	"time"

	"kubemono/internal/systemd"
)

// defaultServiceManager 返回基于 systemctl 的生产实现（见 internal/systemd.Manager）。
func defaultServiceManager() ServiceManager { return systemd.Manager{Timeout: 30 * time.Second} }

// OSFiles 用标准库操作本机文件（仅 linux 目标）。
type OSFiles struct{}

// PrepareDir 实现 LocalOps。
func (OSFiles) PrepareDir(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}

// Exists 实现 LocalOps。
func (OSFiles) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func defaultLocalOps() LocalOps { return OSFiles{} }
