package reset

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// HostFS 是本机文件系统/挂载点的可注入端口。
// reset 只会通过它备份、删除和卸载挂载点：
// 真实实现（linux）直接操作主机，其他平台返回明确错误，
// 单测注入内存 fake 即可断言"删了哪些路径、卸载了哪些挂载点"。
type HostFS interface {
	// Exists 报告路径是否存在。
	Exists(path string) bool
	// CopyTree 递归复制目录（保持权限），用于 PKI 备份。
	CopyTree(ctx context.Context, src, dst string) error
	// RemoveAll 递归删除文件或目录。
	RemoveAll(ctx context.Context, path string) error
	// ListMounts 返回挂载点列表（prefix 非空时只返回以其开头的挂载点）。
	ListMounts(ctx context.Context, prefix string) ([]string, error)
	// Unmount 卸载一个挂载点；已经卸载时返回 nil。
	Unmount(ctx context.Context, target string) error
}

// safePath 拒绝删除/操作空路径、根路径与相对路径。
// reset 的目标路径全部由 constants.Paths 计算得出，这里只是最后一道防线：
// 任何"看起来不像受管路径"的输入都必须让操作失败，而不是照做。
func safePath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("拒绝操作空路径")
	case path == "/":
		return fmt.Errorf("拒绝操作根目录 /")
	case !filepath.IsAbs(path):
		return fmt.Errorf("拒绝操作相对路径 %q", path)
	}
	return nil
}

// unescapeMount 还原 mountinfo 里的八进制转义（\040 空格、\011 tab、\012 换行、\134 反斜杠）。
func unescapeMount(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(s)
}
