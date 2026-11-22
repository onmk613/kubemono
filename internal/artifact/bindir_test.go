package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLocatePrefersManifestAndFallsBack(t *testing.T) {
	root := t.TempDir()
	direct := filepath.Join(root, BundlePath("k8s", "kubelet"))
	write(t, direct)
	ref, err := LocateBinary(root, nil, "kubelet", "kubelet", "k8s")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if ref.Path != direct {
		t.Fatalf("path = %s, want %s", ref.Path, direct)
	}

	// 非 canonical 的 bin/<other>/<name> 不被 glob 兜底：打包错误必须在安装前暴露。
	other := filepath.Join(root, DirBin, "other", "ctr")
	write(t, other)
	if _, err := LocateBinary(root, nil, "ctr", "ctr", "runtime"); err == nil {
		t.Fatal("非 canonical 布局必须报错")
	}

	// manifest 指向的文件不存在时，回退到类别路径。
	m := &Meta{Files: []FileEntry{{Path: "bin/k8s/missing", Component: "kubelet", SHA256: "sha256:x"}}}
	ref, err = LocateBinary(root, m, "kubelet", "kubelet", "k8s")
	if err != nil {
		t.Fatalf("fallback Locate: %v", err)
	}
	if ref.Path != direct {
		t.Fatalf("path = %s, want %s", ref.Path, direct)
	}
}

func TestLocateMissing(t *testing.T) {
	if _, err := LocateBinary(t.TempDir(), nil, "kubelet", "kubelet", "k8s"); err == nil {
		t.Fatal("missing binary must be an error")
	}
}
