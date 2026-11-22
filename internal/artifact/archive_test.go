package artifact

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"kubemono/internal/bom"
	"kubemono/internal/util/fsutil"
)

func writeTestFile(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func testMaterial(t *testing.T) (string, *Meta) {
	t.Helper()
	root := t.TempDir()
	files := []FileEntry{}
	for _, f := range []struct {
		rel, body string
		mode      os.FileMode
		component string
		category  string
	}{
		{"bin/k8s/kubelet", "kubelet", 0o755, "kubelet", "k8s"},
	} {
		writeTestFile(t, root, f.rel, f.body, f.mode)
		files = append(files, FileEntry{
			Path: f.rel, SHA256: fsutil.SHA256Bytes([]byte(f.body)),
			Size: int64(len(f.body)), Mode: "0755", Component: f.component, Category: f.category,
		})
	}
	m := &Meta{
		FormatVersion: FormatVersion,
		Release:       bom.MustLoad().Release,
		Kubernetes:    bom.MustLoad().Kubernetes,
		Arch:          runtime.GOARCH,
		OS:            runtime.GOOS,
		Created:       time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Builder:       "test",
		Files:         files,
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("meta: %v", err)
	}
	return root, m
}

func writeTestArchive(t *testing.T, root string, meta *Meta) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.tar.zst")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteArchive(f, meta, root); err != nil {
		_ = f.Close()
		t.Fatalf("WriteArchive: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestArchiveInstallAndVerify(t *testing.T) {
	root, meta := testMaterial(t)
	archive := writeTestArchive(t, root, meta)

	got, err := ReadMetaFromArchive(archive)
	if err != nil {
		t.Fatalf("ReadMetaFromArchive: %v", err)
	}
	if got.Release != meta.Release || len(got.Files) != len(meta.Files) {
		t.Fatalf("meta mismatch: %+v", got)
	}

	dest := filepath.Join(t.TempDir(), "material")
	installed, err := Install(context.Background(), archive, dest, InstallOptions{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if installed.Release != meta.Release {
		t.Fatalf("installed release = %s", installed.Release)
	}
	info, err := os.Stat(filepath.Join(dest, "bin/k8s/kubelet"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("kubelet mode = %04o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dest, CompleteName)); err != nil {
		t.Fatalf("complete marker: %v", err)
	}

	report, err := VerifyDir(dest, VerifyOptions{NoArch: true})
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	if !report.OK() {
		t.Fatalf("VerifyDir report: %v", report.Error())
	}

	areport, err := VerifyArchive(context.Background(), archive, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyArchive: %v", err)
	}
	if !areport.OK() {
		t.Fatalf("VerifyArchive report: %v", areport.Error())
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	root, meta := testMaterial(t)
	archive := writeTestArchive(t, root, meta)
	dest := filepath.Join(t.TempDir(), "material")
	if _, err := Install(context.Background(), archive, dest, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	before := readCompleteMarker(t, dest)
	if _, err := Install(context.Background(), archive, dest, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	after := readCompleteMarker(t, dest)
	if !before.InstalledAt.Equal(after.InstalledAt) {
		t.Fatalf("second install should be a no-op: %v vs %v", before.InstalledAt, after.InstalledAt)
	}
}

func TestInstallRejectsDigestMismatch(t *testing.T) {
	root, meta := testMaterial(t)
	archive := writeTestArchive(t, root, meta)
	dest := filepath.Join(t.TempDir(), "material")
	_, err := Install(context.Background(), archive, dest, InstallOptions{
		ExpectedSHA256: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("digest mismatch must fail")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("dest should not exist after failure: %v", statErr)
	}
}

func TestVerifyArchiveDetectsCorruption(t *testing.T) {
	root, meta := testMaterial(t)
	archive := writeTestArchive(t, root, meta)
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(raw) / 2; i < len(raw)/2+8 && i < len(raw); i++ {
		raw[i] ^= 0xff
	}
	bad := filepath.Join(t.TempDir(), "bad.tar.zst")
	if err := os.WriteFile(bad, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyArchive(context.Background(), bad, VerifyOptions{})
	if err == nil && report.OK() {
		t.Fatal("corrupted archive must not verify")
	}
}

// readCompleteMarker 读取 .complete 标记。ReadComplete 已随死代码清理删除，
// 这里按测试需要直接解析，避免为了一个断言在生产包里留一个无人使用的导出函数。
func readCompleteMarker(t *testing.T, dir string) Complete {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, CompleteName))
	if err != nil {
		t.Fatal(err)
	}
	var c Complete
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInstallEnforcesExtractLimits(t *testing.T) {
	root, meta := testMaterial(t)
	archive := writeTestArchive(t, root, meta)
	dest := filepath.Join(t.TempDir(), "material")

	if _, err := Install(context.Background(), archive, dest, InstallOptions{MaxBytes: 1}); err == nil {
		t.Fatal("超过 MaxBytes 上限必须失败")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("超限后不应创建目标目录: %v", statErr)
	}

	writeTestFile(t, root, "bin/k8s/kubelet2", "x", 0o755)
	meta.Files = append(meta.Files, FileEntry{
		Path: "bin/k8s/kubelet2", SHA256: fsutil.SHA256Bytes([]byte("x")),
		Size: 1, Mode: "0755", Component: "kubelet", Category: "k8s",
	})
	twoFileArchive := writeTestArchive(t, root, meta)
	if _, err := Install(context.Background(), twoFileArchive, dest, InstallOptions{MaxFiles: 1}); err == nil {
		t.Fatal("超过 MaxFiles 上限必须失败")
	}
}

func TestInstallWritesComputedDigest(t *testing.T) {
	root, meta := testMaterial(t)
	archive := writeTestArchive(t, root, meta)
	digest, err := fsutil.SHA256File(archive)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "material")
	if _, err := Install(context.Background(), archive, dest, InstallOptions{ExpectedSHA256: digest}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := readCompleteMarker(t, dest).SHA256; got != digest {
		t.Fatalf("complete.sha256 = %s, want %s", got, digest)
	}
}

// TestStructuralEntryRules 锁定 VerifyArchive 与解包共用的结构规则。
func TestStructuralEntryRules(t *testing.T) {
	seen := map[string]bool{"bin/k8s/kubelet": true}
	ok := &tar.Header{Name: "bin/k8s/other", Typeflag: tar.TypeReg}
	if err := structuralEntryError(ok, seen); err != nil {
		t.Fatalf("合法条目被拒绝: %v", err)
	}
	cases := map[string]*tar.Header{
		"目录":   {Name: "bin/", Typeflag: tar.TypeDir},
		"软链":   {Name: "bin/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"路径穿越": {Name: "../evil", Typeflag: tar.TypeReg},
		"保留名":  {Name: MetaName, Typeflag: tar.TypeReg},
		"重复":   {Name: "bin/k8s/kubelet", Typeflag: tar.TypeReg},
	}
	for name, hdr := range cases {
		if err := structuralEntryError(hdr, seen); err == nil {
			t.Errorf("%s 条目应被拒绝: %q", name, hdr.Name)
		}
	}
}
