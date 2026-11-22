package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteFileAtomicCreatesParentsAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "config.yaml")
	if err := WriteFileAtomic(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("content = %q，期望 hello", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o，期望 0600", got)
	}
	// 临时文件必须被 rename 掉，目录里只应留下目标文件。
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("目录里有残留临时文件: %v", entries)
	}
}

func TestWriteFileAtomicOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := WriteFileAtomic(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("content = %q，期望 new", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %04o，期望 0640", got)
	}
}

func TestWriteFileAtomicEmptyContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := WriteFileAtomic(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("size = %d，期望 0", info.Size())
	}
}

func TestWriteFileIfChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")

	changed, err := WriteFileIfChanged(path, []byte("v1"), 0o600)
	if err != nil || !changed {
		t.Fatalf("首次写入 changed=%v err=%v，期望 true,nil", changed, err)
	}
	changed, err = WriteFileIfChanged(path, []byte("v1"), 0o600)
	if err != nil || changed {
		t.Fatalf("内容/权限相同时 changed=%v err=%v，期望 false,nil", changed, err)
	}
	changed, err = WriteFileIfChanged(path, []byte("v2"), 0o600)
	if err != nil || !changed {
		t.Fatalf("内容变化时 changed=%v err=%v，期望 true,nil", changed, err)
	}
	changed, err = WriteFileIfChanged(path, []byte("v2"), 0o640)
	if err != nil || !changed {
		t.Fatalf("权限变化时 changed=%v err=%v，期望 true,nil", changed, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %04o，期望 0640", got)
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !FileExists(file) {
		t.Fatal("FileExists 对普通文件应返回 true")
	}
	if FileExists(dir) {
		t.Fatal("FileExists 对目录应返回 false")
	}
	if FileExists(filepath.Join(dir, "missing")) {
		t.Fatal("FileExists 对不存在的路径应返回 false")
	}
}

func TestSHA256FileAndBytes(t *testing.T) {
	const want = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := SHA256Bytes([]byte("hello")); got != want {
		t.Fatalf("SHA256Bytes = %s，期望 %s", got, want)
	}
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("SHA256File = %s，期望 %s", got, want)
	}
	if _, err := SHA256File(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("缺失文件应报错")
	}
}

func TestCopyFilePreservesMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "sub", "dst")
	if err := os.WriteFile(src, []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := CopyFile(src, dst); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data" {
		t.Fatalf("content = %q，期望 data", data)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %04o，期望 0640", got)
	}
	if err := CopyFile(filepath.Join(dir, "missing"), dst); err == nil {
		t.Fatal("缺失源文件应报错")
	}
}

func TestBackup(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "kube-apiserver.yaml")
	if err := os.WriteFile(src, []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "backup")
	dst, err := Backup(src, root, "20260101T000000Z")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if want := filepath.Join(root, "20260101T000000Z", "kube-apiserver.yaml"); dst != want {
		t.Fatalf("dst = %s，期望 %s", dst, want)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "manifest" {
		t.Fatalf("content = %q，期望 manifest", data)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o，期望 0600", got)
	}
	if _, err := Backup(dir, root, "x"); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("备份目录应报错，实际 %v", err)
	}
	if _, err := Backup(filepath.Join(dir, "missing"), root, "x"); err == nil {
		t.Fatal("缺失源文件应报错")
	}
}

func TestTimestamp(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	got := Timestamp(time.Date(2026, 1, 2, 3, 4, 5, 0, loc))
	if want := "20260101T190405Z"; got != want {
		t.Fatalf("Timestamp = %s，期望 %s", got, want)
	}
}

func TestRemoveIfExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIfExists(path); err != nil {
		t.Fatalf("RemoveIfExists: %v", err)
	}
	if FileExists(path) {
		t.Fatal("文件应已删除")
	}
	if err := RemoveIfExists(path); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
}

func TestPruneTimestampedDirs(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 13; i++ {
		name := time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format("20060102T150405Z")
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PruneTimestampedDirs(root, 10); err != nil {
		t.Fatalf("PruneTimestampedDirs: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if e.Name() == "keep-me" {
			continue
		}
		count++
	}
	if count != 10 {
		t.Fatalf("保留目录数 = %d, want 10", count)
	}
	if _, err := os.Stat(filepath.Join(root, "keep-me")); err != nil {
		t.Fatal("非时间戳目录不应被删除")
	}
}
