package runtime

import (
	"fmt"
	"path/filepath"
	"strings"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
	"kubemono/internal/util/fsutil"
)

// installCNIPlugins 把 BOM 声明的 CNI 插件安装到 /opt/cni/bin。
// 与运行时模式无关；先整体检查缺失项，再逐个复制。
func installCNIPlugins(opts Options) error {
	// 测试/离线渲染可能没有 BOM。
	if opts.BOM == nil {
		return nil
	}
	report := opts.reporter()
	names := CNIPlugins(opts.BOM)
	if len(names) == 0 {
		return fmt.Errorf("BOM 未声明 cni-plugins 的 binaries，无法安装 CNI 插件")
	}
	cniBin := opts.Paths.CNIBin()
	if opts.DryRun {
		for _, name := range names {
			report.Infof("将安装 CNI 插件 %s（→ %s）", name, filepath.Join(cniBin, name))
		}
		return nil
	}

	var missing []string
	for _, name := range names {
		src := filepath.Join(opts.artifactDir(), artifact.DirBin, bom.CategoryCNI, name)
		if !fsutil.FileExists(src) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("物料缺少 CNI 插件 %s（BOM 声明 %d 个）：确认物料与本机 kubemono 是同一个 release",
			strings.Join(missing, ", "), len(names))
	}
	for _, name := range names {
		src := filepath.Join(opts.artifactDir(), artifact.DirBin, bom.CategoryCNI, name)
		if err := copyExecutable(src, filepath.Join(cniBin, name)); err != nil {
			return fmt.Errorf("安装 CNI 插件 %s 失败：%w", name, err)
		}
	}
	report.Debugf("CNI 插件已安装到 %s", cniBin)
	return nil
}
