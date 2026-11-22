package bundle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"kubemono/internal/artifact"
	"kubemono/internal/artifact/release"
	"kubemono/internal/bom"
	"kubemono/internal/cli/options"
	"kubemono/internal/pack"
	"kubemono/internal/util/fsutil"
)

func testArchive(t *testing.T, dir string) (string, string) {
	t.Helper()
	rel := bom.MustLoad().Release
	root := t.TempDir()
	files := []artifact.FileEntry{}
	for _, f := range []struct {
		rel, body string
		mode      os.FileMode
	}{
		{"bin/k8s/kubelet", "kubelet", 0o755},
	} {
		p := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.body), f.mode); err != nil {
			t.Fatal(err)
		}
		files = append(files, artifact.FileEntry{
			Path: f.rel, SHA256: fsutil.SHA256Bytes([]byte(f.body)),
			Size: int64(len(f.body)), Mode: "0755",
		})
	}
	meta := &artifact.Meta{
		FormatVersion: artifact.FormatVersion,
		Release:       rel,
		Kubernetes:    bom.MustLoad().Kubernetes,
		Arch:          runtime.GOARCH,
		OS:            "linux",
		Created:       time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Builder:       "test",
		Components: []artifact.ComponentEntry{{
			Name:     "kubelet",
			Category: "k8s",
			Version:  bom.MustLoad().ComponentVersion("kubelet"),
		}},
		Files: files,
	}
	out := filepath.Join(dir, artifact.ArchiveName(rel, "linux", runtime.GOARCH, false))
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.WriteArchive(f, meta, root); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return out, rel
}

func run(t *testing.T, opts *options.GlobalOptions, args ...string) (string, error) {
	t.Helper()
	cmd := New(opts)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return buf.String(), err
}

