package bom

import (
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	"kubemono/internal/constants"
)

// ComponentVersion 是 BOM 组件使用的宽松语义版本。
// 组件的版本格式来自上游，可能是 v1.14.6、v3.7.0、2.8 或 3.10.2。这里只比较
// 数字部分；预发布后缀不参与最低版本判断。
type ComponentVersion struct {
	Major int
	Minor int
	Patch int
	Raw   string
}

// ParseComponentVersion 解析组件版本，例如 "v1.14.6"、"2.8" 或 "3.10.2"。
func ParseComponentVersion(s string) (ComponentVersion, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return ComponentVersion{}, fmt.Errorf("empty component version")
	}
	v := ComponentVersion{Raw: raw}
	base := strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V")
	if i := strings.IndexAny(base, "-+"); i >= 0 {
		base = base[:i]
	}
	parts := strings.Split(base, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return ComponentVersion{}, fmt.Errorf("invalid component version %q: expected major[.minor[.patch]]", raw)
	}
	nums := make([]int, 3)
	for i, part := range parts {
		if part == "" {
			return ComponentVersion{}, fmt.Errorf("invalid component version %q: empty numeric segment", raw)
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return ComponentVersion{}, fmt.Errorf("invalid component version %q: %q is not a non-negative integer", raw, part)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// Clone 返回 BOM 的深拷贝。
func (b *BOM) Clone() *BOM {
	if b == nil {
		return nil
	}
	out := *b
	out.Arch = append([]string(nil), b.Arch...)
	out.Components = make(map[string]Component, len(b.Components))
	for name, c := range b.Components {
		out.Components[name] = cloneComponent(c)
	}
	if len(b.Overrides) > 0 {
		out.Overrides = make(map[string]string, len(b.Overrides))
		maps.Copy(out.Overrides, b.Overrides)
	}
	return &out
}

func cloneComponent(c Component) Component {
	out := c
	out.Binaries = append([]string(nil), c.Binaries...)
	return out
}

// WithVersionOverrides 返回应用了给定组件版本后的 BOM 副本，并为每个非默认
// 固定版本附带人类可读的警告。
// 规则：
// - 组件必须存在于 BOM 中；
// - 任意版本都允许，只要求非空；
// - Kubernetes 组件必须**整组、同版本**地固定：固定为 v1.38.0 时，
// 全部 k8s 组件一起改成 v1.38.0，同时 BOM 的 kubernetes 与 release
// 改写为 v1.38.0 / v1.38.0-km.<n>；任意版本都允许，只告警；
// - 非默认版本没有预记录的上游哈希；包内文件哈希仍由 meta.json 记录。
func (b *BOM) WithVersionOverrides(overrides map[string]string) (*BOM, []string, error) {
	if b == nil {
		return nil, nil, fmt.Errorf("bom: BOM is nil")
	}
	if len(overrides) == 0 {
		return b, nil, nil
	}
	out := b.Clone()
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)

	var warnings []string
	k8sPins := map[string]string{}
	for _, name := range names {
		version := strings.TrimSpace(overrides[name])
		if version == "" {
			return nil, nil, fmt.Errorf("component %q: version must not be empty", name)
		}
		c, ok := out.Components[name]
		if !ok {
			return nil, nil, fmt.Errorf("component %q is not in BOM release %s", name, b.Release)
		}
		// Kubernetes 组件整组处理：先收集，循环结束后统一校验与应用，
		// 避免"只改了一部分"就落盘出半套版本。
		if c.Category == CategoryK8s || constants.IsKubernetesComponent(name) {
			k8sPins[name] = version
			continue
		}
		if version == c.Version {
			continue
		}
		c.Version = version
		out.Components[name] = c
		if out.Overrides == nil {
			out.Overrides = map[string]string{}
		}
		out.Overrides[name] = version
		warnings = append(warnings, fmt.Sprintf(
			"组件 %s 固定为非默认版本 %s（默认 %s）；kubemono 只保证默认版本，兼容性由操作者负责",
			name, version, b.Components[name].Version))
		if c.Category == CategoryEtcd || c.Category == CategoryRuntime || c.Category == CategoryCNI {
			warnings = append(warnings, fmt.Sprintf(
				"组件 %s 是节点级组件，非默认版本建议用对应物料重建节点", name))
		}
	}
	if len(k8sPins) > 0 {
		w, err := out.applyKubernetesGroup(k8sPins)
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, w...)
	}
	if err := out.Validate(); err != nil {
		return nil, nil, err
	}
	return out, warnings, nil
}

