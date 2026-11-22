package kubelet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
	"kubemono/internal/constants"
	"kubemono/internal/execx"
	"kubemono/internal/util/fsutil"
)

// CommandRunner 执行一条主机命令。
// 定义收敛到 internal/execx（真实实现是 os/exec）；测试用内存实现即可，
// 因此本包的所有逻辑都不直接依赖 Linux。
type CommandRunner = execx.Runner

// BinaryInstaller 把组件的二进制从已导入的物料目录复制到运行目录。
// 目录约定：
//
//	物料 <ArtifactDir(release)>/bin/<category>/<name>
//	运行 <Paths.LocalBin()>/<name>（真实文件，0755；默认 /usr/local/bin）
//
// 不建立版本目录，也不建立软链：release 是唯一的版本边界，已安装文件就是
// 运行时真正执行的那一份。这样 /usr/local/kubemono 被删除后，已运行的集群不受影响。
//
// 幂等：目标文件 sha256 与物料一致时不复制。
type BinaryInstaller struct {
	// Paths 解析所有约定路径（生产环境 Root 为空）。
	Paths constants.Paths
	// Release 是已导入的版本，例如 v1.31.4-km.1。
	Release string
	// Report 接收进度消息。
	Report func(string, ...any)
	// BOM 提供组件版本、类别与二进制名。nil 时使用内嵌 BOM。
	BOM *bom.BOM
}

// InstallComponent 从离线物料安装一个组件的宿主二进制。
// controlplane/etcd 的 phase 共用这一份实现；测试仍可通过 phase 的
// InstallBinary 注入点覆盖。
func InstallComponent(ctx context.Context, paths constants.Paths, release string, b *bom.BOM, report func(string, ...any), component string) error {
	inst := &BinaryInstaller{Paths: paths, Release: release, BOM: b, Report: report}
	if _, err := inst.Install(ctx, component); err != nil {
		return fmt.Errorf("安装 %s 二进制失败：%w", component, err)
	}
	return nil
}

// binaryRef 是一个必须安装的二进制。
type binaryRef struct {
	// Name 是可执行文件名。
	Name string
	// Source 是该二进制在物料里的路径。
	Source string
	// SHA256 是清单记录的摘要（已知时）。
	SHA256 string
}

func (b *BinaryInstaller) bom() *bom.BOM {
	if b.BOM != nil {
		return b.BOM
	}
	return bom.MustLoad()
}

func (b *BinaryInstaller) report(format string, args ...any) {
	if b.Report != nil {
		b.Report(format, args...)
	}
}

// InstallDir 返回运行二进制的安装目录。
func (b *BinaryInstaller) InstallDir() string { return b.Paths.LocalBin() }

// InstallPath 返回单个二进制的安装路径（/usr/local/bin/<name>）。
func (b *BinaryInstaller) InstallPath(name string) string {
	return filepath.Join(b.InstallDir(), name)
}

// Install 把组件的每个二进制复制到运行目录。
// 返回安装目录。重复安装同一内容时是 no-op。
func (b *BinaryInstaller) Install(ctx context.Context, component string) (string, error) {
	if b.Release == "" {
		return "", fmt.Errorf("release 为空：无法定位 release %q 的物料目录，请在 phase 上下文里设置 Release", b.Release)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	refs, err := b.refs(component)
	if err != nil {
		return "", err
	}
	dir := b.InstallDir()
	for _, ref := range refs {
		dst := filepath.Join(dir, ref.Name)
		changed, err := b.copyIfChanged(ref, dst)
		if err != nil {
			return dir, err
		}
		if changed {
			b.report("已安装 %s（来自 %s）", dst, ref.Source)
		} else {
			b.report("%s 已是期望内容，跳过复制", dst)
		}
	}
	return dir, nil
}

// Verify 报告已安装的二进制是否与物料一致。
// 物料缺失时返回 (false, nil)：安装阶段会给出带操作指引的错误。
func (b *BinaryInstaller) Verify(_ context.Context, component string) (bool, error) {
	refs, err := b.refs(component)
	if err != nil {
		return false, nil
	}
	for _, ref := range refs {
		dst := b.InstallPath(ref.Name)
		sum, err := fsutil.SHA256File(dst)
		if err != nil {
			return false, nil
		}
		srcSum, err := fsutil.SHA256File(ref.Source)
		if err != nil {
			return false, nil
		}
		if sum != srcSum {
			return false, nil
		}
		info, err := os.Lstat(dst)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
			return false, nil
		}
	}
	return true, nil
}

// refs 解析组件的二进制以及它们的来源。
// 优先使用已导入物料里的 meta.json（它记录了每个文件的 sha256 与类别），
// 没有 meta 时退回 BOM 声明的 bin/<category>/<name>。
func (b *BinaryInstaller) refs(component string) ([]binaryRef, error) {
	comp, ok := b.bom().Component(component)
	if !ok {
		return nil, fmt.Errorf("BOM 里没有组件 %s，无法定位它的二进制；请检查 internal/bom/bom.yaml", component)
	}
	if len(comp.Binaries) == 0 {
		return nil, fmt.Errorf("BOM 里组件 %s 没有声明二进制（components.%s.binaries 为空），无法安装", component, component)
	}
	dir := b.Paths.ArtifactDir(b.Release)
	m := b.meta(dir)

	out := make([]binaryRef, 0, len(comp.Binaries))
	for _, name := range comp.Binaries {
		found, err := artifact.LocateBinary(dir, m, component, name, comp.Category)
		if err != nil {
			return nil, fmt.Errorf("缺少组件 %s 的二进制 %s：%w。请先执行 `kubemono bundle load <archive>` 导入 release %s 的物料",
				component, name, err, b.Release)
		}
		out = append(out, binaryRef{Name: name, Source: found.Path, SHA256: found.SHA256})
	}
	return out, nil
}

// meta 读取物料 meta（如果存在）。
func (b *BinaryInstaller) meta(dir string) *artifact.Meta {
	return artifact.LoadMetaOptional(dir)
}

// copyIfChanged 在内容或权限不同时把源二进制流式复制到 dst。
func (b *BinaryInstaller) copyIfChanged(ref binaryRef, dst string) (bool, error) {
	srcSum, err := fsutil.SHA256File(ref.Source)
	if err != nil {
		return false, fmt.Errorf("读取物料 %s 失败: %w。请重新执行 `kubemono bundle load <archive>`", ref.Source, err)
	}
	if ref.SHA256 != "" && !strings.EqualFold(ref.SHA256, srcSum) {
		return false, fmt.Errorf(
			"物料 %s 的内容与 meta 记录不一致（实际 %s，期望 %s）：物料已损坏，请重新导入物料",
			ref.Source, srcSum, ref.SHA256)
	}
	// 目标位置可能是软链；即使内容相同也必须替换成真实文件。
	if info, err := os.Lstat(dst); err == nil && info.Mode()&os.ModeSymlink == 0 {
		if existing, err := fsutil.SHA256File(dst); err == nil && existing == srcSum && info.Mode().Perm() == 0o755 {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, fmt.Errorf("创建安装目录 %s 失败: %w", filepath.Dir(dst), err)
	}
	if err := fsutil.CopyFileStreaming(ref.Source, dst, 0o755); err != nil {
		return false, fmt.Errorf("安装 %s 失败: %w。请确认 %s 可写且磁盘空间充足", dst, err, filepath.Dir(dst))
	}
	return true, nil
}
