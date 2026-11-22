// Package artifact 实现 kubemono 离线物料的格式、落盘与校验。
//
// 物料是发布方交给节点的“已组装目录”：
//
//	<release>/
//	  meta.json                 物料清单（本文件的唯一权威）
//	  bin/<category>/<name>     组件二进制
//	  images/                   可选，OCI layout
//
// 离线归档是一个以 meta.json 开头的 zstd tar（*.tar.zst）。节点侧只做
// “拉取/解包/落盘/齐备性校验”，不做组件下载、镜像组装或发布侧 checksums。
package artifact

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"kubemono/internal/bom"
)

// FormatVersion 是 meta.json 的 schema 版本。
// 只接受当前版本：物料格式不向前兼容，旧包必须重新打包。
const FormatVersion = 2

// 物料根目录下的格式自带条目。
const (
	// MetaName 是物料清单，也是归档的第一个条目。
	MetaName = "meta.json"
	// CompleteName 记录一次成功的 load（归档摘要与时间），用于幂等。
	CompleteName = ".complete"
)

// 物料目录布局。
const (
	// DirBin 存放组件二进制：bin/<category>/<name>。
	DirBin = "bin"
	// DirImages 存放去重后的 OCI 镜像布局。
	DirImages = "images"
)

// maxMetaSize 限制 meta.json 的读取大小。
const maxMetaSize = 64 << 20

// BundlePath 返回二进制在物料内的规范路径：bin/<category>/<name>。
func BundlePath(category, name string) string { return path.Join(DirBin, category, name) }

// Meta 是物料清单（meta.json）。
// Files 列出物料内所有普通文件；SHA256 为空表示该文件不记录哈希（仍校验大小）。
type Meta struct {
	FormatVersion int              `json:"formatVersion"`
	Release       string           `json:"release"`
	Kubernetes    string           `json:"kubernetes"`
	Arch          string           `json:"arch"`
	OS            string           `json:"os"`
	Created       time.Time        `json:"created"`
	Builder       string           `json:"builder,omitempty"`
	Components    []ComponentEntry `json:"components,omitempty"`
	Images        []ImageEntry     `json:"images,omitempty"`
	Files         []FileEntry      `json:"files,omitempty"`
}

// FileEntry 描述物料内的一个普通文件。
type FileEntry struct {
	// Path 是物料内相对路径，用斜杠分隔。
	Path string `json:"path"`
	// SHA256 是内容摘要，形式为 "sha256:<hex>"；为空表示未记录。
	SHA256 string `json:"sha256,omitempty"`
	// Size 是文件大小（字节）。
	Size int64 `json:"size"`
	// Mode 是八进制权限字符串，例如 "0755"。
	Mode string `json:"mode"`
	// Component 是该文件所属的 BOM 组件（如果有）。
	Component string `json:"component,omitempty"`
	// Category 是组件的 BOM 分类（如果有）。
	Category string `json:"category,omitempty"`
}

// ImageEntry 描述物料内的一个 OCI 镜像。
type ImageEntry struct {
	Component string `json:"component,omitempty"`
	Reference string `json:"reference"`
	Digest    string `json:"digest,omitempty"`
	Platform  string `json:"platform,omitempty"`
}

// ComponentEntry 记录物料内的一个组件版本。
type ComponentEntry struct {
	Name     string `json:"name"`
	Category string `json:"category,omitempty"`
	Version  string `json:"version"`
}

// FileMode 解析文件条目的八进制模式。
func (f FileEntry) FileMode() (fs.FileMode, error) {
	if f.Mode == "" {
		return 0o644, nil
	}
	var mode uint32
	if _, err := fmt.Sscanf(f.Mode, "%o", &mode); err != nil {
		return 0, fmt.Errorf("artifact: 文件 %s 的模式 %q 不是八进制: %w", f.Path, f.Mode, err)
	}
	return fs.FileMode(mode).Perm(), nil
}

// Normalize 就地规范化清单：按各自的自然键去重并排序，
// 保证同样的内容得到同样的 meta.json 字节。
func (m *Meta) Normalize() {
	if m == nil {
		return
	}
	m.Files = normalizeByKey(m.Files, func(f FileEntry) string { return f.Path })
	m.Images = normalizeImages(m.Images)
	m.Components = normalizeByKey(m.Components, func(c ComponentEntry) string { return c.Name })
}

func normalizeByKey[T any](in []T, key func(T) string) []T {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]T, 0, len(in))
	for _, item := range in {
		k := key(item)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

func normalizeImages(in []ImageEntry) []ImageEntry {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]int, len(in))
	out := make([]ImageEntry, 0, len(in))
	for _, img := range in {
		if i, ok := seen[img.Reference]; ok {
			if out[i].Digest == "" {
				out[i].Digest = img.Digest
			}
			if out[i].Platform == "" {
				out[i].Platform = img.Platform
			}
			if out[i].Component == "" {
				out[i].Component = img.Component
			}
			continue
		}
		seen[img.Reference] = len(out)
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reference < out[j].Reference })
	return out
}

