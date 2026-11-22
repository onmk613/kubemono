//go:build !linux

package etcd

import (
	"fmt"
	"os"
	"time"

	"kubemono/internal/systemd"
)

// unsupportedLocalOps 在非 linux 平台拒绝改本机文件。
// 单测全部注入 fake，因此这里的报错不会影响 darwin 上的 go test；
// 它只是保证“在开发机上误执行真实操作”会立刻失败。
type unsupportedLocalOps struct{}

// PrepareDir 实现 LocalOps。
func (unsupportedLocalOps) PrepareDir(path string, _ os.FileMode) error {
	return fmt.Errorf("当前平台不支持准备 etcd 数据目录（%s）：数据目录操作只在 linux 目标上生效", path)
}

// Exists 实现 LocalOps。
func (unsupportedLocalOps) Exists(string) bool { return false }

// defaultServiceManager 在非 linux 平台也返回 systemd.Manager：
// execx.Default() 会拒绝执行宿主机命令，错误由统一入口给出。
func defaultServiceManager() ServiceManager { return systemd.Manager{Timeout: 30 * time.Second} }

func defaultLocalOps() LocalOps { return unsupportedLocalOps{} }
