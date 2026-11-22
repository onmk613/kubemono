package pki

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsurePrivateDirDoesNotChmodExistingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("已存在的目录权限被改成 %04o，期望保持 0755", got)
	}
}

func TestEnsurePrivateDirCreatesWith0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	if err := ensurePrivateDir(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("新建目录权限 = %04o，期望 0700", got)
	}
}

func TestWriteCertAndKeyRejectsNilCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := WriteCertAndKey(certPath, keyPath, nil, nil); err == nil {
		t.Fatal("nil 证书必须被拒绝")
	}
	if _, err := os.Stat(certPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("拒绝后不应留下证书文件: %v", err)
	}
}
