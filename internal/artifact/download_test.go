package artifact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/util/fsutil"
)

func TestDownloadToFile(t *testing.T) {
	body := []byte("archive bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bundle.tar.zst" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "nested", "bundle.tar.zst")
	sum, n, err := DownloadToFile(context.Background(), NewHTTPDownloader(), srv.URL+"/bundle.tar.zst", dest)
	if err != nil {
		t.Fatalf("DownloadToFile: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("size = %d, want %d", n, len(body))
	}
	if sum != fsutil.SHA256Bytes(body) {
		t.Fatalf("digest = %s", sum)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(body) {
		t.Fatalf("content = %q, err = %v", got, err)
	}
}

func TestDownloadToFileHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, _, err := DownloadToFile(context.Background(), NewHTTPDownloader(), srv.URL+"/missing", filepath.Join(t.TempDir(), "x"))
	if err == nil {
		t.Fatal("404 must fail")
	}
}

// TestComponentCachePathKeyedBySource 守住“改 URL / 改版本不会命中旧缓存”。
// runc 的 basename 固定是 runc.amd64，版本只在 URL 路径里；只按 basename
// 缓存会把旧二进制静默复用。
func TestComponentCachePathKeyedBySource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "release")
	oldURL := "https://github.com/opencontainers/runc/releases/download/v1.5.1/runc.amd64"
	newURL := "https://github.com/opencontainers/runc/releases/download/v1.5.2/runc.amd64"
	mirrorURL := "https://mirror.example/runc/v1.5.1/runc.amd64"

	oldPath := ComponentCachePath(root, "runc", "v1.5.2", "amd64", oldURL)
	newPath := ComponentCachePath(root, "runc", "v1.5.2", "amd64", newURL)
	mirrorPath := ComponentCachePath(root, "runc", "v1.5.2", "amd64", mirrorURL)

	if oldPath == newPath {
		t.Fatalf("版本变化不应复用同一缓存路径: %s", oldPath)
	}
	if oldPath == mirrorPath {
		t.Fatalf("镜像 URL 变化不应复用同一缓存路径: %s", oldPath)
	}
	if got := ComponentCachePath(root, "runc", "v1.5.2", "amd64", oldURL); got != oldPath {
		t.Fatalf("同一 URL 的缓存路径应稳定: %s != %s", got, oldPath)
	}
	if filepath.Base(oldPath) != "runc.amd64" {
		t.Fatalf("缓存文件名应保留 basename: %s", oldPath)
	}
	if !strings.Contains(oldPath, filepath.Join("runc", "amd64")) {
		t.Fatalf("缓存路径应包含 component/arch: %s", oldPath)
	}
}
