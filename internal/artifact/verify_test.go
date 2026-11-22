package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func installTestMaterial(t *testing.T) (string, *Meta) {
	t.Helper()
	root, meta := testMaterial(t)
	archive := writeTestArchive(t, root, meta)
	dest := filepath.Join(t.TempDir(), "material")
	if _, err := Install(context.Background(), archive, dest, InstallOptions{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	return dest, meta
}

func TestVerifyDirMissing(t *testing.T) {
	dest, _ := installTestMaterial(t)
	if err := os.Remove(filepath.Join(dest, "bin/k8s/kubelet")); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyDir(dest, VerifyOptions{NoArch: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() || len(report.Missing) != 1 || report.Missing[0] != "bin/k8s/kubelet" {
		t.Fatalf("missing not reported: %+v", report)
	}
}

func TestVerifyDirHashAndSize(t *testing.T) {
	dest, _ := installTestMaterial(t)
	// 同长度、不同内容：只应命中哈希，不应命中大小。
	writeTestFile(t, dest, "bin/k8s/kubelet", "KUBELET", 0o755)
	report, err := VerifyDir(dest, VerifyOptions{NoArch: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() || len(report.HashMismatches) != 1 {
		t.Fatalf("hash mismatch not reported: %+v", report)
	}
	if len(report.SizeMismatches) != 0 {
		t.Fatalf("unexpected size mismatch: %+v", report.SizeMismatches)
	}
	// --no-hash 只跳过哈希，大小/权限仍检查。
	report, err = VerifyDir(dest, VerifyOptions{NoArch: true, NoHash: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("no-hash should pass: %v", report.Error())
	}
}

func TestVerifyDirModeAndUnknown(t *testing.T) {
	dest, _ := installTestMaterial(t)
	if err := os.Chmod(filepath.Join(dest, "bin/k8s/kubelet"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dest, "bin/k8s/extra", "extra", 0o755)
	report, err := VerifyDir(dest, VerifyOptions{NoArch: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("report should fail")
	}
	if len(report.ModeMismatches) != 1 || report.ModeMismatches[0].Path != "bin/k8s/kubelet" {
		t.Fatalf("mode mismatch not reported: %+v", report.ModeMismatches)
	}
	if len(report.Unknown) != 1 || report.Unknown[0] != "bin/k8s/extra" {
		t.Fatalf("unknown not reported: %+v", report.Unknown)
	}
}
