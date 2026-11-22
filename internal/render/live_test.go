package render

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func specOf(files ...File) *Spec {
	s := &Spec{Node: "n1"}
	for _, f := range files {
		s.Add(f)
	}
	s.Sort()
	return s
}

// TestOverlayLiveRewritesDrift: 快照内容与期望一致、但磁盘被改过时，
// ComputeChanges 必须报告一次修改（否则漂移会被静默跳过）。
func TestOverlayLiveRewritesDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, "drifted", 0o644)

	desired := specOf(File{Path: path, Content: []byte("desired"), Mode: 0o644, Component: "kube-router"})
	base := specOf(File{Path: path, Content: []byte("desired"), Mode: 0o644, Component: "kube-router"})

	changes := ComputeChanges(OverlayLive(base, desired), desired)
	if len(changes) != 1 || changes[0].Kind != ChangeModified {
		t.Fatalf("changes = %+v, want 1 modified", changes)
	}
}

// TestOverlayLiveNoChangeWhenIdentical: 完全相同时不产生任何变更（幂等）。
func TestOverlayLiveNoChangeWhenIdentical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, "same", 0o600)

	desired := specOf(File{Path: path, Content: []byte("same"), Mode: 0o600, Component: "kube-router"})
	base := specOf(File{Path: path, Content: []byte("same"), Mode: 0o600, Component: "kube-router"})

	if changes := ComputeChanges(OverlayLive(base, desired), desired); len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

// TestOverlayLiveDetectsModeDrift: 只有权限被放宽也必须重写（安全修复）。
func TestOverlayLiveDetectsModeDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.conf")
	writeFile(t, path, "same", 0o644)

	desired := specOf(File{Path: path, Content: []byte("same"), Mode: 0o600, Component: "kube-router", Secret: true})
	changes := ComputeChanges(OverlayLive(nil, desired), desired)
	if len(changes) != 1 || changes[0].Kind != ChangeModified {
		t.Fatalf("changes = %+v, want 1 modified", changes)
	}
}

// TestOverlayLiveMissingFileIsAdded: 磁盘上不存在的文件应判定为新增，
// 这样 ComputeChanges 不会去备份一个不存在的文件。
func TestOverlayLiveMissingFileIsAdded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.yaml")
	desired := specOf(File{Path: path, Content: []byte("x"), Mode: 0o644, Component: "kube-router"})

	changes := ComputeChanges(OverlayLive(nil, desired), desired)
	if len(changes) != 1 || changes[0].Kind != ChangeAdded {
		t.Fatalf("changes = %+v, want 1 added", changes)
	}
}

// TestOverlayLiveKeepsStaleSnapshotEntries: 快照里有、期望里没有的文件必须保留，
// 从而被判定为 Removed（这一代不需要的文件）。
func TestOverlayLiveKeepsStaleSnapshotEntries(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "old.yaml")
	writeFile(t, stale, "old", 0o644)

	base := specOf(File{Path: stale, Content: []byte("old"), Mode: 0o644, Component: "kube-router"})
	desired := specOf()

	changes := ComputeChanges(OverlayLive(base, desired), desired)
	if len(changes) != 1 || changes[0].Kind != ChangeRemoved {
		t.Fatalf("changes = %+v, want 1 removed", changes)
	}
}

// TestReadLive: 读不到时返回 false，读到内容与权限时返回 live 版本。
func TestReadLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if _, ok := ReadLive(File{Path: path, Content: []byte("d"), Mode: 0o600}); ok {
		t.Fatal("文件不存在时应返回 false")
	}
	writeFile(t, path, "live", 0o640)
	live, ok := ReadLive(File{Path: path, Content: []byte("desired"), Mode: 0o600})
	if !ok {
		t.Fatal("文件存在时应返回 true")
	}
	if string(live.Content) != "live" || live.Mode != 0o640 {
		t.Fatalf("live = %q %04o", live.Content, live.Mode)
	}
}

// TestSelectComponent: 只挑出指定组件的文件，顺序稳定。
func TestSelectComponent(t *testing.T) {
	s := specOf(
		File{Path: "/b", Component: "kube-router"},
		File{Path: "/a", Component: "kubelet"},
		File{Path: "/c", Component: "kube-router"},
	)
	got := SelectComponent(s, "kube-router")
	if len(got.Files) != 2 || got.Files[0].Path != "/b" || got.Files[1].Path != "/c" {
		t.Fatalf("files = %+v", got.Files)
	}
	if len(SelectComponent(s, "nope").Files) != 0 {
		t.Fatal("没有该组件时应返回空")
	}
}
