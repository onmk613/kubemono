package pack

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"kubemono/internal/artifact"
	"kubemono/internal/artifact/release"
	"kubemono/internal/util/fsutil"
)

// Plan 是 `bundle pack --dry-run` 的只读计划：不联网、不写盘，只回答
// “这次打包会用到哪些组件/镜像、哪些命中缓存、最终写到哪里”。
type Plan struct {
	Release       string          `json:"release"`
	OS            string          `json:"os"`
	Archs         []string        `json:"archs"`
	OutDir        string          `json:"outDir"`
	CacheRoot     string          `json:"cacheRoot"`
	IncludeImages bool            `json:"includeImages"`
	Offline       bool            `json:"offline"`
	Refresh       bool            `json:"refresh"`
	Components    []ComponentPlan `json:"components"`
	Images        []ImagePlan     `json:"images,omitempty"`
	Archives      []string        `json:"archives"`
}

// ComponentPlan 是一个组件在某个架构下的缓存状态。
type ComponentPlan struct {
	Arch     string `json:"arch"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Version  string `json:"version"`
	Source   string `json:"source"`
	Action   string `json:"action"` // cache | download | refresh
}

// ImagePlan 是一条镜像在当前 OCI layout 下的状态。
type ImagePlan struct {
	Arch      string `json:"arch"`
	Reference string `json:"reference"`
	Action    string `json:"action"` // present | pull
}

// PlanPack 计算一次打包会做什么，不产生任何副作用。
//
// 与 Run 共用 resolvePackOptions 的架构/互斥/目录边界校验，因此 --dry-run
// 能提前发现 --offline 缺物料、--out-dir 与缓存目录互相包含这类失败。
func PlanPack(opts Options) (*Plan, error) {
	opts.DryRun = true
	r, err := resolvePackOptions(opts)
	if err != nil {
		return nil, err
	}
	plan := &Plan{
		Release:       r.bom.Release,
		OS:            r.osName,
		Archs:         append([]string(nil), r.arches...),
		OutDir:        r.outDir,
		CacheRoot:     filepath.Join(r.cacheRoot, r.bom.Release),
		IncludeImages: r.IncludeImages,
		Offline:       r.Offline,
		Refresh:       r.Refresh,
	}
	refs := imageRefs(r.bom)
	for _, arch := range r.arches {
		platform := r.osName + "/" + arch
		for _, name := range r.bom.Names() {
			c := r.bom.Components[name]
			if c.Source == "" || len(c.Binaries) == 0 {
				continue
			}
			src, err := r.bom.SourceURL(name, arch)
			if err != nil {
				return nil, err
			}
			cached := artifact.ComponentCachePath(plan.CacheRoot, name, c.Version, arch, src)
			action := "download"
			switch {
			case r.Refresh:
				action = "refresh"
			case fsutil.FileExists(cached):
				action = "cache"
			case r.Offline:
				return nil, fmt.Errorf("%w：组件 %s 的缓存文件 %s 不存在，--offline 不会下载",
					release.ErrMaterialMissing, name, cached)
			}
			plan.Components = append(plan.Components, ComponentPlan{
				Arch: arch, Name: name, Category: c.Category, Version: c.Version,
				Source: src, Action: action,
			})
		}
		if r.IncludeImages {
			missing := missingImageRefs(r.imgDir, refs, platform)
			missingSet := make(map[string]bool, len(missing))
			for _, ref := range missing {
				missingSet[ref] = true
			}
			if (r.Offline || !r.FetchImages) && len(missing) > 0 {
				return nil, fmt.Errorf(
					"OCI layout %s 缺少 %d 个 %s 镜像且当前模式不会拉取（%s）：先联网 pack 或补齐 layout",
					r.imgDir, len(missing), platform, strings.Join(missing, ", "))
			}
			for _, img := range r.bom.Images() {
				action := "present"
				if missingSet[img.Ref] {
					action = "pull"
				}
				plan.Images = append(plan.Images, ImagePlan{
					Arch: arch, Reference: img.Ref, Action: action,
				})
			}
		}
		plan.Archives = append(plan.Archives,
			filepath.Join(r.outDir, artifact.ArchiveName(r.bom.Release, r.osName, arch, r.IncludeImages)))
	}
	sort.Slice(plan.Components, func(i, j int) bool {
		if plan.Components[i].Arch != plan.Components[j].Arch {
			return plan.Components[i].Arch < plan.Components[j].Arch
		}
		return plan.Components[i].Name < plan.Components[j].Name
	})
	sort.Slice(plan.Images, func(i, j int) bool {
		if plan.Images[i].Arch != plan.Images[j].Arch {
			return plan.Images[i].Arch < plan.Images[j].Arch
		}
		return plan.Images[i].Reference < plan.Images[j].Reference
	})
	return plan, nil
}
