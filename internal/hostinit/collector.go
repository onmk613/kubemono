package hostinit

import (
	"os"
	"path/filepath"
	"sort"
)

// osCollector 是 Collector 的宿主实现。
// 只依赖标准库的跨平台部分（文件读取与通配）；
// 命令执行与文件系统统计在 collector_linux.go / collector_other.go 中定义。
type osCollector struct{}

// NewCollector 返回读取真实主机的 Collector。
// linux 上可以执行 systemctl 等主机命令并做文件系统统计；
// 其它平台（包括开发用的 darwin）这些 OS 相关操作返回 ErrUnsupported，
// 但 /etc/os-release 等文件读取仍然可用，
// 因此 Collect 在非 linux 上仍然返回“能得到的那部分事实”。
func NewCollector() Collector { return osCollector{} }

// ReadFile 读取文件。
func (osCollector) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// Glob 返回匹配的路径（已排序）。
func (osCollector) Glob(pattern string) ([]string, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}
