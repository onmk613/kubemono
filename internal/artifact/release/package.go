package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
	"kubemono/internal/util/fsutil"
)

// Builder 是写入 meta.json 的 builder 字段（信息来源，不参与校验）。
// 造包入口只有 `kubemono bundle pack`，因此这里就是 kubemono。
const Builder = "kubemono"

// ErrMaterialMissing 在组件归档既不在缓存中、也无法下载时返回。
var ErrMaterialMissing = errors.New("artifact: 缓存中缺少组件物料")

// PackageOptions 配置一次单架构打包。
type PackageOptions struct {
	// BOM 是版本来源；为 nil 时使用内置 BOM。
	BOM *bom.BOM
	// CacheRoot 是下载缓存根目录（组件归档 + OCI layout）；为空时使用用户缓存。
	// 实际按 <CacheRoot>/<release> 隔离，避免不同 release 共用同一份镜像 layout。
	CacheRoot string
	// Refresh 为 true 时忽略已有组件缓存，重新下载。
	Refresh bool
	// OutDir 只接收最终归档与 .sha256；为空时使用当前目录。
	OutDir string
	// Arch 是目标架构：amd64 或 arm64。
	Arch string
	// OS 是目标操作系统；默认为 linux。
	OS string
	// IncludeImages 把 OCI layout 打进归档。
	IncludeImages bool
	// ImagesDir 是源 OCI layout；为空表示 <CacheRoot>/<release>/images。
	ImagesDir string
	// Downloader 拉取缺失的组件归档；为 nil 表示"仅用缓存"。
	Downloader artifact.Downloader
	// Reporter 接收进度行（可选）。
	Reporter artifact.Reporter
	// Created 覆盖 meta 时间戳（测试用）。
	Created time.Time
}

// Result 是一次打包的产物。
type Result struct {
	Meta    *artifact.Meta
	Archive string
	SHA256  string
	Size    int64
}

// resolvedPackageOptions 是应用默认值后的 PackageOptions。
type resolvedPackageOptions struct {
	PackageOptions
	bom      *bom.BOM
	cacheDir string // 已按 release 隔离的缓存目录
	staging  string
}

// placedFile 记录一个物料文件所属的组件与类别。
type placedFile struct {
	Component string
	Category  string
}

// Package 从本地缓存（必要时下载）组装一个 release/arch 的物料并写出归档。
// 归档是“全量”的：BOM 里所有有 Source 的组件都会进包，不按 cluster.yaml 裁剪。
// cluster.yaml 只在打包前决定版本（kubernetesVersion/componentVersions）。
func Package(ctx context.Context, opts PackageOptions) (*Result, error) {
	o, err := resolvePackageOptions(opts)
	if err != nil {
		return nil, err
	}
	defer func() {
		if o.staging != "" {
			_ = os.RemoveAll(o.staging)
		}
	}()

	o.Reporter.Reportf("release %s，架构 %s/%s，输出 %s", o.bom.Release, o.OS, o.Arch, o.OutDir)
	// 先确认输出目录可用，再开始收集/下载物料：避免下载完才在最后一步失败。
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("artifact: 创建输出目录 %s 失败: %w", o.OutDir, err)
	}
	placed := make(map[string]placedFile)
	if err := o.collectComponents(ctx, placed); err != nil {
		return nil, err
	}
	var images []artifact.ImageEntry
	if o.IncludeImages {
		images, err = o.collectImages()
		if err != nil {
			return nil, err
		}
	}
	meta, err := o.buildMeta(placed, images)
	if err != nil {
		return nil, err
	}
	out := filepath.Join(o.OutDir, artifact.ArchiveName(o.bom.Release, o.OS, o.Arch, o.IncludeImages))
	o.Reporter.Reportf("写出归档 %s（%d 个文件，%.1f MiB）", out, len(meta.Files), float64(meta.TotalSize())/(1<<20))
	// 归档边生成边写临时文件，成功后原子改名；失败不会在输出目录留下半截文件。
	if err := fsutil.WriteStreamingAtomic(out, 0o644, func(w io.Writer) error {
		return artifact.WriteArchive(w, meta, o.staging)
	}); err != nil {
		return nil, fmt.Errorf("artifact: 写出归档 %s 失败: %w", out, err)
	}
	sum, err := fsutil.SHA256File(out)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(out)
	if err != nil {
		return nil, err
	}
	sidecar := strings.TrimPrefix(sum, "sha256:") + "  " + filepath.Base(out) + "\n"
	if err := fsutil.WriteFileAtomic(out+".sha256", []byte(sidecar), 0o644); err != nil {
		return nil, fmt.Errorf("artifact: 写入 %s.sha256 失败: %w", out, err)
	}
	o.Reporter.Reportf("已生成 %s（%s，%d 字节）", out, sum, info.Size())
	res := &Result{Meta: meta, Archive: out, SHA256: sum, Size: info.Size()}
	return res, nil
}

