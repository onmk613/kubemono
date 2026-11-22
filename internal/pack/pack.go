package pack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kubemono/internal/artifact"
	"kubemono/internal/artifact/release"
	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
)

// Options 配置一次打包。
type Options struct {
	// BOM 是版本来源；为 nil 时使用内置 BOM。
	BOM *bom.BOM
	// OutDir 只接收最终归档与 .sha256；为空时使用 dist。
	OutDir string
	// Archs 是目标架构列表（amd64/arm64）；为空时取 linux/amd64。
	Archs []string
	// IncludeImages 把 OCI layout 打进归档。默认 false（轻量包）。
	IncludeImages bool
	// FetchImages 在 IncludeImages 时从镜像仓库补齐缺失的 OCI layout（需要网络）。
	FetchImages bool
	// ImagesDir 是源 OCI layout；为空表示 <CacheRoot>/<release>/images。
	ImagesDir string
	// CacheRoot 是下载缓存根目录；实际按 <CacheRoot>/<release> 隔离。
	// 为空时使用用户缓存。
	CacheRoot string
	// Refresh 为 true 时忽略已有组件缓存，重新下载。
	Refresh bool
	// Offline 为 true 时只用本地缓存；缺失组件/镜像直接报错，不创建网络下载器。
	Offline bool
	// DryRun 只做计划（见 PlanPack）：不建目录、不下载、不写归档。
	DryRun bool
	// Downloader 拉取缺失的组件归档；为 nil 时使用默认 HTTP 下载器。
	Downloader artifact.Downloader
	// Reporter 接收进度行；nil 时静默。
	Reporter artifact.Reporter
	// Created 覆盖归档内的创建时间（测试用）。
	Created time.Time
}

// DefaultArch 是 `kubemono bundle pack` 的默认目标架构。
// 绝大多数据场景是 linux/amd64；需要别的架构时显式传 --arch。
const DefaultArch = "amd64"

// DefaultOS 是唯一支持的目标 OS。
const DefaultOS = "linux"

// resolvedOptions 是补齐默认值、做完互斥校验后的打包参数。
// 把"解析/校验"从 Run 的主循环里分离出来，Run 只负责按架构执行。
type resolvedOptions struct {
	Options
	bom       *bom.BOM
	osName    string
	arches    []string
	outDir    string
	cacheRoot string
	download  artifact.Downloader
	report    artifact.Reporter
	imgDir    string
}

// resolvePackOptions 补齐默认值、校验架构与互斥开关、准备输出目录。
func resolvePackOptions(opts Options) (*resolvedOptions, error) {
	b := opts.BOM
	if b == nil {
		b = bom.MustLoad()
	}
	osName := DefaultOS
	arches := opts.Archs
	if len(arches) == 0 {
		arches = []string{DefaultArch}
	}
	outDir := strings.TrimSpace(opts.OutDir)
	if outDir == "" {
		outDir = "dist"
	}

	// 缓存根目录和输出目录是两条独立的边界：
	//   <cache-root>/<release>/<component>/<arch>/<source-hash>, images  只放下载物
	//   <out-dir>/kubemono-...tar.zst[.sha256]                   只放最终产物
	cacheRoot := strings.TrimSpace(opts.CacheRoot)
	if cacheRoot == "" {
		cacheRoot = release.DefaultCacheRoot()
	}
	releaseCache := filepath.Join(cacheRoot, b.Release)

	for _, arch := range arches {
		if !b.SupportsArch(arch) {
			return nil, fmt.Errorf("架构 %q 不在 BOM 的 arch 列表里（%s）", arch, strings.Join(b.Arch, ", "))
		}
	}
	if opts.Offline && opts.Refresh {
		return nil, fmt.Errorf("--offline 与 --refresh 不能同时使用")
	}
	if opts.Offline {
		// 离线模式只允许用缓存；镜像也不能现场拉取。
		opts.FetchImages = false
	}
	if err := ensureSeparateDirs(outDir, releaseCache); err != nil {
		return nil, err
	}
	if !opts.DryRun {
		if err := prepareOutDir(outDir); err != nil {
			return nil, err
		}
	}
	download := opts.Downloader
	if download == nil && !opts.Offline && !opts.DryRun {
		download = artifact.NewHTTPDownloader()
	}
	report := opts.Reporter
	report.Reportf("缓存根目录 %s（实际使用 %s）", cacheRoot, releaseCache)
	report.Reportf("输出目录 %s（只写归档与 .sha256）", outDir)

	imgDir := strings.TrimSpace(opts.ImagesDir)
	if imgDir == "" && opts.IncludeImages {
		imgDir = filepath.Join(releaseCache, artifact.DirImages)
	}
	return &resolvedOptions{
		Options: opts, bom: b, osName: osName, arches: arches, outDir: outDir,
		cacheRoot: cacheRoot, download: download,
		report: report, imgDir: imgDir,
	}, nil
}

