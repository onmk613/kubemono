package pack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
)

const testBOM = `release: v1.37.1-km.1
kubernetes: v1.37.1
arch: [amd64]
components:
  kubelet:
    version: v1.37.1
    category: k8s
    binaries: [kubelet]
    source: https://example.invalid/kubelet-{arch}
`

const testBOMWithImage = `release: v1.37.1-km.1
kubernetes: v1.37.1
arch: [amd64]
components:
  kubelet:
    version: v1.37.1
    category: k8s
    binaries: [kubelet]
    source: https://example.invalid/kubelet-{arch}
    image: registry.k8s.io/pause:3.10.2
`

// TestRunProducesLoadableBundle 端到端验证"pack 出来的就是一个可以直接 load 的包"：
// 造包 → 读 meta → Install 到物料库。全程不联网（组件来自本地缓存）。
func TestRunProducesLoadableBundle(t *testing.T) {
	b, err := bom.Parse([]byte(testBOM))
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	src, err := b.SourceURL("kubelet", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	cachePath := artifact.ComponentCachePath(filepath.Join(cache, b.Release), "kubelet", b.Components["kubelet"].Version, "amd64", src)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("#!/bin/sh\necho kubelet\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	results, err := Run(context.Background(), Options{
		BOM:       b,
		CacheRoot: cache,
		OutDir:    out,
		Archs:     []string{"amd64"},
		Created:   time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("结果数 = %d", len(results))
	}
	res := results[0]
	wantName := artifact.ArchiveName(b.Release, "linux", "amd64", false)
	if filepath.Base(res.Archive) != wantName {
		t.Fatalf("归档名 = %s，期望 %s", filepath.Base(res.Archive), wantName)
	}
	if _, err := os.Stat(res.Archive + ".sha256"); err != nil {
		t.Fatalf("缺少 .sha256 sidecar: %v", err)
	}

	meta, err := artifact.ReadMetaFromArchive(res.Archive)
	if err != nil {
		t.Fatalf("ReadMetaFromArchive: %v", err)
	}
	if meta.Release != b.Release || meta.Arch != "amd64" || meta.OS != "linux" {
		t.Fatalf("meta = %+v", meta)
	}

	// 关键断言：产物能被 Install（也就是 bundle load 的落地路径）接受。
	dest := filepath.Join(t.TempDir(), "material")
	installed, err := artifact.Install(context.Background(), res.Archive, dest, artifact.InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if installed.Release != b.Release {
		t.Fatalf("install meta = %+v", installed)
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "k8s", "kubelet")); err != nil {
		t.Fatalf("物料里缺少 kubelet: %v", err)
	}
}

// seedKubelet 把测试 BOM 的 kubelet 放进 release 级组件缓存。
func seedKubelet(t *testing.T, b *bom.BOM, cacheRoot string) string {
	t.Helper()
	src, err := b.SourceURL("kubelet", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	p := artifact.ComponentCachePath(filepath.Join(cacheRoot, b.Release), "kubelet", b.Components["kubelet"].Version, "amd64", src)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho kubelet\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunSeparatesCacheAndOutput 锁定目录边界：缓存按 release 隔离，输出目录
// 里只出现最终归档和 .sha256。
func TestRunSeparatesCacheAndOutput(t *testing.T) {
	b, err := bom.Parse([]byte(testBOM))
	if err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()
	seedKubelet(t, b, cacheRoot)
	out := t.TempDir()

	if _, err := Run(context.Background(), Options{
		BOM:       b,
		CacheRoot: cacheRoot,
		OutDir:    out,
		Archs:     []string{"amd64"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("输出目录条目 = %d，期望 2（归档 + .sha256）：%v", len(entries), entries)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("输出目录不应包含目录：%s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, b.Release, "kubelet")); err != nil {
		t.Fatalf("组件缓存不在 <cache>/<release> 下：%v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(cacheRoot, "*.tar.zst")); len(matches) != 0 {
		t.Fatalf("缓存根目录不应出现最终归档：%v", matches)
	}
}

// TestRunDefaultDoesNotCreateImageCache 确认默认轻量包不会碰镜像缓存。
func TestRunDefaultDoesNotCreateImageCache(t *testing.T) {
	b, err := bom.Parse([]byte(testBOMWithImage))
	if err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()
	seedKubelet(t, b, cacheRoot)
	results, err := Run(context.Background(), Options{
		BOM:       b,
		CacheRoot: cacheRoot,
		OutDir:    t.TempDir(),
		Archs:     []string{"amd64"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := filepath.Base(results[0].Archive), artifact.ArchiveName(b.Release, "linux", "amd64", false); got != want {
		t.Fatalf("归档名 = %s，期望 %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, b.Release, "images")); !os.IsNotExist(err) {
		t.Fatalf("默认轻量包不应创建镜像缓存，stat err=%v", err)
	}
}

// TestRunRequiresCachedImagesWhenFetchDisabled 确认禁止联网时会在下载组件前
// 就报缺镜像，而不是下载完才失败。
func TestRunRequiresCachedImagesWhenFetchDisabled(t *testing.T) {
	b, err := bom.Parse([]byte(testBOMWithImage))
	if err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()
	_, err = Run(context.Background(), Options{
		BOM:           b,
		CacheRoot:     cacheRoot,
		OutDir:        t.TempDir(),
		Archs:         []string{"amd64"},
		IncludeImages: true,
		FetchImages:   false,
	})
	if err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("err = %v，期望提示缺少镜像", err)
	}
	src, _ := b.SourceURL("kubelet", "amd64")
	if _, statErr := os.Stat(artifact.ComponentCachePath(filepath.Join(cacheRoot, b.Release), "kubelet", b.Components["kubelet"].Version, "amd64", src)); !os.IsNotExist(statErr) {
		t.Fatalf("缺镜像时不应先下载组件，stat err=%v", statErr)
	}
}

// TestRunRejectsOverlappingDirs 输出目录与缓存目录必须分开（双向）。
func TestRunRejectsOverlappingDirs(t *testing.T) {
	b, err := bom.Parse([]byte(testBOM))
	if err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()
	outRoot := t.TempDir()
	cases := []struct {
		name  string
		out   string
		cache string
	}{
		{"输出目录在 release 缓存内", filepath.Join(cacheRoot, b.Release, "dist"), cacheRoot},
		{"缓存目录在输出目录内", outRoot, filepath.Join(outRoot, "cache")},
		{"输出目录等于缓存根目录", cacheRoot, cacheRoot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(context.Background(), Options{
				BOM:       b,
				CacheRoot: tc.cache,
				OutDir:    tc.out,
				Archs:     []string{"amd64"},
			})
			if err == nil || !strings.Contains(err.Error(), "--out-dir") {
				t.Fatalf("err = %v，期望边界错误", err)
			}
		})
	}
}

// TestRunPreparesOutputBeforeMaterial 输出路径不可用时不应先下载物料。
func TestRunPreparesOutputBeforeMaterial(t *testing.T) {
	b, err := bom.Parse([]byte(testBOM))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "dist")
	if err := os.WriteFile(out, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), Options{
		BOM:       b,
		CacheRoot: t.TempDir(),
		OutDir:    out,
		Archs:     []string{"amd64"},
	})
	if err == nil || !strings.Contains(err.Error(), "输出目录") {
		t.Fatalf("err = %v，期望输出目录错误", err)
	}
}
