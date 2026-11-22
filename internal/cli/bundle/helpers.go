package bundle

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"kubemono/internal/bom"
	"kubemono/internal/cli/options"
	"kubemono/internal/pack"
)

// loadPlan 解析本次命令使用的集群配置与 BOM。
// 有 -f 或本机 /usr/local/kubemono/cluster.yaml 时按集群覆盖版本；否则退回内嵌 BOM。
func loadPlan(opts *options.GlobalOptions) (*bom.BOM, error) {
	if _, exists := opts.ClusterConfigPath(); exists {
		cluster, err := opts.LoadCluster()
		if err != nil {
			return nil, err
		}
		return opts.EffectiveBOM(cluster)
	}
	return bom.MustLoad(), nil
}

// resolveArch 解析 --arch；为空时使用与 pack 相同的默认架构（linux/amd64）。
// list 一次只列一个架构；pack 可以一次出多个架构。
func resolveArch(b *bom.BOM, flag string) (string, error) {
	spec := strings.TrimSpace(flag)
	if spec == "" {
		spec = pack.DefaultArch
	}
	osName, arches, err := pack.ParseArchSpec(spec)
	if err != nil {
		return "", err
	}
	if osName != pack.DefaultOS {
		return "", fmt.Errorf("目标 OS %q 不受支持（只支持 %s）", osName, pack.DefaultOS)
	}
	if len(arches) != 1 {
		return "", fmt.Errorf("一次只支持一个架构（%s）；请分别用 --arch 执行", strings.Join(arches, ", "))
	}
	arch := arches[0]
	if b == nil || !b.SupportsArch(arch) {
		supported := ""
		if b != nil {
			supported = strings.Join(b.Arch, ", ")
		}
		return "", fmt.Errorf("架构 %q 不在 BOM 的 arch 列表里（%s）", arch, supported)
	}
	return arch, nil
}

// releaseDir 返回某个 release 的物料目录。
func releaseDir(opts *options.GlobalOptions, release string) string {
	return opts.Paths().ArtifactDir(release)
}

// resolveRelease 解析要检查/查看的 release。
// 优先 --release，其次 cluster.yaml 的生效 BOM，最后物料库里唯一的目录。
func resolveRelease(opts *options.GlobalOptions, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	b, err := loadPlan(opts)
	if err != nil {
		return "", err
	}
	if _, exists := opts.ClusterConfigPath(); exists {
		return b.Release, nil
	}
	entries, err := os.ReadDir(opts.Paths().Artifacts())
	if err == nil {
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		switch len(names) {
		case 1:
			return names[0], nil
		case 0:
			// 落到内嵌 BOM：verify 会明确报告物料目录缺失。
		default:
			return "", fmt.Errorf("物料库里有多个 release（%s），请用 --release 或 -f 指定", strings.Join(names, ", "))
		}
	}
	return b.Release, nil
}
