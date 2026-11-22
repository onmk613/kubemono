package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kubemono/internal/util/fsutil"
)

// VerifyOptions 控制齐备性检查的深度。零值表示“能查的都查”。
type VerifyOptions struct {
	// NoHash 跳过逐文件 sha256（只查存在、大小、权限、架构）。
	NoHash bool
	// NoArch 跳过 ELF/Mach-O 架构检查。
	NoArch bool
}

// HashMismatch 描述一个内容与 meta 不一致的文件。
type HashMismatch struct {
	Path string `json:"path"`
	Want string `json:"want"`
	Got  string `json:"got"`
}

// SizeMismatch 描述一个大小与 meta 不一致的文件。
type SizeMismatch struct {
	Path string `json:"path"`
	Want int64  `json:"want"`
	Got  int64  `json:"got"`
}

// ModeMismatch 描述一个权限与 meta 不一致的文件。
type ModeMismatch struct {
	Path    string      `json:"path"`
	Want    fs.FileMode `json:"-"`
	Got     fs.FileMode `json:"-"`
	Missing bool        `json:"missing,omitempty"`
}

// String 渲染权限不匹配。
func (m ModeMismatch) String() string {
	if m.Missing {
		return fmt.Sprintf("%s 缺失（期望 %04o）", m.Path, m.Want.Perm())
	}
	return fmt.Sprintf("%s 权限是 %04o，meta 记录 %04o", m.Path, m.Got.Perm(), m.Want.Perm())
}

// Report 是一次齐备性检查的结果。
type Report struct {
	Source         string         `json:"source"`
	Meta           *Meta          `json:"-"`
	Release        string         `json:"release"`
	OS             string         `json:"os"`
	Arch           string         `json:"arch"`
	FilesChecked   int            `json:"filesChecked"`
	Missing        []string       `json:"missing,omitempty"`
	Unknown        []string       `json:"unknown,omitempty"`
	HashMismatches []HashMismatch `json:"hashMismatches,omitempty"`
	SizeMismatches []SizeMismatch `json:"sizeMismatches,omitempty"`
	ModeMismatches []ModeMismatch `json:"modeMismatches,omitempty"`
	ArchMismatches []string       `json:"archMismatches,omitempty"`
}

// OK 报告是否通过全部检查。
func (r *Report) OK() bool {
	if r == nil {
		return false
	}
	return len(r.Missing) == 0 && len(r.Unknown) == 0 &&
		len(r.HashMismatches) == 0 && len(r.SizeMismatches) == 0 &&
		len(r.ModeMismatches) == 0 && len(r.ArchMismatches) == 0
}

// Issue 是一类校验问题及其数量；Long/Short 分别是错误信息与命令行摘要的措辞。
type Issue struct {
	Long  string
	Short string
	Count int
}

// Issues 按固定顺序返回非空的问题分类：错误信息与命令行摘要共用这一份统计。
func (r *Report) Issues() []Issue {
	if r == nil {
		return nil
	}
	var out []Issue
	for _, issue := range []Issue{
		{Long: "文件缺失", Short: "缺失", Count: len(r.Missing)},
		{Long: "文件不在 meta 里", Short: "多余", Count: len(r.Unknown)},
		{Long: "文件哈希不匹配", Short: "哈希不匹配", Count: len(r.HashMismatches)},
		{Long: "文件大小不匹配", Short: "大小不匹配", Count: len(r.SizeMismatches)},
		{Long: "文件权限不匹配", Short: "权限不匹配", Count: len(r.ModeMismatches)},
		{Long: "文件架构不匹配", Short: "架构不匹配", Count: len(r.ArchMismatches)},
	} {
		if issue.Count > 0 {
			out = append(out, issue)
		}
	}
	return out
}

// Error 把校验结果汇总为 error。
func (r *Report) Error() error {
	if r == nil {
		return fmt.Errorf("artifact: 校验结果为空")
	}
	if r.OK() {
		return nil
	}
	parts := make([]string, 0, 6)
	for _, issue := range r.Issues() {
		parts = append(parts, fmt.Sprintf("%d 个%s", issue.Count, issue.Long))
	}
	return fmt.Errorf("artifact: 物料 %s 校验失败：%s", r.Source, strings.Join(parts, "；"))
}