// resolvePackageOptions 校验选项并应用默认值。
func resolvePackageOptions(opts PackageOptions) (*resolvedPackageOptions, error) {
	o := &resolvedPackageOptions{PackageOptions: opts}
	if o.BOM == nil {
		loaded, err := bom.Load()
		if err != nil {
			return nil, fmt.Errorf("artifact: 读取内置 BOM 失败: %w", err)
		}
		o.BOM = loaded
	}
	o.bom = o.BOM
	if o.Arch == "" {
		o.Arch = defaultArch()
	}
	if !o.bom.SupportsArch(o.Arch) {
		return nil, fmt.Errorf("artifact: 架构 %q 不在 BOM 的 arch 列表里（%s）", o.Arch, strings.Join(o.bom.Arch, ", "))
	}
	if o.OS == "" {
		o.OS = "linux"
	}
	if o.OS != "linux" {
		return nil, fmt.Errorf("artifact: 目标 OS %q 不受支持，当前只支持 linux", o.OS)
	}
	o.CacheRoot = strings.TrimSpace(o.CacheRoot)
	if o.CacheRoot == "" {
		o.CacheRoot = DefaultCacheRoot()
	}
	o.cacheDir = filepath.Join(o.CacheRoot, o.bom.Release)
	if o.OutDir == "" {
		o.OutDir = "."
	}
	if o.IncludeImages && o.ImagesDir == "" {
		o.ImagesDir = filepath.Join(o.cacheDir, artifact.DirImages)
	}
	if o.Created.IsZero() {
		o.Created = time.Now().UTC()
	} else {
		o.Created = o.Created.UTC()
	}
	dir, err := os.MkdirTemp("", "kubemono-pack-*")
	if err != nil {
		return nil, fmt.Errorf("artifact: 创建暂存目录失败: %w", err)
	}
	o.staging = dir
	return o, nil
}

// DefaultCacheRoot 返回默认的下载缓存根目录。
// 实际缓存按 release 隔离：<root>/<release>/<component>/<arch>/<source-hash>，镜像在 images。
func DefaultCacheRoot() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "kubemono", "pack")
	}
	return filepath.Join(os.TempDir(), "kubemono-pack-cache")
}

// defaultArch 返回本机架构；BOM 的 arch 列表负责最终校验。
func defaultArch() string { return runtime.GOARCH }

// collectComponents 把每个可下载组件的二进制物化到 staging/bin/<category>/<name>。
func (o *resolvedPackageOptions) collectComponents(ctx context.Context, placed map[string]placedFile) error {
	for _, name := range o.bom.Names() {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := o.bom.Components[name]
		if c.Source == "" || len(c.Binaries) == 0 {
			continue
		}
		cache, err := o.cacheComponent(ctx, name)
		if err != nil {
			return err
		}
		if isArchive(cache) {
			extractDir := filepath.Join(o.staging, ".extract", name)
			if err := os.MkdirAll(extractDir, 0o755); err != nil {
				return fmt.Errorf("artifact: 创建暂存目录 %s 失败: %w", extractDir, err)
			}
			extracted, err := extractArchiveMembers(cache, c.Binaries, extractDir)
			if err != nil {
				return fmt.Errorf("组件 %s: %w", name, err)
			}
			for _, bin := range c.Binaries {
				rel := artifact.BundlePath(c.Category, bin)
				if err := fsutil.CopyFileStreaming(extracted[bin], filepath.Join(o.staging, filepath.FromSlash(rel)), 0o755); err != nil {
					return fmt.Errorf("组件 %s: %w", name, err)
				}
				placed[rel] = placedFile{Component: name, Category: c.Category}
			}
			continue
		}
		if len(c.Binaries) != 1 {
			return fmt.Errorf("组件 %s: 有 %d 个二进制但来源 %s 不是归档，无法打包", name, len(c.Binaries), cache)
		}
		rel := artifact.BundlePath(c.Category, c.Binaries[0])
		if err := fsutil.CopyFileStreaming(cache, filepath.Join(o.staging, filepath.FromSlash(rel)), 0o755); err != nil {
			return fmt.Errorf("组件 %s: %w", name, err)
		}
		placed[rel] = placedFile{Component: name, Category: c.Category}
	}
	_ = os.RemoveAll(filepath.Join(o.staging, ".extract"))
	return nil
}

