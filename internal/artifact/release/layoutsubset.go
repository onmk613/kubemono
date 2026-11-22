package release

import (
	"fmt"
	. "kubemono/internal/artifact" //nolint:staticcheck // ST1001: 共享类型很多，点导入让发布侧代码保持可读
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// SelectLayoutImagesForPlatform 写出限定单一平台的过滤副本。
// 例如 "linux/amd64"。
// 多架构打包时每份物料归档只应带本架构的镜像：共享的 OCI layout 里可能同时
// 存在 amd64 与 arm64 的 manifest，不过滤会让两份包都膨胀一倍。没有记录
// platform 的条目（旧 layout）按保留处理，避免误删。
func SelectLayoutImagesForPlatform(srcDir, dstDir string, keep func(reference string) bool, platform string) ([]string, error) {
	if srcDir == "" || dstDir == "" {
		return nil, fmt.Errorf("artifact: OCI layout 的源目录或目标目录为空")
	}
	if _, err := os.Stat(filepath.Join(srcDir, OCILayoutFile)); err != nil {
		return nil, fmt.Errorf("artifact: %s 不是 OCI layout: %w", srcDir, err)
	}
	src, err := layout.FromPath(srcDir)
	if err != nil {
		return nil, fmt.Errorf("artifact: 打开 OCI layout %s 失败: %w", srcDir, err)
	}
	dst, err := layout.Write(dstDir, empty.Index)
	if err != nil {
		return nil, fmt.Errorf("artifact: 创建 OCI layout %s 失败: %w", dstDir, err)
	}
	idx, err := src.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("artifact: 读取 %s 的 index 失败: %w", srcDir, err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("artifact: 解析 %s 的 index 失败: %w", srcDir, err)
	}
	var copied []string
	for _, d := range manifest.Manifests {
		if d.MediaType != types.OCIManifestSchema1 && d.MediaType != types.DockerManifestSchema2 {
			// 跳过 index/attestation 条目：它们不是可导入的镜像。
			continue
		}
		ref := d.Annotations[OCIRefNameAnnotation]
		if ref == "" {
			ref = d.Digest.String()
		}
		if keep != nil && !keep(ref) {
			continue
		}
		if platform != "" && d.Platform != nil && d.Platform.OS+"/"+d.Platform.Architecture != platform {
			continue
		}
		img, err := src.Image(d.Digest)
		if err != nil {
			return nil, fmt.Errorf("artifact: 读取镜像 %s（%s）失败: %w", ref, d.Digest, err)
		}
		opts := []layout.Option{layout.WithAnnotations(map[string]string{OCIRefNameAnnotation: ref})}
		if d.Platform != nil {
			opts = append(opts, layout.WithPlatform(*d.Platform))
		}
		if err := dst.AppendImage(img, opts...); err != nil {
			return nil, fmt.Errorf("artifact: 写入镜像 %s 到 %s 失败: %w", ref, dstDir, err)
		}
		copied = append(copied, ref)
	}
	return copied, nil
}