// applyKubernetesGroup 把整组 k8s 组件固定到同一个 patch，并同步 BOM 的
// kubernetes 与 release。
// 为什么必须整组：控制面三件套（apiserver/kcm/scheduler）上游要求同版本，
// kubelet 不得新于 apiserver；即使不限制版本范围，也不允许单组件漂移。
// 为什么必须改写 kubernetes/release：Validate 要求 k8s 组件版本与 BOM 的
// kubernetes 一致，而 release 是清单与 kubemono-cluster 记录的键。若只改组件
// 而不改 release，两个内容不同的包会共用同一个 release，运维与审计都无法
// 区分它们实际包含哪个版本。
func (b *BOM) applyKubernetesGroup(pins map[string]string) ([]string, error) {
	group := b.kubernetesComponentNames()
	if len(group) == 0 {
		return nil, fmt.Errorf("bom: BOM 里没有 Kubernetes 组件，无法固定版本")
	}
	missing := make([]string, 0, len(group))
	for _, name := range group {
		if _, ok := pins[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"k8s 组件必须整组固定（%s）：缺少 %s；只固定其中一部分会让控制面出现版本偏差",
			strings.Join(group, ", "), strings.Join(missing, ", "))
	}

	version := pins[group[0]]
	var conflicts []string
	for _, name := range group {
		if pins[name] != version {
			conflicts = append(conflicts, fmt.Sprintf("%s=%s", name, pins[name]))
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, fmt.Errorf("k8s 组件必须整组同版本：%s=%s，与之冲突的是 %s",
			group[0], version, strings.Join(conflicts, ", "))
	}

	if _, err := ParseComponentVersion(version); err != nil {
		return nil, fmt.Errorf("component %q: %w", group[0], err)
	}
	if version == b.Kubernetes {
		// 与 BOM 基线一致：视为没有覆盖，不改写 release。
		return nil, nil
	}

	baseline := b.Kubernetes
	release := rebuildRelease(b.Release, version)
	for _, name := range group {
		c := b.Components[name]
		c.Version = version
		b.Components[name] = c
		if b.Overrides == nil {
			b.Overrides = map[string]string{}
		}
		b.Overrides[name] = version
	}
	b.Kubernetes = version
	b.Release = release
	return []string{fmt.Sprintf(
		"Kubernetes 组件整组固定为 %s（BOM 默认 %s）；release 记为 %s，物料、meta 与节点记录都会使用该版本，kubemono 只保证默认版本，兼容性由操作者负责",
		version, baseline, release)}, nil
}

// kubernetesComponentNames 返回 BOM 里全部 Kubernetes 组件（按名字排序）。
func (b *BOM) kubernetesComponentNames() []string {
	out := make([]string, 0, 6)
	for name, c := range b.Components {
		if c.Category == CategoryK8s || constants.IsKubernetesComponent(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// rebuildRelease 把 release 的 Kubernetes 部分替换为 kubernetes，保留 km.N。
func rebuildRelease(current, kubernetes string) string {
	r, err := ParseRelease(current)
	if err != nil {
		return kubernetes
	}
	return Release{Kubernetes: kubernetes, Patch: r.Patch}.String()
}

// ApplyOverrides 依次应用版本覆盖与按组件镜像覆盖，返回新的 BOM。
// warn 接收版本覆盖产生的告警（调用方决定输出到 reporter 还是 stderr）；
// 版本覆盖失败时不会应用镜像覆盖，避免半套覆盖落盘。
func ApplyOverrides(b *BOM, versions, images map[string]string, warn func(string)) (*BOM, error) {
	if b == nil {
		return nil, fmt.Errorf("bom: BOM is nil")
	}
	if len(versions) > 0 {
		withVersions, warnings, err := b.WithVersionOverrides(versions)
		if err != nil {
			return nil, err
		}
		for _, w := range warnings {
			if warn != nil {
				warn(w)
			}
		}
		b = withVersions
	}
	if len(images) > 0 {
		withImages, err := b.WithImageOverrides(images)
		if err != nil {
			return nil, err
		}
		b = withImages
	}
	return b, nil
}
