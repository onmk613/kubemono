package identity

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

// writeFile 以 0600 权限写入文件，用于植入损坏或恶意输入。
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func sampleNodeConfig() *clusterconfig.NodeConfig {
	return &clusterconfig.NodeConfig{
		ClusterID:   "prod",
		NodeName:    "w1",
		Address:     "10.0.0.11",
		Roles:       []clusterconfig.Role{clusterconfig.RoleWorker},
		Release:     "v1.31.4-km.1",
		InstalledAt: time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2024, 5, 2, 10, 0, 0, 0, time.UTC),
	}
}

func TestStoreLoadMissing(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	if store.Exists() {
		t.Errorf("Exists() = true on an empty directory")
	}
	n, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for a missing file", err)
	}
	if n != nil {
		t.Errorf("Load() = %+v, want nil for a missing file", n)
	}
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kubemono")
	store := &Store{Dir: dir}
	want := sampleNodeConfig()

	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if !store.Exists() {
		t.Fatalf("Exists() = false after Save()")
	}

	// 权限：目录 0700、文件 0600。
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir) error = %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
	fi, err := os.Stat(store.FilePath())
	if err != nil {
		t.Fatalf("Stat(file) error = %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.NodeName != want.NodeName || got.ClusterID != want.ClusterID || got.Address != want.Address {
		t.Errorf("Load() = %+v, want identity of %+v", got, want)
	}
	if len(got.Roles) != 1 || got.Roles[0] != clusterconfig.RoleWorker {
		t.Errorf("Load() roles = %v, want [worker]", got.Roles)
	}
	if got.Release != want.Release {
		t.Errorf("Load() release = %q, want %q", got.Release, want.Release)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("Load() updatedAt = %v, want %v", got.UpdatedAt, want.UpdatedAt)
	}
}

func TestStoreSaveIsAtomicAndRepairsPerms(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	first := sampleNodeConfig()
	if err := store.Save(first); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// 模拟被外部改成 0644 的文件。
	if err := os.Chmod(store.FilePath(), 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	second := sampleNodeConfig()
	second.NodeName = "w2"
	second.Address = "10.0.0.12"
	if err := store.Save(second); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.NodeName != "w2" {
		t.Errorf("NodeName = %q, want w2", got.NodeName)
	}
	fi, err := os.Stat(store.FilePath())
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600 after repair", perm)
	}

	// 目录里不应留下临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover temporary file %q in %s", e.Name(), dir)
		}
	}
}

func TestStoreSaveRejectsBadInput(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	if err := store.Save(nil); err == nil {
		t.Errorf("Save(nil) = nil error, want an error")
	}
	if err := store.Save(&clusterconfig.NodeConfig{ClusterID: "prod"}); err == nil {
		t.Errorf("Save(without nodeName) = nil error, want an error")
	}
	if store.Exists() {
		t.Errorf("Exists() = true after a rejected Save()")
	}
}

func TestStoreLoadBrokenFile(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	if err := writeFile(store.FilePath(), "nodeName: \"\"\n"); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Errorf("Load() = nil error, want an error for a node config without nodeName")
	}

	if err := writeFile(store.FilePath(), "kind: [not, a, string\n"); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Errorf("Load() = nil error, want a parse error")
	}
}

func TestStoreRejectsSymlinks(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere.yaml")
	if err := writeFile(target, "nodeName: w1\n"); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}

	dir := filepath.Join(base, "kubemono")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	store := &Store{Dir: dir}
	if err := os.Symlink(target, store.FilePath()); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	if store.Exists() {
		t.Errorf("Exists() = true for a symlink, want false")
	}
	if _, err := store.Load(); err == nil {
		t.Errorf("Load() = nil error for a symlink, want a refusal")
	}
	if err := store.Save(sampleNodeConfig()); err == nil {
		t.Errorf("Save() = nil error for a symlink target, want a refusal")
	}
	// 目标文件必须保持原样。
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(data) != "nodeName: w1\n" {
		t.Errorf("symlink target was modified: %q", data)
	}
}

func TestStoreRejectsSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	store := &Store{Dir: link}
	if err := store.Save(sampleNodeConfig()); err == nil {
		t.Errorf("Save() = nil error for a symlinked directory, want a refusal")
	}
	if _, err := os.Stat(filepath.Join(real, NodeConfigFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("node.yaml was written through the symlinked directory: err = %v", err)
	}
}

func TestStoreRemove(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	if err := store.Remove(); err != nil {
		t.Errorf("Remove() on a missing file = %v, want nil (idempotent)", err)
	}
	if err := store.Save(sampleNodeConfig()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if store.Exists() {
		t.Errorf("Exists() = true after Remove()")
	}
	n, err := store.Load()
	if err != nil || n != nil {
		t.Errorf("Load() after Remove() = (%+v, %v), want (nil, nil)", n, err)
	}
}

func TestStoreSaveCreatesNestedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested", "kubemono")
	store := &Store{Dir: dir}
	if err := store.Save(sampleNodeConfig()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if !store.Exists() {
		t.Fatalf("Exists() = false after Save() into a nested directory")
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
}

func TestStoreSaveParentIsFile(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := writeFile(blocker, "not a directory\n"); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}
	store := &Store{Dir: filepath.Join(blocker, "kubemono")}
	if err := store.Save(sampleNodeConfig()); err == nil {
		t.Errorf("Save() = nil error under a file parent, want an error")
	}
}

func TestStoreLoadLimitAndNonRegular(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	if err := os.Mkdir(store.FilePath(), 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Errorf("Load() = nil error for a directory, want an error")
	}
}

func TestStoreDefaultDir(t *testing.T) {
	store := &Store{}
	if got, want := store.FilePath(), constants.NodeConfigFile; got != want {
		t.Errorf("FilePath() = %q, want %q", got, want)
	}
	if got, want := store.dir(), constants.KubemonoDir; got != want {
		t.Errorf("dir() = %q, want %q", got, want)
	}
	if got, want := NewStore("").FilePath(), constants.NodeConfigFile; got != want {
		t.Errorf("NewStore(\"\").FilePath() = %q, want %q", got, want)
	}
	if got := NewStore("/tmp/x").FilePath(); got != filepath.Join("/tmp/x", NodeConfigFileName) {
		t.Errorf("NewStore(/tmp/x).FilePath() = %q", got)
	}
}

// Exists/Remove 只被测试使用，放在测试文件里，避免生产包暴露未使用的 API。
func (s *Store) Exists() bool {
	fi, err := os.Lstat(s.FilePath())
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

func (s *Store) Remove() error {
	path := s.FilePath()
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}
