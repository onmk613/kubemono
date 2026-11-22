package runtime

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CTRImageImporter 通过 containerd 的 ctr 二进制导入 OCI layout。
// kubemono 不直接链接 containerd 客户端，而是复用节点上已经安装的 ctr。
// 造包（internal/pack）与节点侧（init/join 的 runtime phase）共用它。
type CTRImageImporter struct {
	// Namespace 是 containerd 命名空间（默认 k8s.io）。
	Namespace string
}

// Import 实现 artifact.ImageImporter。
// ctr images import 只接受归档（docker-archive / oci-archive），不接受
// OCI layout 目录，因此先把 layout 打成 tar 再导入。
func (c CTRImageImporter) Import(ctx context.Context, layoutDir string, _ []string) error {
	path, err := exec.LookPath("ctr")
	if err != nil {
		return fmt.Errorf("找不到 ctr：请先确认 containerd 已安装并启动；%w", err)
	}
	ns := c.Namespace
	if ns == "" {
		ns = "k8s.io"
	}
	archive, cleanup, err := PackOCILayout(layoutDir)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, path, "-n", ns, "images", "import", archive)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ctr -n %s images import %s 失败: %w: %s", ns, archive, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PackOCILayout 把 OCI layout 目录打成临时 tar（oci-archive），返回路径与清理函数。
func PackOCILayout(dir string) (string, func(), error) {
	f, err := os.CreateTemp("", "kubemono-oci-*.tar")
	if err != nil {
		return "", func() {}, fmt.Errorf("创建临时归档失败: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	tw := tar.NewWriter(f)
	walkErr := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		_, err = io.Copy(tw, src)
		return err
	})
	if walkErr == nil {
		walkErr = tw.Close()
	} else {
		_ = tw.Close()
	}
	if err := f.Close(); err != nil && walkErr == nil {
		walkErr = err
	}
	if walkErr != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("打包 OCI layout %s 失败: %w", dir, walkErr)
	}
	return f.Name(), cleanup, nil
}
