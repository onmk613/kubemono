//go:build !linux

package reset

import (
	"context"
	"fmt"
	"runtime"
)

// unsupportedHostFS 是 linux 之外的文件系统桩：所有会改主机的动作都明确报错，
// 因此在开发机上误执行 `kubemono reset` 不会删除任何东西。
type unsupportedHostFS struct{}

// NewHostFS 在非 linux 平台上返回一个永远报错的文件系统端口。
func NewHostFS() HostFS { return unsupportedHostFS{} }

func errUnsupported(action string) error {
	return fmt.Errorf("在 %s 上不能%s：reset 只在 linux 节点上执行（当前为开发/测试主机）", runtime.GOOS, action)
}

func (unsupportedHostFS) Exists(string) bool { return false }

func (unsupportedHostFS) CopyTree(context.Context, string, string) error {
	return errUnsupported("备份目录")
}

func (unsupportedHostFS) RemoveAll(context.Context, string) error {
	return errUnsupported("删除文件")
}

func (unsupportedHostFS) ListMounts(context.Context, string) ([]string, error) {
	return nil, errUnsupported("读取挂载点")
}

func (unsupportedHostFS) Unmount(context.Context, string) error {
	return errUnsupported("卸载挂载点")
}
