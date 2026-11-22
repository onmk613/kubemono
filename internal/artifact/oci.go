package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// OCI 镜像 layout 与注解名。
const (
	// OCILayoutFile 是 layout 标记文件（"imageLayoutVersion": "1.0.0"）。
	OCILayoutFile = "oci-layout"
	// OCIIndexFile 是 layout 的镜像索引。
	OCIIndexFile = "index.json"
	// OCIRefNameAnnotation 携带 index 条目的镜像引用。
	OCIRefNameAnnotation = "org.opencontainers.image.ref.name"
)

const (
	ociImageManifestMediaType    = "application/vnd.oci.image.manifest.v1+json"
	dockerImageManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
)

// ImageImporter 把磁盘上的 OCI layout 中的镜像导入容器运行时。
// 本包不依赖任何运行时：containerd/ctr 的实现属于另一个包，通过这个接口注入。
type ImageImporter interface {
	// Import 把 refs（为空时是 layout 中的全部镜像）从 layoutDir 导入 dst 标识的运行时。
	Import(ctx context.Context, layoutDir string, refs []string) error
}

// LayoutReferences 列出 OCI layout 中记录的镜像引用。
// 结果去重且排序；缺少 ref.name 注解时退化为摘要引用；只返回镜像 manifest。
func LayoutReferences(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, OCIIndexFile))
	if err != nil {
		return nil, fmt.Errorf("artifact: 读取 %s 的 index 失败: %w", dir, err)
	}
	var index struct {
		Manifests []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("artifact: 解析 %s 的 index 失败: %w", dir, err)
	}
	seen := make(map[string]bool, len(index.Manifests))
	out := make([]string, 0, len(index.Manifests))
	for _, d := range index.Manifests {
		// index / attestation 条目不是可导入的镜像。
		if d.MediaType != ociImageManifestMediaType && d.MediaType != dockerImageManifestMediaType {
			continue
		}
		ref := d.Annotations[OCIRefNameAnnotation]
		if ref == "" {
			ref = d.Digest
		}
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	sort.Strings(out)
	return out, nil
}

// LoadImages 把 OCI layout 导入容器运行时。
// 本包与运行时无关：containerd/ctr 的实现由调用方注入。
func LoadImages(ctx context.Context, dir string, refs []string, dst string, importer ImageImporter) error {
	if importer == nil {
		return fmt.Errorf("artifact: 缺少 ImageImporter")
	}
	if _, err := os.Stat(filepath.Join(dir, OCILayoutFile)); err != nil {
		return fmt.Errorf("artifact: %s 不是 OCI layout: %w", dir, err)
	}
	if len(refs) == 0 {
		var err error
		refs, err = LayoutReferences(dir)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := importer.Import(ctx, dir, refs); err != nil {
		return fmt.Errorf("artifact: 导入镜像到 %s 失败: %w", dst, err)
	}
	return nil
}
