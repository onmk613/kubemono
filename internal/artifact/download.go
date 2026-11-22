package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"kubemono/internal/bom"
	"kubemono/internal/util/fsutil"
)

// Downloader 拉取一个 URL。所有网络访问都必须经过这个接口，测试用内存/本地
// 文件的 fake 覆盖，真实实现是 HTTPDownloader。
type Downloader interface {
	// Download 返回 url 的响应体，由调用方负责关闭。
	Download(ctx context.Context, url string) (io.ReadCloser, error)
}

// DefaultDownloadTimeout 限制单次下载的时长。
const DefaultDownloadTimeout = 15 * time.Minute

// downloadHeaderTimeout 限制等待响应头的时间。
const downloadHeaderTimeout = 60 * time.Second

// HTTPDownloader 是生产环境的 Downloader。
// 行为约定：
//   - 只接受 http/https；
//   - 不发送 If-None-Match/If-Modified-Since：本地缓存的新鲜度由文件是否存在决定；
//   - 非 200 一律报错（含 3xx：重定向由 http.Client 自动跟随，跟随结束后仍是
//     非 200 才算失败）；
//   - 重定向跟随由标准库默认策略完成（最多 10 跳）。
type HTTPDownloader struct {
	// Client 是 HTTP 客户端；为 nil 时使用带 DefaultDownloadTimeout 的客户端。
	Client *http.Client
}

// NewHTTPDownloader 返回带合理超时的下载器。
func NewHTTPDownloader() *HTTPDownloader {
	return &HTTPDownloader{Client: defaultHTTPClient()}
}

func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = downloadHeaderTimeout
	transport.Proxy = http.ProxyFromEnvironment
	return &http.Client{
		Timeout:   DefaultDownloadTimeout,
		Transport: transport,
	}
}

func (d *HTTPDownloader) client() *http.Client {
	if d != nil && d.Client != nil {
		return d.Client
	}
	return defaultHTTPClient()
}

// Download 实现 Downloader。
func (d *HTTPDownloader) Download(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("artifact: 下载地址 %q 非法: %w", rawURL, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("artifact: 下载地址 %q 的协议 %q 不受支持（只支持 http/https）", rawURL, u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("artifact: 构造下载请求失败: %w", err)
	}
	ua := "kubemono"
	if b, err := bom.Load(); err == nil {
		ua += "/" + b.Release
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifact: 下载 %s 失败（网络不可达或超时）: %w", rawURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		status := resp.Status
		if status == "" {
			status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		_ = resp.Body.Close()
		return nil, fmt.Errorf("artifact: 下载 %s 返回 HTTP %s（期望 200）", rawURL, status)
	}
	return resp.Body, nil
}

// DownloadToFile 把 url 下载到 dest，返回 "sha256:<hex>" 与字节数。
// 先写临时文件再原子改名；失败时不会留下半截文件。
func DownloadToFile(ctx context.Context, d Downloader, rawURL, dest string) (string, int64, error) {
	if d == nil {
		return "", 0, fmt.Errorf("artifact: 缺少 Downloader，无法下载 %s", rawURL)
	}
	if strings.TrimSpace(dest) == "" {
		return "", 0, fmt.Errorf("artifact: 下载目标路径为空")
	}
	rc, err := d.Download(ctx, rawURL)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rc.Close() }()
	// 摘要与落盘同步进行：字节同时喂给哈希与临时文件，写完即可算出 sha256，
	// 不需要下载完再读一遍盘。
	h := sha256.New()
	var n int64
	if err := fsutil.WriteStreamingAtomic(dest, 0o644, func(w io.Writer) error {
		var err error
		n, err = io.Copy(io.MultiWriter(w, h), rc)
		return err
	}); err != nil {
		return "", 0, fmt.Errorf("artifact: 下载 %s 失败: %w", rawURL, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

// ComponentCachePath 返回组件归档的缓存路径。
// cacheDir 是已经按 release 隔离的缓存目录；布局为
// <cacheDir>/<component>/<arch>/<source-hash>/<URL 的最后一个路径段>。
//
// 按组件 + 架构分目录，避免 kubelet 这类文件名不含架构的下载互相覆盖；
// key 同时包含展开后的完整 URL 与组件版本：镜像站常用不含 {version} 的稳定
// URL（例如 .../containerd-linux-amd64.tar.gz），只 hash URL 会在版本覆盖后
// 静默复用旧二进制。
func ComponentCachePath(cacheDir, component, version, arch, rawURL string) string {
	base := ""
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		base = path.Base(u.Path)
	}
	if base == "" || base == "." || base == "/" {
		base = component
	}
	if arch == "" {
		arch = "unknown"
	}
	sum := sha256.Sum256([]byte(rawURL + "\x00" + version))
	source := hex.EncodeToString(sum[:])[:12]
	return filepath.Join(cacheDir, component, arch, source, base)
}