func TestLoadAndVerify(t *testing.T) {
	root := t.TempDir()
	opts := options.Default()
	opts.TestRoot = root
	archive, rel := testArchive(t, t.TempDir())
	out, err := run(t, opts, "load", archive, "--no-arch")
	if err != nil {
		t.Fatalf("load: %v\n%s", err, out)
	}
	out, err = run(t, opts, "verify", archive, "--no-arch")
	if err != nil {
		t.Fatalf("verify archive: %v\n%s", err, out)
	}
	out, err = run(t, opts, "list", archive)
	if err != nil {
		t.Fatalf("list archive: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kubelet") {
		t.Fatalf("list archive output missing kubelet:\n%s", out)
	}
	out, err = run(t, opts, "verify", "--release", rel, "--no-arch")
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	out, err = run(t, opts, "list", "--arch", runtime.GOARCH)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !bytes.Contains([]byte(out), []byte(rel)) {
		t.Fatalf("list output missing release:\n%s", out)
	}
}

func TestLoadDryRunDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	opts := options.Default()
	opts.TestRoot = root
	archive, rel := testArchive(t, t.TempDir())

	out, err := run(t, opts, "load", "--dry-run", archive, "--no-arch")
	if err != nil {
		t.Fatalf("load --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, rel) {
		t.Fatalf("dry-run output missing release:\n%s", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run output missing mode:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(opts.Paths().Artifacts(), rel)); !os.IsNotExist(statErr) {
		t.Fatalf("dry-run 不应写物料库，stat err = %v", statErr)
	}
}

// TestPackOptionsForwarded 守住 pack 的常用开关确实传给 internal/pack。
func TestPackOptionsForwarded(t *testing.T) {
	orig := packFor
	t.Cleanup(func() { packFor = orig })

	var got pack.Options
	packFor = func(_ context.Context, o pack.Options) ([]release.Result, error) {
		got = o
		return nil, nil
	}
	opts := options.Default()
	opts.Reporter = nil
	opts.TestRoot = t.TempDir()

	runPack := func(args ...string) {
		t.Helper()
		base := []string{"pack", "--out-dir", t.TempDir(), "--cache-dir", t.TempDir()}
		if _, err := run(t, opts, append(base, args...)...); err != nil {
			t.Fatalf("pack %v: %v", args, err)
		}
	}

	runPack()
	if got.IncludeImages {
		t.Fatal("未传 --images 时不应包含镜像")
	}
	if !got.FetchImages {
		t.Fatal("--fetch-images 默认应为 true（仅在 --images 时生效）")
	}
	if got.Refresh {
		t.Fatal("--refresh 默认应为 false")
	}

	runPack("--images", "--refresh")
	if !got.IncludeImages {
		t.Fatal("传 --images 时应包含镜像")
	}
	if !got.Refresh {
		t.Fatal("传 --refresh 时应要求刷新组件缓存")
	}

	runPack("--images", "--fetch-images=false")
	if got.FetchImages {
		t.Fatal("--fetch-images=false 应关闭镜像拉取")
	}

	runPack("--offline")
	if !got.Offline {
		t.Fatal("传 --offline 时应进入只用缓存模式")
	}
}

func TestPackPrintsLoadAndVerifyHints(t *testing.T) {
	orig := packFor
	t.Cleanup(func() { packFor = orig })

	packFor = func(_ context.Context, _ pack.Options) ([]release.Result, error) {
		return []release.Result{{
			Archive: "/tmp/kubemono-test.tar.zst",
			Meta:    &artifact.Meta{Release: bom.MustLoad().Release, Arch: "amd64"},
			SHA256:  "sha256:abc",
			Size:    1,
		}}, nil
	}
	opts := options.Default()
	opts.Reporter = nil
	opts.TestRoot = t.TempDir()
	out, err := run(t, opts, "pack", "--out-dir", t.TempDir(), "--cache-dir", t.TempDir())
	if err != nil {
		t.Fatalf("pack: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kubemono bundle load /tmp/kubemono-test.tar.zst") {
		t.Fatalf("pack 输出缺少 load 提示:\n%s", out)
	}
	if !strings.Contains(out, "kubemono bundle verify /tmp/kubemono-test.tar.zst") {
		t.Fatalf("pack 输出缺少 verify 提示:\n%s", out)
	}
}

func TestListUsesBOM(t *testing.T) {
	dir := t.TempDir()
	bomPath := filepath.Join(dir, "bom.yaml")
	if err := os.WriteFile(bomPath, []byte(`release: v1.37.1-km.1
kubernetes: v1.37.1
arch: [amd64]
components:
  coredns:
    version: v1.14.6
    category: addon
`), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := options.Default()
	opts.TestRoot = t.TempDir()
	opts.Reporter = nil
	out, err := run(t, opts, "list", "--bom", bomPath)
	if err != nil {
		t.Fatalf("list --bom: %v\n%s", err, out)
	}
	for _, want := range []string{"coredns", "v1.14.6", "linux/amd64"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list 输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestLoadMissingArchiveSuggestsPack(t *testing.T) {
	opts := options.Default()
	opts.TestRoot = t.TempDir()
	_, err := run(t, opts, "load", filepath.Join(t.TempDir(), "missing.tar.zst"))
	if err == nil || !strings.Contains(err.Error(), "bundle pack") {
		t.Fatalf("缺失归档的错误应提示 pack，实际 err=%v", err)
	}
}

func TestBundleCommandSet(t *testing.T) {
	cmd := New(options.Default())
	got := make([]string, 0, len(cmd.Commands()))
	for _, c := range cmd.Commands() {
		got = append(got, c.Name())
	}
	slices.Sort(got)
	want := []string{"list", "load", "pack", "verify"}
	if !slices.Equal(got, want) {
		t.Fatalf("bundle 子命令 = %v，期望 %v", got, want)
	}
}

// TestDataDirRelocatesKubemonoRoot 固定全局 --data-dir：物料库落在自定义目录，
// 而不是默认 /usr/local/kubemono。
func TestDataDirRelocatesKubemonoRoot(t *testing.T) {
	dataDir := t.TempDir()
	opts := options.Default()
	opts.DataDir = dataDir
	opts.Reporter = nil
	archive, rel := testArchive(t, t.TempDir())
	if out, err := run(t, opts, "load", archive, "--no-arch"); err != nil {
		t.Fatalf("load: %v\n%s", err, out)
	}
	metaPath := filepath.Join(dataDir, "artifacts", rel, "meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("--data-dir 下缺少物料 meta: %v", err)
	}
}

// TestPackRejectsNonLinuxArch 锁定 --os 删除后的约束：目标 OS 只能是 linux，
// 非 linux 的 --arch 前缀必须在打包前被拒绝，而不是静默按 linux 打包。
func TestPackRejectsNonLinuxArch(t *testing.T) {
	opts := options.Default()
	opts.TestRoot = t.TempDir()
	out, err := run(t, opts, "pack", "--arch", "windows/amd64", "--offline", "--out-dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "只支持") {
		t.Fatalf("非 linux 目标必须被拒绝，got err=%v out=%s", err, out)
	}
}
