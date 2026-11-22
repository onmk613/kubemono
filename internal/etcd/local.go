package etcd

import (
	"os"

	"kubemono/internal/systemd"
)

// ServiceManager 是 systemctl 原语；声明来自 internal/systemd，避免三份同形接口。
type ServiceManager = systemd.Actioner

// LocalOps 是本机文件操作：etcd 数据目录。
// leave 会删除数据目录、停止 etcd.service，这些都会改本机，
// 因此必须可注入；真实实现只在 linux 上执行（见 local_linux.go）。
type LocalOps interface {
	// PrepareDir 创建目录及其父目录。
	PrepareDir(path string, mode os.FileMode) error
	// Exists 报告路径是否存在。
	Exists(path string) bool
}

// DefaultServiceManager 返回当前平台的 systemd 实现。
func DefaultServiceManager() ServiceManager { return defaultServiceManager() }

// DefaultLocalOps 返回当前平台的本机文件操作实现。
func DefaultLocalOps() LocalOps { return defaultLocalOps() }
