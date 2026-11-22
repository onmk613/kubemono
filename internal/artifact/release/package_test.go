package release

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
	"kubemono/internal/util/fsutil"
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

func TestPackageProducesArchive(t *testing.T) {
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
	res, err := Package(context.Background(), PackageOptions{
		BOM:       b,
		CacheRoot: cache,
		OutDir:    out,
		Arch:      "amd64",
		OS:        "linux",
		Created:   time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if filepath.Base(res.Archive) != artifact.ArchiveName(b.Release, "linux", "amd64", false) {
		t.Fatalf("archive name = %s", res.Archive)
	}
	if _, err := os.Stat(res.Archive + ".sha256"); err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	meta, err := artifact.ReadMetaFromArchive(res.Archive)
	if err != nil {
		t.Fatalf("ReadMetaFromArchive: %v", err)
	}
	if meta.Release != b.Release || meta.Arch != "amd64" || meta.OS != "linux" {
		t.Fatalf("meta = %+v", meta)
	}
	if len(meta.Files) != 1 {
		t.Fatalf("files = %d, want 1（归档不应包含 kubemono）", len(meta.Files))
	}
	if _, ok := meta.Find("bin/k8s/kubelet"); !ok {
		t.Fatalf("kubelet missing: %+v", meta.Files)
	}

	dest := filepath.Join(t.TempDir(), "material")
	if _, err := artifact.Install(context.Background(), res.Archive, dest, artifact.InstallOptions{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	report, err := artifact.VerifyDir(dest, artifact.VerifyOptions{NoArch: true})
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	if !report.OK() {
		t.Fatalf("VerifyDir: %v", report.Error())
	}
}

// TestPackageFiltersImagesToBOM 确认缓存 layout 里多余的镜像不会进归档。
func TestPackageFiltersImagesToBOM(t *testing.T) {
	b, err := bom.Parse([]byte(testBOMWithImage))
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

	layoutDir := t.TempDir()
	lp, err := layout.Write(layoutDir, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	want := b.Images()[0].Reference()
	for _, ref := range []string{want, "registry.k8s.io/pause:3.9.0"} {
		opts := []layout.Option{
			layout.WithAnnotations(map[string]string{artifact.OCIRefNameAnnotation: ref}),
			layout.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
		}
		if err := lp.AppendImage(empty.Image, opts...); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Package(context.Background(), PackageOptions{
		BOM:           b,
		CacheRoot:     cache,
		OutDir:        t.TempDir(),
		Arch:          "amd64",
		OS:            "linux",
		IncludeImages: true,
		ImagesDir:     layoutDir,
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if len(res.Meta.Images) != 1 {
		t.Fatalf("镜像数 = %d，期望 1：%+v", len(res.Meta.Images), res.Meta.Images)
	}
	if res.Meta.Images[0].Reference != want {
		t.Fatalf("镜像 = %s，期望 %s", res.Meta.Images[0].Reference, want)
	}
}

// TestPackageFiltersImagesWithImageOverride 确认生效 BOM 的按组件镜像覆盖
// 和缓存 layout 里的引用一致时，镜像不会被过滤掉。
func TestPackageFiltersImagesWithImageOverride(t *testing.T) {
	b, err := bom.Parse([]byte(testBOMWithImage))
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

	effective, err := b.WithImageOverrides(map[string]string{
		"kubelet": "registry.example.com/mirror/pause:3.10.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	layoutDir := t.TempDir()
	lp, err := layout.Write(layoutDir, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	want := effective.Images()[0].Reference()
	for _, ref := range []string{want, b.Images()[0].Reference()} {
		opts := []layout.Option{
			layout.WithAnnotations(map[string]string{artifact.OCIRefNameAnnotation: ref}),
			layout.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
		}
		if err := lp.AppendImage(empty.Image, opts...); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Package(context.Background(), PackageOptions{
		BOM:           effective,
		CacheRoot:     cache,
		OutDir:        t.TempDir(),
		Arch:          "amd64",
		OS:            "linux",
		IncludeImages: true,
		ImagesDir:     layoutDir,
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if len(res.Meta.Images) != 1 || res.Meta.Images[0].Reference != want {
		t.Fatalf("镜像 = %+v，期望只保留 %s", res.Meta.Images, want)
	}
}

// staticDownloader 用固定内容模拟组件下载。
type staticDownloader struct {
	body []byte
}

func (d staticDownloader) Download(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(d.body)), nil
}

// TestPackageRefreshReplacesComponentCache 守住 --refresh：即使缓存文件存在，
// 也必须重新下载并让归档使用新内容。
func TestPackageRefreshReplacesComponentCache(t *testing.T) {
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
	if err := os.WriteFile(cachePath, []byte("old-kubelet"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Package(context.Background(), PackageOptions{
		BOM:        b,
		CacheRoot:  cache,
		OutDir:     t.TempDir(),
		Arch:       "amd64",
		OS:         "linux",
		Refresh:    true,
		Downloader: staticDownloader{body: []byte("new-kubelet")},
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	entry, ok := res.Meta.Find("bin/k8s/kubelet")
	if !ok {
		t.Fatalf("kubelet missing: %+v", res.Meta.Files)
	}
	if want := fsutil.SHA256Bytes([]byte("new-kubelet")); entry.SHA256 != want {
		t.Fatalf("kubelet sha256 = %s，期望 %s（说明 --refresh 没有重新下载）", entry.SHA256, want)
	}
	got, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-kubelet" {
		t.Fatalf("缓存内容 = %q，期望 new-kubelet", got)
	}
}