// cacheComponent 返回组件的缓存归档，缺失时下载。
func (o *resolvedPackageOptions) cacheComponent(ctx context.Context, name string) (string, error) {
	src, err := o.bom.SourceURL(name, o.Arch)
	if err != nil {
		return "", err
	}
	cached := artifact.ComponentCachePath(o.cacheDir, name, o.bom.Components[name].Version, o.Arch, src)
	if !o.Refresh && fsutil.FileExists(cached) {
		return cached, nil
	}
	if o.Downloader == nil {
		reason := "不存在"
		if o.Refresh {
			reason = "被 --refresh 忽略"
		}
		return "", fmt.Errorf("%w：组件 %s 的缓存文件 %s %s，且未提供下载器",
			ErrMaterialMissing, name, cached, reason)
	}
	if o.Refresh {
		o.Reporter.Reportf("刷新组件缓存 %s（%s）", name, src)
	} else {
		o.Reporter.Reportf("下载 %s（%s）", name, src)
	}
	if _, _, err := artifact.DownloadToFile(ctx, o.Downloader, src, cached); err != nil {
		return "", fmt.Errorf("组件 %s: %w", name, err)
	}
	return cached, nil
}

// collectImages 把本架构的 OCI layout 复制到 staging/images，并生成镜像条目。
func (o *resolvedPackageOptions) collectImages() ([]artifact.ImageEntry, error) {
	wanted := o.bom.Images()
	if len(wanted) == 0 {
		o.Reporter.Reportf("BOM 未声明镜像，跳过 OCI layout")
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(o.ImagesDir, artifact.OCILayoutFile)); err != nil {
		return nil, fmt.Errorf(
			"artifact: OCI layout 不存在：%s（先拉取镜像，或把 layout 放到该目录）: %w",
			o.ImagesDir, err)
	}
	// 只把 BOM 需要的引用放进归档。缓存 layout 里可能还有其它版本/仓库的镜像，
	// 不过滤会让归档随缓存无限变胖。
	keep := make(map[string]bool, len(wanted))
	for _, img := range wanted {
		keep[img.Reference()] = true
	}
	dst := filepath.Join(o.staging, artifact.DirImages)
	platform := o.OS + "/" + o.Arch
	copied, err := SelectLayoutImagesForPlatform(o.ImagesDir, dst, func(ref string) bool {
		return keep[ref]
	}, platform)
	if err != nil {
		return nil, err
	}
	if len(copied) == 0 {
		o.Reporter.Reportf("警告：镜像裁剪后没有任何镜像（layout %s，平台 %s）", o.ImagesDir, platform)
	}
	summary, err := LayoutSummary(dst)
	if err != nil {
		return nil, err
	}
	components := imageComponentMap(o.bom)
	entries := make([]artifact.ImageEntry, 0, len(summary))
	for _, s := range summary {
		entries = append(entries, artifact.ImageEntry{
			Component: components[s.Reference],
			Reference: s.Reference,
			Digest:    s.Digest,
			Platform:  s.Platform,
		})
	}
	o.Reporter.Reportf("已裁剪 OCI layout：%d 个镜像", len(copied))
	return entries, nil
}