// Run 按 Options 打包（可多架构），返回每个架构的产物。
func Run(ctx context.Context, opts Options) ([]release.Result, error) {
	r, err := resolvePackOptions(opts)
	if err != nil {
		return nil, err
	}
	report := r.report
	refs := imageRefs(r.bom)
	// --fetch-images=false 时在下载组件之前先确认镜像已在缓存里，避免白下载。
	if r.IncludeImages && !r.FetchImages {
		if err := ensureImagesCached(r.imgDir, refs, r.osName, r.arches); err != nil {
			return nil, err
		}
	}

	var out []release.Result
	for _, arch := range r.arches {
		platform := r.osName + "/" + arch
		if r.IncludeImages && r.FetchImages && len(refs) > 0 {
			if missing := missingImageRefs(r.imgDir, refs, platform); len(missing) > 0 {
				report.Reportf("拉取 %d 个镜像到 %s（%s）", len(missing), r.imgDir, platform)
				if err := release.SaveImagesContext(ctx, missing, r.imgDir, platform); err != nil {
					return nil, err
				}
			} else {
				report.Reportf("OCI layout 已包含全部 %d 个镜像，跳过拉取", len(refs))
			}
		}
		report.Reportf("打包 %s/%s", r.osName, arch)
		res, err := release.Package(ctx, release.PackageOptions{
			BOM:           r.bom,
			CacheRoot:     r.cacheRoot,
			Refresh:       r.Refresh,
			OutDir:        r.outDir,
			Arch:          arch,
			OS:            r.osName,
			IncludeImages: r.IncludeImages,
			ImagesDir:     r.imgDir,
			Downloader:    r.download,
			Reporter:      report,
			Created:       r.Created,
		})
		if err != nil {
			return out, err
		}
		out = append(out, *res)
	}
	return out, nil
}

