package bom

import (
	_ "embed"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	"sigs.k8s.io/yaml"

	"kubemono/internal/constants"
)

//go:embed bom.yaml
var bomYAML []byte

// 组件类别。顺序决定 artifact list 与打包的输出顺序。
const (
	CategoryRuntime = "runtime"
	CategoryCNI     = "cni"
	CategoryK8s     = "k8s"
	CategoryEtcd    = "etcd"
	CategoryNetwork = "network"
	CategoryAddon   = "addon"
	CategoryLB      = "lb"
	CategoryImage   = "image"
)

// CategoryNames 返回规范顺序的组件类别名。
func CategoryNames() []string {
	out := make([]string, len(categoryOrder))
	copy(out, categoryOrder)
	return out
}

// CategoryValid 报告 name 是否是已知的组件类别。
func CategoryValid(name string) bool {
	for _, c := range categoryOrder {
		if c == name {
			return true
		}
	}
	return false
}

// categoryOrder 是规范的输出顺序。
var categoryOrder = []string{
	CategoryK8s,
	CategoryEtcd,
	CategoryRuntime,
	CategoryCNI,
	CategoryNetwork,
	CategoryAddon,
	CategoryLB,
	CategoryImage,
}

// Component 是 BOM 中的一条条目。
type Component struct {
	Version  string `json:"version"`
	Category string `json:"category"`
	// Source 是下载 URL 模板。占位符：{version}、{versiontrim}、{arch}。
	// 空值表示该组件不由网络下载。
	Source string `json:"source,omitempty"`
	// Binaries 列出该组件提供的可执行文件。
	Binaries []string `json:"binaries,omitempty"`
	// Image 是该组件的完整镜像引用模板，例如
	// "registry.k8s.io/kube-proxy:{version}"。没有镜像的组件为空。
	// 没有全局仓库前缀：每个镜像都在这里写全，避免前缀叠加二义性。
	Image string `json:"image,omitempty"`
	// Notes 记录 kubemono 特有的处理。
	Notes string `json:"notes,omitempty"`
}

// BOM 是解析后的 Bill of Materials。
type BOM struct {
	Release    string               `json:"release"`
	Kubernetes string               `json:"kubernetes"`
	Arch       []string             `json:"arch,omitempty"`
	Components map[string]Component `json:"components"`
	// Overrides 记录运行时固定的组件版本。它不会被序列化：生效版本已写入
	// Components，打包之后的 meta.json 才是权威。
	Overrides map[string]string `json:"-"`
}

// Load 返回内嵌的 BOM。解析结果（含错误）在第一次调用时确定并缓存，
// sync.OnceValues 同时保证并发调用安全。
var Load = sync.OnceValues(loadEmbedded)

func loadEmbedded() (*BOM, error) {
	// 节点二进制只加载 BOM（版本/来源）；物料哈希在 meta.json 里，
	// 不参与启动路径。
	return Parse(bomYAML)
}

// MustLoad 返回内嵌的 BOM，失败则 panic。只用于启动路径。
func MustLoad() *BOM {
	b, err := Load()
	if err != nil {
		panic(err)
	}
	return b
}