// buildMeta 遍历 staging 生成 meta.json。
func (o *resolvedPackageOptions) buildMeta(placed map[string]placedFile, images []artifact.ImageEntry) (*artifact.Meta, error) {
	meta := &artifact.Meta{
		FormatVersion: artifact.FormatVersion,
		Release:       o.bom.Release,
		Kubernetes:    o.bom.Kubernetes,
		Arch:          o.Arch,
		OS:            o.OS,
		Created:       o.Created,
		Builder:       Builder,
		Components:    componentEntries(o.bom),
		Images:        images,
	}
	err := filepath.WalkDir(o.staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact: 物料 %s 不是普通文件", p)
		}
		rel, err := filepath.Rel(o.staging, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if artifact.IsReservedPath(rel) {
			return nil
		}
		sum, err := fsutil.SHA256File(p)
		if err != nil {
			return err
		}
		owner := placed[rel]
		if strings.HasPrefix(rel, artifact.DirImages+"/") {
			owner.Category = bom.CategoryImage
		}
		meta.Files = append(meta.Files, artifact.FileEntry{
			Path:      rel,
			SHA256:    sum,
			Size:      info.Size(),
			Mode:      fmt.Sprintf("%04o", info.Mode().Perm()),
			Component: owner.Component,
			Category:  owner.Category,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("artifact: 遍历物料目录失败: %w", err)
	}
	meta.Normalize()
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	return meta, nil
}

// componentEntries 列出 meta 所需的 BOM 组件。
func componentEntries(b *bom.BOM) []artifact.ComponentEntry {
	if b == nil {
		return nil
	}
	out := make([]artifact.ComponentEntry, 0, len(b.Components))
	for _, name := range b.Names() {
		c := b.Components[name]
		out = append(out, artifact.ComponentEntry{Name: name, Category: c.Category, Version: c.Version})
	}
	return out
}

// imageComponentMap 把 BOM 镜像引用映射到组件名。
func imageComponentMap(b *bom.BOM) map[string]string {
	out := map[string]string{}
	if b == nil {
		return out
	}
	for _, img := range b.Images() {
		out[img.Reference()] = img.Component
	}
	return out
}

// isArchive 报告缓存中的组件文件是否需要解包。
func isArchive(p string) bool {
	lower := strings.ToLower(p)
	return strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar")
}

// extractArchiveMembers 把 tar(.gz) 中所需的成员写入 dstDir。
// 只按 basename 匹配：BOM 只声明可执行文件名，归档内部的目录层级由这里消化。
func extractArchiveMembers(src string, wanted []string, dstDir string) (map[string]string, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, fmt.Errorf("打开归档 %s 失败: %w", src, err)
	}
	defer func() { _ = f.Close() }()

	var reader io.Reader = f
	if strings.HasSuffix(strings.ToLower(src), ".gz") || strings.HasSuffix(strings.ToLower(src), ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("解压 %s 失败: %w", src, err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	tr := tar.NewReader(reader)
	out := make(map[string]string, len(wanted))
	names := make([]string, 0, 8)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档 %s 失败: %w", src, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := path.Base(hdr.Name)
		if len(names) < 8 {
			names = append(names, base)
		}
		if _, ok := out[base]; ok {
			continue
		}
		if !slices.Contains(wanted, base) {
			continue
		}
		dst := filepath.Join(dstDir, base)
		mode := fs.FileMode(hdr.Mode).Perm()
		if mode == 0 {
			mode = 0o755
		}
		if err := writeTarMember(dst, mode, tr); err != nil {
			return nil, err
		}
		out[base] = dst
	}
	for _, w := range wanted {
		if _, ok := out[w]; !ok {
			return nil, fmt.Errorf("归档 %s 中没有 %s（前几个条目：%s）", src, w, strings.Join(names, ", "))
		}
	}
	return out, nil
}

// writeTarMember 按给定模式把一个 tar 成员写入 dst（fsync + 原子改名）。
func writeTarMember(dst string, mode fs.FileMode, r io.Reader) error {
	return fsutil.WriteStreamingAtomic(dst, mode, func(w io.Writer) error {
		_, err := io.Copy(w, r)
		return err
	})
}