// VerifyDir 校验一个已落盘的物料目录。
func VerifyDir(dir string, opts VerifyOptions) (*Report, error) {
	meta, err := LoadMeta(dir)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Source:  dir,
		Meta:    meta,
		Release: meta.Release,
		OS:      meta.OS,
		Arch:    meta.Arch,
	}
	seen := make(map[string]bool, len(meta.Files))
	for _, f := range meta.Files {
		seen[f.Path] = true
	}
	for _, f := range meta.Files {
		abs, err := SafeJoin(dir, f.Path)
		if err != nil {
			report.Unknown = append(report.Unknown, f.Path)
			continue
		}
		info, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				report.Missing = append(report.Missing, f.Path)
				continue
			}
			return nil, fmt.Errorf("artifact: 读取 %s 失败: %w", f.Path, err)
		}
		if !info.Mode().IsRegular() {
			report.Unknown = append(report.Unknown, f.Path)
			continue
		}
		if info.Size() != f.Size {
			report.SizeMismatches = append(report.SizeMismatches, SizeMismatch{Path: f.Path, Want: f.Size, Got: info.Size()})
		}
		mode, err := f.FileMode()
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm() != mode.Perm() {
			report.ModeMismatches = append(report.ModeMismatches, ModeMismatch{Path: f.Path, Want: mode, Got: info.Mode()})
		}
		if !opts.NoHash && f.SHA256 != "" {
			got, err := fsutil.SHA256File(abs)
			if err != nil {
				return nil, fmt.Errorf("artifact: 计算 %s 的 sha256 失败: %w", f.Path, err)
			}
			if !strings.EqualFold(got, f.SHA256) {
				report.HashMismatches = append(report.HashMismatches, HashMismatch{Path: f.Path, Want: f.SHA256, Got: got})
			}
		}
		if !opts.NoArch && isExecutable(f.Path, mode) {
			if err := CheckExecutable(abs, meta.OS, meta.Arch); err != nil {
				report.ArchMismatches = append(report.ArchMismatches, err.Error())
			}
		}
		report.FilesChecked++
	}
	if err := walkMaterial(dir, func(rel string, info fs.FileInfo) {
		if seen[rel] {
			return
		}
		if rel == MetaName || rel == CompleteName {
			return
		}
		report.Unknown = append(report.Unknown, rel)
	}); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, DirImages, OCILayoutFile)); err == nil {
		if _, err := LayoutReferences(filepath.Join(dir, DirImages)); err != nil {
			report.Unknown = append(report.Unknown, DirImages+"/: "+err.Error())
		}
	}
	sortStrings(report.Missing)
	sortStrings(report.Unknown)
	return report, nil
}

// VerifyArchive 校验归档结构、大小、权限与（可选的）哈希，不解包。
func VerifyArchive(ctx context.Context, archivePath string, opts VerifyOptions) (*Report, error) {
	a, err := openArchive(archivePath)
	if err != nil {
		return nil, err
	}
	defer a.close()
	meta, _, err := a.readMeta()
	if err != nil {
		return nil, err
	}
	report := &Report{
		Source:  archivePath,
		Meta:    meta,
		Release: meta.Release,
		OS:      meta.OS,
		Arch:    meta.Arch,
	}
	seen := map[string]bool{MetaName: true}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := a.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("artifact: 读取归档失败: %w", err)
		}
		if structuralEntryError(hdr, seen) != nil {
			report.Unknown = append(report.Unknown, hdr.Name)
			if _, err := io.Copy(io.Discard, a.tr); err != nil {
				return nil, err
			}
			continue
		}
		entry, ok := meta.Find(hdr.Name)
		if !ok {
			report.Unknown = append(report.Unknown, hdr.Name)
			if _, err := io.Copy(io.Discard, a.tr); err != nil {
				return nil, err
			}
			continue
		}
		seen[hdr.Name] = true
		report.FilesChecked++
		if hdr.Size != entry.Size {
			report.SizeMismatches = append(report.SizeMismatches, SizeMismatch{Path: entry.Path, Want: entry.Size, Got: hdr.Size})
		}
		wantMode, err := entry.FileMode()
		if err != nil {
			return nil, err
		}
		if got := fs.FileMode(hdr.Mode).Perm(); got != wantMode.Perm() {
			report.ModeMismatches = append(report.ModeMismatches, ModeMismatch{Path: entry.Path, Want: wantMode, Got: got})
		}
		h := sha256.New()
		if _, err := io.Copy(h, a.tr); err != nil {
			return nil, fmt.Errorf("artifact: 读取 %s 失败: %w", entry.Path, err)
		}
		if !opts.NoHash && entry.SHA256 != "" {
			got := "sha256:" + hex.EncodeToString(h.Sum(nil))
			if !strings.EqualFold(got, entry.SHA256) {
				report.HashMismatches = append(report.HashMismatches, HashMismatch{Path: entry.Path, Want: entry.SHA256, Got: got})
			}
		}
	}
	for _, f := range meta.Files {
		if !seen[f.Path] {
			report.Missing = append(report.Missing, f.Path)
		}
	}
	sortStrings(report.Missing)
	sortStrings(report.Unknown)
	return report, nil
}

// walkMaterial 遍历物料目录里的普通文件（相对路径）。
// 目录与符号链接不进入 walk；调用方按 meta 判断是否合法。
func walkMaterial(dir string, fn func(rel string, info fs.FileInfo)) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(rel), info)
		return nil
	})
}

func isExecutable(p string, mode fs.FileMode) bool {
	if mode.Perm()&0o111 == 0 {
		return false
	}
	return strings.HasPrefix(p, DirBin+"/")
}

func sortStrings(in []string) {
	if len(in) < 2 {
		return
	}
	sort.Slice(in, func(i, j int) bool { return in[i] < in[j] })
}
