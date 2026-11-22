package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// Snapshot 是本节点上次渲染的结果（/usr/local/kubemono/desired）。
// 修复流程以这份数据为“期望状态”：即使集群不可达，
// 也能据此发现并修复文件漂移。
type Snapshot struct {
	Node      string               `json:"node"`
	ClusterID string               `json:"clusterID,omitempty"`
	Release   string               `json:"release,omitempty"`
	SpecHash  string               `json:"specHash"`
	Files     map[string]FileState `json:"files"`
}

// FileState 是单个期望文件的持久化元数据。
type FileState struct {
	SHA256    string `json:"sha256"`
	Mode      uint32 `json:"mode"`
	Component string `json:"component"`
	Kind      Kind   `json:"kind"`
	OnChange  Action `json:"onChange"`
	Secret    bool   `json:"secret,omitempty"`
}

// Store 读写期望状态目录树。
type Store struct {
	Paths constants.Paths
}

// NewStore 返回以给定路径为根的 Store。
func NewStore(paths constants.Paths) *Store { return &Store{Paths: paths} }

// Dir 返回期望状态目录。
func (s *Store) Dir() string { return s.Paths.Desired() }

// ChecksumsPath 返回校验和文件路径。
func (s *Store) ChecksumsPath() string { return s.Paths.DesiredChecksums() }

// Write 持久化 spec：每个文件镜像到期望状态目录下（因此无需重新渲染即可恢复），
// 最后原子写入 checksums.json。
func (s *Store) Write(spec *Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	root := s.Dir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create desired directory: %w", err)
	}
	snap := Snapshot{
		Node:      spec.Node,
		ClusterID: spec.ClusterID,
		Release:   spec.Release,
		SpecHash:  spec.Hash(),
		Files:     make(map[string]FileState, len(spec.Files)),
	}
	keep := make(map[string]bool, len(spec.Files))
	for _, f := range spec.Files {
		rel := f.RelPath(s.Paths.Root)
		// Secret 文件不镜像到 desired/files：它只是排障快照，不应该成为
		// registry Basic 凭据等敏感内容的第二份持久副本。校验和/模式仍记录，
		// Restore 会跳过这些条目。
		if !f.Secret {
			dst := filepath.Join(root, "files", rel)
			if _, err := fsutil.WriteFileIfChanged(dst, f.Content, os.FileMode(f.Mode)); err != nil {
				return fmt.Errorf("write desired file %s: %w", dst, err)
			}
			keep[filepath.ToSlash(rel)] = true
		}
		snap.Files[f.Path] = FileState{
			SHA256:    f.SHA256(),
			Mode:      f.Mode,
			Component: f.Component,
			Kind:      f.Kind,
			OnChange:  f.OnChange,
			Secret:    f.Secret,
		}
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(s.ChecksumsPath(), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", s.ChecksumsPath(), err)
	}
	if err := pruneDesiredFiles(filepath.Join(root, "files"), keep); err != nil {
		return fmt.Errorf("清理 desired 旧文件: %w", err)
	}
	return nil
}

// pruneDesiredFiles 删除 desired/files 下不再由当前 spec 声明的文件。
// 旧实现只写不删：切换 plugin/registry 后旧文件（可能含凭据）会永久留在
// 快照里，既占磁盘也扩大敏感内容留存面。
func pruneDesiredFiles(filesDir string, keep map[string]bool) error {
	if _, err := os.Stat(filesDir); os.IsNotExist(err) {
		return nil
	}
	var errs []error
	err := filepath.WalkDir(filesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(filesDir, p)
		if relErr != nil {
			return relErr
		}
		if keep[filepath.ToSlash(rel)] {
			return nil
		}
		if removeErr := os.Remove(p); removeErr != nil {
			errs = append(errs, removeErr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 自底向上删除空目录；不碰 filesDir 本身。
	var dirs []string
	_ = filepath.WalkDir(filesDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != filesDir {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
	return errors.Join(errs...)
}

// Load 读取持久化的快照。
func (s *Store) Load() (*Snapshot, error) {
	data, err := os.ReadFile(s.ChecksumsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	snap := &Snapshot{}
	if err := json.Unmarshal(data, snap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.ChecksumsPath(), err)
	}
	return snap, nil
}

// Restore 从持久化的期望状态树重建 Spec。
// 用于两种场景：集群不可达时按上次渲染结果修复；以及把 desired 与集群里的
// 配置版本比对，判断“本机是否落后于配置”。
func (s *Store) Restore() (*Spec, error) {
	snap, err := s.Load()
	if err != nil || snap == nil {
		return nil, err
	}
	spec := &Spec{
		Node:      snap.Node,
		ClusterID: snap.ClusterID,
		Release:   snap.Release,
	}
	paths := make([]string, 0, len(snap.Files))
	for p := range snap.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		st := snap.Files[p]
		// Secret 内容不落 desired 镜像；Restore 返回的 spec 不包含它们。
		if st.Secret {
			continue
		}
		rel := File{Path: p}.RelPath(s.Paths.Root)
		content, err := os.ReadFile(filepath.Join(s.Dir(), "files", rel))
		if err != nil {
			return nil, fmt.Errorf("desired file %s is missing: %w", p, err)
		}
		spec.Add(File{
			Path:      p,
			Content:   content,
			Mode:      st.Mode,
			Component: st.Component,
			Kind:      st.Kind,
			OnChange:  st.OnChange,
			Secret:    st.Secret,
		})
	}
	spec.Sort()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

// DriftState 描述实际文件与期望状态的差异类型。
type DriftState string

// DriftState 的取值。
const (
	DriftMissing     DriftState = "missing"
	DriftModified    DriftState = "modified"
	DriftModeChanged DriftState = "mode-changed"
)

// DriftItem 是一个发生漂移的文件。
type DriftItem struct {
	File  File
	State DriftState
}

// Drift 比较期望 spec 与实际文件系统。
// 只报告，不修复：修复动作（是否重启组件、是否走原子替换）由调用方决定。
func Drift(spec *Spec) ([]DriftItem, error) {
	if spec == nil {
		return nil, nil
	}
	var out []DriftItem
	for _, f := range spec.Files {
		info, err := os.Stat(f.Path)
		if err != nil {
			if os.IsNotExist(err) {
				out = append(out, DriftItem{File: f, State: DriftMissing})
				continue
			}
			return nil, err
		}
		live, err := os.ReadFile(f.Path)
		if err != nil {
			return nil, err
		}
		if contentDigestForDrift(live) != contentDigestForDrift(f.Content) {
			out = append(out, DriftItem{File: f, State: DriftModified})
			continue
		}
		if info.Mode().Perm() != os.FileMode(f.Mode).Perm() {
			out = append(out, DriftItem{File: f, State: DriftModeChanged})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File.Path < out[j].File.Path })
	return out, nil
}
