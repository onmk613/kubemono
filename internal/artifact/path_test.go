package artifact

import (
	"path/filepath"
	"testing"
)

func TestValidateBundlePath(t *testing.T) {
	valid := []string{"meta.json", "bin/k8s/kubelet", "images/blobs/sha256/abc"}
	for _, p := range valid {
		if err := ValidateBundlePath(p); err != nil {
			t.Errorf("%q should be valid: %v", p, err)
		}
	}
	invalid := []string{"", "/abs", "../x", "a/../b", "a//b", "./a", `a\b`, "C:/x", "a/./b"}
	for _, p := range invalid {
		if err := ValidateBundlePath(p); err == nil {
			t.Errorf("%q should be invalid", p)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	got, err := SafeJoin(root, "bin/k8s/kubelet")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "bin", "k8s", "kubelet"); got != want {
		t.Fatalf("SafeJoin = %s, want %s", got, want)
	}
	if _, err := SafeJoin(root, "../escape"); err == nil {
		t.Fatal("traversal must fail")
	}
}