// prepareOutDir 在下载/组装之前确认输出目录存在且可写。
func prepareOutDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("artifact: 输出目录为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("artifact: 创建输出目录 %s 失败: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("artifact: 检查输出目录 %s 失败: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("artifact: 输出路径 %s 不是目录", dir)
	}
	f, err := os.CreateTemp(dir, ".kubemono-write-test-*")
	if err != nil {
		return fmt.Errorf("artifact: 输出目录 %s 不可写: %w", dir, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("artifact: 输出目录 %s 不可写: %w", dir, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("artifact: 清理输出目录 %s 的临时文件失败: %w", dir, err)
	}
	return nil
}

// ensureSeparateDirs 拒绝输出目录与 release 缓存目录相同或互相包含，保证两者边界清晰。
func ensureSeparateDirs(outDir, cacheDir string) error {
	outAbs, err := filepath.Abs(outDir)
	if err != nil {
		return fmt.Errorf("artifact: 解析输出目录 %s 失败: %w", outDir, err)
	}
	cacheAbs, err := filepath.Abs(cacheDir)
	if err != nil {
		return fmt.Errorf("artifact: 解析缓存目录 %s 失败: %w", cacheDir, err)
	}
	if pathWithin(outAbs, cacheAbs) || pathWithin(cacheAbs, outAbs) {
		return fmt.Errorf(
			"artifact: --out-dir 与缓存目录不能相同或互相包含（out-dir=%s，cache=%s）；归档只写 --out-dir，下载只写 --cache-dir",
			outAbs, cacheAbs)
	}
	return nil
}

// pathWithin 报告 child 是否等于 parent 或位于 parent 之下。
func pathWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// imageRefs 返回本次打包需要的镜像引用（已去重、排序）。
func imageRefs(b *bom.BOM) []string {
	if b == nil {
		return nil
	}
	images := b.Images()
	refs := make([]string, 0, len(images))
	for _, img := range images {
		refs = append(refs, img.Reference())
	}
	return refs
}

// ensureImagesCached 在禁止联网拉取时预先检查每个架构的镜像是否齐全。
func ensureImagesCached(dir string, refs []string, osName string, arches []string) error {
	if len(refs) == 0 {
		return nil
	}
	var missing []string
	for _, arch := range arches {
		platform := osName + "/" + arch
		for _, ref := range missingImageRefs(dir, refs, platform) {
			missing = append(missing, ref+" ("+platform+")")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"artifact: 缓存 %s 缺少 %d 个镜像；用 --fetch-images=true（默认）让 pack 拉取，或先把 OCI layout 放到该目录：%s",
		dir, len(missing), strings.Join(missing, ", "))
}

// EffectiveBOM 返回应用了 cluster.yaml 版本覆盖的 BOM。
// cluster.yaml 是版本覆盖的唯一入口：k8s 用顶层 kubernetesVersion，
// 非 k8s 用 images.componentVersions；--bom 只替换组件列表与获取 URL。
func EffectiveBOM(bomPath, clusterPath string) (*bom.BOM, error) {
	b := bom.MustLoad()
	if bomPath != "" {
		data, err := os.ReadFile(bomPath)
		if err != nil {
			return nil, err
		}
		parsed, err := bom.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", bomPath, err)
		}
		b = parsed
	}
	if clusterPath == "" {
		return b, nil
	}
	data, err := os.ReadFile(clusterPath)
	if err != nil {
		return nil, err
	}
	cluster, err := clusterconfig.LoadCluster(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", clusterPath, err)
	}
	effective, err := bom.ApplyOverrides(b, cluster.VersionOverrides(), cluster.ImageOverrides(), func(w string) {
		_, _ = fmt.Fprintln(os.Stderr, "warning:", w)
	})
	if err != nil {
		return nil, err
	}
	return effective, nil
}

// ParseArchSpec 解析 "linux/amd64"、"amd64" 或逗号分隔的架构列表。
// 返回去重后的 (os, []arch)。只接受 value 里给出的合法 OS。
func ParseArchSpec(spec string) (osName string, archs []string, err error) {
	osName = DefaultOS
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, "/"); i >= 0 {
			partOS := strings.TrimSpace(part[:i])
			part = strings.TrimSpace(part[i+1:])
			if partOS != "" {
				osName = partOS
			}
		}
		if part == "" {
			return "", nil, fmt.Errorf("架构为空")
		}
		if seen[part] {
			continue
		}
		seen[part] = true
		archs = append(archs, part)
	}
	if len(archs) == 0 {
		archs = []string{DefaultArch}
	}
	return osName, archs, nil
}

// missingImageRefs 返回 layout 里还没有的引用（按引用 + 平台判断）。
func missingImageRefs(dir string, refs []string, platform string) []string {
	have := map[string]bool{}
	if existing, err := release.LayoutEntries(dir); err == nil {
		for _, e := range existing {
			have[e.Reference+"|"+e.Platform] = true
		}
	}
	var out []string
	for _, ref := range refs {
		// 没有记录 platform 的旧 layout 条目按"任意平台"处理，与
		// SelectLayoutImagesForPlatform 的保留策略保持一致。
		if !have[ref+"|"+platform] && !have[ref+"|"] {
			out = append(out, ref)
		}
	}
	return out
}