// Validate 校验 meta.json 的不变量。
func (m *Meta) Validate() error {
	if m == nil {
		return fmt.Errorf("artifact: meta 为空")
	}
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("artifact: 不支持的物料格式版本 %d，本版本只支持 %d（旧包必须重新打包）", m.FormatVersion, FormatVersion)
	}
	if m.Release == "" {
		return fmt.Errorf("artifact: meta 缺少 release")
	}
	if _, err := bom.ParseRelease(m.Release); err != nil {
		return fmt.Errorf("artifact: meta release 非法: %w", err)
	}
	if m.Kubernetes == "" {
		return fmt.Errorf("artifact: meta 缺少 kubernetes")
	}
	if m.OS == "" || m.Arch == "" {
		return fmt.Errorf("artifact: meta 缺少 os/arch")
	}
	seen := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if f.Path == "" {
			return fmt.Errorf("artifact: meta 中存在空路径条目")
		}
		if err := ValidateBundlePath(f.Path); err != nil {
			return fmt.Errorf("artifact: meta 条目 %q: %w", f.Path, err)
		}
		if IsReservedPath(f.Path) {
			return fmt.Errorf("artifact: meta 条目 %q 是格式自带文件，不能出现在 files 里", f.Path)
		}
		if f.Size < 0 {
			return fmt.Errorf("artifact: 文件 %s 的大小为负数", f.Path)
		}
		if f.SHA256 != "" {
			if _, err := NormalizeDigest(f.SHA256); err != nil {
				return fmt.Errorf("artifact: 文件 %s 的 sha256 %q 非法: %w", f.Path, f.SHA256, err)
			}
		}
		if f.Mode != "" {
			var mode uint32
			if _, err := fmt.Sscanf(f.Mode, "%o", &mode); err != nil {
				return fmt.Errorf("artifact: 文件 %s 的模式 %q 不是八进制: %w", f.Path, f.Mode, err)
			}
		}
		if seen[f.Path] {
			return fmt.Errorf("artifact: meta 中存在重复路径 %q", f.Path)
		}
		seen[f.Path] = true
	}
	for _, img := range m.Images {
		if img.Reference == "" {
			return fmt.Errorf("artifact: 存在镜像引用为空的条目")
		}
		if img.Digest != "" {
			if _, err := NormalizeDigest(img.Digest); err != nil {
				return fmt.Errorf("artifact: 镜像 %s 的摘要 %q 非法: %w", img.Reference, img.Digest, err)
			}
		}
	}
	for _, c := range m.Components {
		if c.Name == "" {
			return fmt.Errorf("artifact: 存在没有名字的组件条目")
		}
	}
	return nil
}

// Find 返回某个路径对应的文件条目。
func (m *Meta) Find(p string) (FileEntry, bool) {
	if m == nil {
		return FileEntry{}, false
	}
	for _, f := range m.Files {
		if f.Path == p {
			return f, true
		}
	}
	return FileEntry{}, false
}

// CheckPlatform 校验物料声明的目标 OS/架构是否与当前节点一致。
func (m *Meta) CheckPlatform(goos, goarch string) error {
	if m == nil {
		return fmt.Errorf("artifact: meta 为空")
	}
	if m.OS == "" || m.Arch == "" {
		return fmt.Errorf("artifact: 物料 meta 缺少 os/arch")
	}
	if m.OS != goos {
		return fmt.Errorf("artifact: 物料目标 OS 是 %s，当前节点是 %s", m.OS, goos)
	}
	if m.Arch != goarch {
		return fmt.Errorf("artifact: 物料目标架构是 %s，当前节点是 %s；请使用对应架构的物料", m.Arch, goarch)
	}
	return nil
}

// TotalSize 返回物料内所有文件的大小之和。
func (m *Meta) TotalSize() int64 {
	if m == nil {
		return 0
	}
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// IsReservedPath 报告某个路径是否由物料格式自身占用。
func IsReservedPath(p string) bool {
	return p == MetaName || p == CompleteName
}

// LoadMeta 从物料目录读取 meta.json。
func LoadMeta(dir string) (*Meta, error) {
	return LoadMetaFile(path.Join(dir, MetaName))
}

// LoadMetaFile 读取并校验一个 meta.json 文件。
func LoadMetaFile(p string) (*Meta, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("artifact: 读取 %s 失败: %w", p, err)
	}
	m, err := ParseMeta(data)
	if err != nil {
		return nil, fmt.Errorf("artifact: 解析 %s 失败: %w", p, err)
	}
	return m, nil
}

// ParseMeta 解析并校验 meta.json 字节。
func ParseMeta(data []byte) (*Meta, error) {
	if len(data) > maxMetaSize {
		return nil, fmt.Errorf("artifact: meta.json 超过 %d 字节上限", maxMetaSize)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// MarshalMeta 确定性地校验并序列化 meta.json。
func MarshalMeta(m *Meta) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("artifact: meta 为空")
	}
	clone := *m
	clone.Normalize()
	if clone.FormatVersion == 0 {
		clone.FormatVersion = FormatVersion
	}
	if !clone.Created.IsZero() {
		clone.Created = clone.Created.UTC()
	}
	if err := clone.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(&clone, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("artifact: 序列化 meta 失败: %w", err)
	}
	return append(data, '\n'), nil
}

// NormalizeDigest 把 sha256 规范化为 "sha256:<hex>"。
// 接受带前缀或不带前缀的 64 位十六进制；其他算法一律拒绝。
func NormalizeDigest(sum string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(sum))
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) != 64 {
		return "", fmt.Errorf("摘要 %q 不是 sha256（需要 64 位十六进制）", sum)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("摘要 %q 不是合法十六进制: %w", sum, err)
	}
	return "sha256:" + s, nil
}
