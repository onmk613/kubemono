package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"kubemono/internal/artifact"
)

// verifyMaterials 校验已导入物料的 meta 与本次生效 BOM 一致。
//
// release 只是版本边界，不是内容身份：同一个 release 允许被重新打包（文档明确
// 允许），节点却仍按自己的 cluster.yaml + 内嵌 BOM 渲染版本/镜像。如果不校验，
// 会出现“物料是 A、渲染引用是 B”的静默漂移（气隙环境下通常到 Pod 拉镜像
// 或组件版本不符才暴露）。这里在 install/join/repair 开始前 fail-closed。
func verifyMaterials(opts *Options) error {
	if opts == nil || opts.BOM == nil || opts.Paths.ArtifactDir(opts.Release) == "" {
		return nil
	}
	dir := opts.Paths.ArtifactDir(opts.Release)
	metaPath := filepath.Join(dir, artifact.MetaName)
	meta, err := artifact.LoadMetaFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 手工物料/无 meta：由后续 bindir 的缺失错误给出指引
		}
		return fmt.Errorf("读取物料清单 %s 失败: %w", metaPath, err)
	}
	if meta.Release != "" && opts.Release != "" && meta.Release != opts.Release {
		return fmt.Errorf("物料 release 是 %s，本机期望 %s：请重新执行 bundle load 或修正配置", meta.Release, opts.Release)
	}
	if meta.Kubernetes != "" && opts.BOM.Kubernetes != "" && meta.Kubernetes != opts.BOM.Kubernetes {
		return fmt.Errorf("物料 Kubernetes 版本是 %s，当前生效 BOM 是 %s：pack 与节点配置不一致，"+
			"请按节点 cluster.yaml 重新 pack/load（或修正 kubernetesVersion）", meta.Kubernetes, opts.BOM.Kubernetes)
	}
	if len(meta.Components) > 0 {
		versions := make(map[string]string, len(meta.Components))
		for _, c := range meta.Components {
			versions[c.Name] = c.Version
		}
		for _, name := range opts.BOM.Names() {
			want := opts.BOM.ComponentVersion(name)
			got, ok := versions[name]
			if !ok || want == "" {
				continue
			}
			if got != want {
				return fmt.Errorf("物料组件 %s 版本是 %s，当前生效 BOM 是 %s：pack 与节点配置不一致，"+
					"请按节点 cluster.yaml 重新 pack/load", name, got, want)
			}
		}
	}
	if len(meta.Images) > 0 {
		have := make(map[string]bool, len(meta.Images))
		for _, img := range meta.Images {
			have[img.Reference] = true
		}
		for _, img := range opts.BOM.Images() {
			if !have[img.Reference()] {
				return fmt.Errorf("物料缺少当前生效 BOM 的镜像 %s：请用 --images 重新 pack/load", img.Reference())
			}
		}
	}
	return nil
}