// Parse 解码并校验一份 BOM 文档。
func Parse(data []byte) (*BOM, error) {
	b := &BOM{}
	if err := yaml.UnmarshalStrict(data, b); err != nil {
		return nil, fmt.Errorf("parse BOM: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// Validate 检查 BOM 的各项不变量。
func (b *BOM) Validate() error {
	if b.Release == "" {
		return fmt.Errorf("bom: release is required")
	}
	if _, err := ParseRelease(b.Release); err != nil {
		return fmt.Errorf("bom: %w", err)
	}
	if b.Kubernetes == "" {
		return fmt.Errorf("bom: kubernetes version is required")
	}
	// kubemono 只支持 Linux amd64/arm64；BOM 是唯一允许声明目标架构的地方。
	if len(b.Arch) == 0 {
		return fmt.Errorf("bom: arch must not be empty")
	}
	seenArch := map[string]bool{}
	for _, arch := range b.Arch {
		if arch != "amd64" && arch != "arm64" {
			return fmt.Errorf("bom: unsupported arch %q (only amd64 and arm64)", arch)
		}
		if seenArch[arch] {
			return fmt.Errorf("bom: duplicate arch %q", arch)
		}
		seenArch[arch] = true
	}
	if len(b.Components) == 0 {
		return fmt.Errorf("bom: components must not be empty")
	}
	if _, ok := b.Components["kubemono"]; ok {
		return fmt.Errorf("bom: component \"kubemono\" must not be in the BOM; kubemono is shipped as a standalone binary")
	}
	for name, c := range b.Components {
		if c.Version == "" {
			return fmt.Errorf("bom: component %q has no version", name)
		}
		if c.Category == "" {
			return fmt.Errorf("bom: component %q has no category", name)
		}
		if !validCategory(c.Category) {
			return fmt.Errorf("bom: component %q has unknown category %q", name, c.Category)
		}
		if c.Image != "" {
			if err := validateImageRef(b.expand(c.Image, c.Version)); err != nil {
				return fmt.Errorf("bom: component %q image %q: %w", name, c.Image, err)
			}
		}
	}
	// k8s 组件版本必须与 kubernetes 一致，否则控制面会出现版本偏差。
	// 整组固定 patch 时由 WithVersionOverrides 同步改写 kubernetes 与 release。
	// 清单必须来自 constants：硬编码组件列表会漏掉 kubectl，
	// 导致 kubectl 版本不一致时 Validate 依然通过。
	for _, name := range constants.KubernetesComponentNames() {
		if c, ok := b.Components[name]; ok && c.Version != b.Kubernetes {
			return fmt.Errorf("bom: component %q version %s does not match kubernetes %s", name, c.Version, b.Kubernetes)
		}
	}
	return nil
}

func validCategory(c string) bool {
	return slices.Contains(categoryOrder, c)
}

// Component 按名字返回组件。
func (b *BOM) Component(name string) (Component, bool) {
	c, ok := b.Components[name]
	return c, ok
}

// ComponentVersion 返回组件版本，未知时返回 ""。
func (b *BOM) ComponentVersion(name string) string { return b.Components[name].Version }

// Names 按规范顺序返回全部组件名。
func (b *BOM) Names() []string {
	out := make([]string, 0, len(b.Components))
	for name := range b.Components {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		ci, cj := b.Components[out[i]], b.Components[out[j]]
		if categoryRank(ci.Category) != categoryRank(cj.Category) {
			return categoryRank(ci.Category) < categoryRank(cj.Category)
		}
		return out[i] < out[j]
	})
	return out
}

func categoryRank(c string) int {
	for i, k := range categoryOrder {
		if k == c {
			return i
		}
	}
	return len(categoryOrder)
}

// SupportsArch 报告该 BOM 是否发布了某个架构。
func (b *BOM) SupportsArch(arch string) bool {
	return slices.Contains(b.Arch, arch)
}

// Image 是一条完整的镜像引用。
type Image struct {
	Component string
	Ref       string
}

// Reference 返回完整镜像引用，例如 registry.k8s.io/kube-proxy:v1.37.1。
func (i Image) Reference() string { return i.Ref }

// String 实现 fmt.Stringer。
func (i Image) String() string { return i.Reference() }

// Images 返回 BOM 声明的全部镜像，已按组件顺序去重。
func (b *BOM) Images() []Image {
	var out []Image
	seen := map[string]bool{}
	for _, name := range b.Names() {
		c := b.Components[name]
		if c.Image == "" {
			continue
		}
		ref := b.expand(c.Image, c.Version)
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, Image{Component: name, Ref: ref})
	}
	return out
}

// Image 返回组件的完整镜像引用。
func (b *BOM) Image(component string) (Image, error) {
	c, ok := b.Components[component]
	if !ok {
		return Image{}, fmt.Errorf("bom: unknown component %q", component)
	}
	if c.Image == "" {
		return Image{}, fmt.Errorf("bom: component %q has no image", component)
	}
	return Image{Component: component, Ref: b.expand(c.Image, c.Version)}, nil
}

// WithImageOverrides 返回应用了按组件镜像覆盖的 BOM 副本。
// 只允许覆盖 BOM 中原本有镜像的组件；覆盖值必须是写全的镜像引用。
func (b *BOM) WithImageOverrides(overrides map[string]string) (*BOM, error) {
	if b == nil {
		return nil, fmt.Errorf("bom: BOM is nil")
	}
	if len(overrides) == 0 {
		return b, nil
	}
	out := b.Clone()
	for component, ref := range overrides {
		ref = strings.TrimSpace(ref)
		c, ok := out.Components[component]
		if !ok {
			return nil, fmt.Errorf("component %q is not in BOM release %s", component, b.Release)
		}
		if c.Image == "" {
			return nil, fmt.Errorf("component %q has no image to override", component)
		}
		if ref == "" {
			return nil, fmt.Errorf("component %q image must not be empty", component)
		}
		c.Image = ref
		out.Components[component] = c
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// validateImageRef 要求镜像地址写全（含 registry host），不接受依赖隐式 Docker Hub。
func validateImageRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("image reference must not be empty")
	}
	if strings.ContainsAny(ref, " \t\n\r") {
		return fmt.Errorf("image reference must not contain whitespace")
	}
	first, _, ok := strings.Cut(ref, "/")
	if !ok || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return fmt.Errorf("must be a full image reference including registry host")
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	if !strings.ContainsAny(last, ":@") {
		return fmt.Errorf("must include an explicit tag or digest")
	}
	if _, err := name.ParseReference(ref); err != nil {
		return fmt.Errorf("invalid image reference: %w", err)
	}
	return nil
}

// expand 替换 BOM 占位符。
func (b *BOM) expand(tmpl, version string) string {
	r := strings.NewReplacer(
		"{version}", version,
		"{versiontrim}", strings.TrimPrefix(version, "v"),
		"{kubernetes}", b.Kubernetes,
		"{release}", b.Release,
	)
	return r.Replace(tmpl)
}

// SourceURL 返回组件在某个架构下的下载 URL。
// 架构占位符 {arch} 也会被替换。
func (b *BOM) SourceURL(component, arch string) (string, error) {
	c, ok := b.Components[component]
	if !ok {
		return "", fmt.Errorf("bom: unknown component %q", component)
	}
	if c.Source == "" {
		return "", fmt.Errorf("bom: component %q has no download source", component)
	}
	if !b.SupportsArch(arch) {
		return "", fmt.Errorf("bom: architecture %q is not supported by release %s", arch, b.Release)
	}
	return strings.ReplaceAll(b.expand(c.Source, c.Version), "{arch}", arch), nil
}
