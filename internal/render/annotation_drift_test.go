package render

import (
	"os"
	"path/filepath"
	"testing"
)

func manifestWithRestart(ts string) []byte {
	return []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  annotations:\n    kubemono.io/restarted-at: \"" + ts + "\"\n")
}

func TestComputeChangesIgnoresRestartedAt(t *testing.T) {
	old := &Spec{Files: []File{{
		Path:    "/etc/kubernetes/manifests/kube-apiserver.yaml",
		Content: manifestWithRestart("2024-01-01T00:00:00Z"),
		Mode:    0o644,
	}}}
	newer := &Spec{Files: []File{{
		Path:    "/etc/kubernetes/manifests/kube-apiserver.yaml",
		Content: manifestWithRestart("2025-02-02T00:00:00Z"),
		Mode:    0o644,
	}}}
	if changes := ComputeChanges(old, newer); len(changes) != 0 {
		t.Fatalf("仅 restarted-at 不同不应算变更，实际 %v", changes)
	}
	// 其它内容变化仍然必须被发现。
	newer.Files[0].Content = append(newer.Files[0].Content, []byte("# changed\n")...)
	if changes := ComputeChanges(old, newer); len(changes) != 1 {
		t.Fatalf("真实内容变化必须被发现，实际 %v", changes)
	}
}

func TestDriftIgnoresRestartedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kube-apiserver.yaml")
	if err := os.WriteFile(path, manifestWithRestart("2025-02-02T00:00:00Z"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := &Spec{Files: []File{{
		Path:    path,
		Content: manifestWithRestart("2024-01-01T00:00:00Z"),
		Mode:    0o644,
	}}}
	items, err := Drift(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("仅 restarted-at 不同不应算漂移，实际 %v", items)
	}
	spec.Files[0].Content = manifestWithRestart("2024-01-01T00:00:00Z")
	spec.Files[0].Content = append(spec.Files[0].Content, []byte("# real change\n")...)
	items, err = Drift(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("真实内容变化必须被发现，实际 %v", items)
	}
}
