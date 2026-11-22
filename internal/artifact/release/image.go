package release

import (
	"context"

	"encoding/json"
	"fmt"
	. "kubemono/internal/artifact" //nolint:staticcheck // ST1001: 共享类型很多，点导入让发布侧代码保持可读
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/google/go-containerregistry/pkg/v1/types"
)

// parsePlatform 把 "linux/amd64" 转换成 v1.Platform。
func parsePlatform(s string) (*v1.Platform, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("artifact: 平台 %q 非法，应为 os/arch 或 os/arch/variant", s)
	}
	p := &v1.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) > 2 {
		p.Variant = parts[2]
	}
	if len(parts) > 3 {
		return nil, fmt.Errorf("artifact: 平台 %q 非法，段数过多", s)
	}
	return p, nil
}

// SaveImagesContext 拉取 refs 并写入 OCI layout。
func SaveImagesContext(ctx context.Context, refs []string, dir string, platform string) error {
	if dir == "" {
		return fmt.Errorf("artifact: OCI layout 目录为空")
	}
	plat, err := parsePlatform(platform)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("artifact: 创建目录 %s 失败: %w", filepath.Dir(dir), err)
	}
	// 目标已存在时增量写入，否则从空 index 开始（layout 会自行创建目录）。
	var lp layout.Path
	if _, err := os.Stat(filepath.Join(dir, OCILayoutFile)); err == nil {
		lp, err = layout.FromPath(dir)
		if err != nil {
			return fmt.Errorf("artifact: 打开已有 OCI layout %s 失败: %w", dir, err)
		}
	} else {
		lp, err = layout.Write(dir, empty.Index)
		if err != nil {
			return fmt.Errorf("artifact: 创建 OCI layout %s 失败: %w", dir, err)
		}
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if err := saveOne(ctx, lp, ref, plat); err != nil {
			return err
		}
	}
	return nil
}

// saveOne 拉取一个镜像并追加到 layout，同时记录它的引用。
func saveOne(ctx context.Context, lp layout.Path, ref string, plat *v1.Platform) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("artifact: 镜像引用 %q 非法: %w", ref, err)
	}
	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	}
	if plat != nil {
		opts = append(opts, remote.WithPlatform(*plat))
	}
	img, err := remote.Image(r, opts...)
	if err != nil {
		return fmt.Errorf("artifact: 拉取镜像 %s 失败: %w", ref, err)
	}
	descOpts := []layout.Option{layout.WithAnnotations(map[string]string{OCIRefNameAnnotation: ref})}
	if plat != nil {
		// 显式记录平台，避免依赖 remote 推断（多平台 tag 时 index 里需要它）。
		descOpts = append(descOpts, layout.WithPlatform(*plat))
	}
	if err := lp.AppendImage(img, descOpts...); err != nil {
		return fmt.Errorf("artifact: 写入镜像 %s 到 OCI layout 失败: %w", ref, err)
	}
	return nil
}

// LayoutEntry 是 LayoutSummary 的一条记录：一个镜像的引用、摘要、大小与平台。
type LayoutEntry struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  string `json:"platform,omitempty"`
}

// LayoutSummary 汇总 OCI layout 里的镜像：只取镜像 manifest，引用优先用
// org.opencontainers.image.ref.name 注解，缺失时退化为摘要；结果按引用排序。
func LayoutSummary(dir string) ([]LayoutEntry, error) {
	entries, err := LayoutEntries(dir)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]int, len(entries))
	out := make([]LayoutEntry, 0, len(entries))
	for _, e := range entries {
		if i, ok := seen[e.Reference]; ok {
			if out[i].Platform == "" {
				out[i].Platform = e.Platform
			}
			if out[i].Digest == "" {
				out[i].Digest = e.Digest
			}
			continue
		}
		seen[e.Reference] = len(out)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reference < out[j].Reference })
	return out, nil
}

// LayoutEntries 返回 index 里的全部镜像条目（同一引用可能对应多个平台，不去重）。
func LayoutEntries(dir string) ([]LayoutEntry, error) {
	data, err := os.ReadFile(filepath.Join(dir, OCIIndexFile))
	if err != nil {
		return nil, fmt.Errorf("artifact: 读取 %s 的 index 失败: %w", dir, err)
	}
	var index struct {
		Manifests []struct {
			MediaType   types.MediaType   `json:"mediaType"`
			Digest      string            `json:"digest"`
			Size        int64             `json:"size"`
			Platform    *v1.Platform      `json:"platform"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("artifact: 解析 %s 的 index 失败: %w", dir, err)
	}
	out := make([]LayoutEntry, 0, len(index.Manifests))
	for _, d := range index.Manifests {
		if d.MediaType != types.OCIManifestSchema1 && d.MediaType != types.DockerManifestSchema2 {
			continue
		}
		ref := d.Annotations[OCIRefNameAnnotation]
		if ref == "" {
			ref = d.Digest
		}
		if ref == "" {
			continue
		}
		out = append(out, LayoutEntry{
			Reference: ref,
			Digest:    d.Digest,
			Size:      d.Size,
			Platform:  platformString(d.Platform),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reference != out[j].Reference {
			return out[i].Reference < out[j].Reference
		}
		return out[i].Platform < out[j].Platform
	})
	return out, nil
}

// platformString 把 index 条目里的 platform 渲染成 "os/arch[/variant]"。
func platformString(p *v1.Platform) string {
	if p == nil || p.OS == "" || p.Architecture == "" {
		return ""
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}
