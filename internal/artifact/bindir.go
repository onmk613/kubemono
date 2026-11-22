// 本文件是离线物料中二进制的唯一定位实现（原 internal/bindir 包）。
// runtime、kubelet 通过它按同一优先级查找：meta 记录 → bin/<category>/<name>。
// 只支持 canonical 布局；其它布局直接报错，避免把打包错误藏到安装阶段。
package artifact

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"kubemono/internal/util/fsutil"
)

// BinaryRef 是一个已定位的二进制。
type BinaryRef struct {
	// Path 是磁盘上的绝对路径。
	Path string
	// Category 是 bin/<category> 中的类别。
	Category string
	// SHA256 是 meta 记录的内容哈希；没有 meta 时为空。
	SHA256 string
}

// LoadMetaOptional 读取物料根目录下的 meta.json；不存在或损坏时返回 nil。
func LoadMetaOptional(root string) *Meta {
	p := filepath.Join(root, MetaName)
	if !fsutil.FileExists(p) {
		return nil
	}
	m, err := LoadMetaFile(p)
	if err != nil {
		return nil
	}
	return m
}

// LocateBinary 按统一优先级定位一个二进制。
func LocateBinary(root string, m *Meta, component, name, category string) (BinaryRef, error) {
	candidates := make([]string, 0, 2)
	ref := BinaryRef{Category: category}
	if e, ok := metaEntry(m, component, name); ok {
		ref.Category = categoryOf(e)
		ref.SHA256 = e.SHA256
		candidates = append(candidates, filepath.Join(root, filepath.FromSlash(e.Path)))
	}
	if ref.Category != "" {
		candidates = append(candidates, filepath.Join(root, BundlePath(ref.Category, name)))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			ref.Path = candidate
			return ref, nil
		}
	}
	return BinaryRef{}, fmt.Errorf("物料中找不到 %s（component=%s，category=%s）：期望 %s",
		name, component, category, filepath.Join(root, DirBin, category, name))
}

// metaEntry 在 meta 里查找一个二进制条目；优先精确 component，其次按文件名兜底。
func metaEntry(m *Meta, component, name string) (FileEntry, bool) {
	if m == nil {
		return FileEntry{}, false
	}
	var fallback FileEntry
	found := false
	for _, f := range m.Files {
		if !strings.HasPrefix(f.Path, DirBin+"/") || path.Base(f.Path) != name {
			continue
		}
		if f.Component == component {
			return f, true
		}
		if f.Component == "" && !found {
			fallback, found = f, true
		}
	}
	return fallback, found
}

// categoryOf 返回条目所属的 bin 类别。
func categoryOf(e FileEntry) string {
	if e.Category != "" {
		return e.Category
	}
	rest := strings.TrimPrefix(e.Path, DirBin+"/")
	parts := strings.Split(rest, "/")
	if len(parts) >= 2 {
		return parts[0]
	}
	return ""
}
