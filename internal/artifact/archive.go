package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"kubemono/internal/util/fsutil"
)

// zstdWindowMax 限制解码器内存：生产节点可能只有 2GiB 内存。
const zstdWindowMax = 128 << 20

// ArchiveName 返回一个 release/arch 的归档文件名。
// images=false 时使用 -lite 后缀（发布方据此区分是否携带镜像）。
func ArchiveName(release, osName, arch string, images bool) string {
	suffix := ""
	if !images {
		suffix = "-lite"
	}
	return fmt.Sprintf("kubemono-%s-%s-%s%s.tar.zst", release, osName, arch, suffix)
}

// WriteArchive 把 root 下的物料写成 zstd tar。
// meta.json 必须是第一个条目：load 只解压一遍就能先拿到清单。
// meta 里的 sha256 由调用方预计算；这里在复制时复核大小与哈希。
func WriteArchive(w io.Writer, meta *Meta, root string) error {
	if w == nil {
		return fmt.Errorf("artifact: 归档输出为 nil")
	}
	if meta == nil {
		return fmt.Errorf("artifact: 物料 meta 为空")
	}
	raw, err := MarshalMeta(meta)
	if err != nil {
		return err
	}
	zw, err := zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(true),
		zstd.WithWindowSize(8<<20),
	)
	if err != nil {
		return fmt.Errorf("artifact: 初始化 zstd 写入器失败: %w", err)
	}
	tw := tar.NewWriter(zw)
	fail := func(err error) error {
		_ = tw.Close()
		_ = zw.Close()
		return err
	}
	if err := writeTarEntry(tw, MetaName, 0o644, int64(len(raw)), meta.Created, bytes.NewReader(raw), ""); err != nil {
		return fail(err)
	}
	for _, f := range meta.Files {
		src, err := SafeJoin(root, f.Path)
		if err != nil {
			return fail(err)
		}
		info, err := os.Stat(src)
		if err != nil {
			return fail(fmt.Errorf("artifact: 读取物料 %s 失败: %w", f.Path, err))
		}
		if !info.Mode().IsRegular() {
			return fail(fmt.Errorf("artifact: 物料 %s 不是普通文件", f.Path))
		}
		if info.Size() != f.Size {
			return fail(fmt.Errorf("artifact: 物料 %s 大小是 %d，meta 记录 %d", f.Path, info.Size(), f.Size))
		}
		mode, err := f.FileMode()
		if err != nil {
			return fail(err)
		}
		fh, err := os.Open(src)
		if err != nil {
			return fail(fmt.Errorf("artifact: 打开物料 %s 失败: %w", f.Path, err))
		}
		err = writeTarEntry(tw, f.Path, mode, f.Size, meta.Created, fh, f.SHA256)
		_ = fh.Close()
		if err != nil {
			return fail(err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return fmt.Errorf("artifact: 关闭 tar 流失败: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("artifact: 关闭 zstd 流失败: %w", err)
	}
	return nil
}

// writeTarEntry 写一个普通文件条目，并在复制时复核大小与（可选的）sha256。
func writeTarEntry(tw *tar.Writer, name string, mode fs.FileMode, size int64, mod time.Time, r io.Reader, want string) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(mode.Perm()),
		Size:     size,
		Typeflag: tar.TypeReg,
		ModTime:  mod.UTC(),
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("artifact: 写入 %s 的 tar 头失败: %w", name, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), r)
	if err != nil {
		return fmt.Errorf("artifact: 写入 %s 失败: %w", name, err)
	}
	if n != size {
		return fmt.Errorf("artifact: %s 实际写入 %d 字节，meta 记录 %d", name, n, size)
	}
	if want != "" {
		got := "sha256:" + hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("artifact: %s 的内容与 meta 记录不一致（实际 %s，期望 %s）", name, got, want)
		}
	}
	return nil
}

// archiveReader 是归档的顺序读取器。
// raw 在开启摘要时是 io.TeeReader(f, hash)：解码器读到的每个压缩字节都会
// 进入哈希，避免为了校验再完整读一遍归档。
type archiveReader struct {
	f    *os.File
	raw  io.Reader
	hash hash.Hash
	zr   *zstd.Decoder
	tr   *tar.Reader
}

func openArchive(path string) (*archiveReader, error) { return openArchiveWithDigest(path, false) }

// openArchiveWithDigest 打开归档；digest=true 时同时计算归档文件的 sha256。
func openArchiveWithDigest(path string, digest bool) (*archiveReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("artifact: 打开归档 %s 失败: %w", path, err)
	}
	var (
		raw io.Reader = f
		h   hash.Hash
	)
	if digest {
		h = sha256.New()
		raw = io.TeeReader(f, h)
	}
	zr, err := zstd.NewReader(raw,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(zstdWindowMax),
		zstd.WithDecoderMaxMemory(zstdWindowMax*2),
	)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("artifact: 初始化 zstd 读取器失败: %w", err)
	}
	return &archiveReader{f: f, raw: raw, hash: h, zr: zr, tr: tar.NewReader(zr)}, nil
}

// digest 返回归档文件的 sha256。
// 先抽干 raw：zstd/tar 可能在归档尾部之前停止读取，未读的尾部字节也必须
// 计入摘要，否则"边读边算"得到的值与整文件哈希不一致。
func (a *archiveReader) digest() (string, error) {
	if a == nil || a.hash == nil {
		return "", fmt.Errorf("artifact: 未开启归档摘要计算")
	}
	if _, err := io.Copy(io.Discard, a.raw); err != nil {
		return "", fmt.Errorf("artifact: 读取归档摘要失败: %w", err)
	}
	return "sha256:" + hex.EncodeToString(a.hash.Sum(nil)), nil
}

func (a *archiveReader) close() {
	if a == nil {
		return
	}
	if a.zr != nil {
		a.zr.Close()
	}
	if a.f != nil {
		_ = a.f.Close()
	}
}

// readMeta 读取归档的第一个条目，要求它就是 meta.json。
func (a *archiveReader) readMeta() (*Meta, []byte, error) {
	hdr, err := a.tr.Next()
	if err != nil {
		return nil, nil, fmt.Errorf("artifact: 读取归档失败（缺少 %s）: %w", MetaName, err)
	}
	if hdr.Name != MetaName {
		return nil, nil, fmt.Errorf("artifact: 归档的第一个条目必须是 %s，实际是 %q", MetaName, hdr.Name)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, nil, fmt.Errorf("artifact: %s 不是普通文件", MetaName)
	}
	if hdr.Size < 0 || hdr.Size > maxMetaSize {
		return nil, nil, fmt.Errorf("artifact: %s 大小 %d 超过上限 %d", MetaName, hdr.Size, maxMetaSize)
	}
	data, err := io.ReadAll(io.LimitReader(a.tr, maxMetaSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("artifact: 读取 %s 失败: %w", MetaName, err)
	}
	m, err := ParseMeta(data)
	if err != nil {
		return nil, nil, fmt.Errorf("artifact: 解析 %s 失败: %w", MetaName, err)
	}
	return m, data, nil
}

// ReadMetaFromArchive 只读归档开头的 meta.json，不校验其余内容。
func ReadMetaFromArchive(path string) (*Meta, error) {
	a, err := openArchive(path)
	if err != nil {
		return nil, err
	}
	defer a.close()
	m, _, err := a.readMeta()
	return m, err
}

// InstallOptions 控制一次 load 的落盘行为。
type InstallOptions struct {
	// ExpectedSHA256 是归档的期望摘要（可带或不带 "sha256:" 前缀）。
	// 为空表示不校验归档本身。
	ExpectedSHA256 string
	// Force 为 true 时即使目标目录已经完整也重新解包。
	Force bool
	// Reporter 接收进度行（可选）。
	Reporter Reporter
	// MaxFiles / MaxBytes 限制解包规模；0 表示使用默认上限。
	// 默认值对正常 release 足够宽松，同时挡住"高压缩比 tar.zst 写满磁盘"的
	// 压缩炸弹。
	MaxFiles int
	MaxBytes int64
}

// 解包规模默认上限。
const (
	defaultMaxExtractFiles = 200_000
	defaultMaxExtractBytes = int64(64 << 30) // 64 GiB
)

// extractLimits 把 InstallOptions 的零值解析成默认上限。
type extractLimits struct {
	maxFiles int
	maxBytes int64
}

func (o InstallOptions) limits() extractLimits {
	l := extractLimits{maxFiles: o.MaxFiles, maxBytes: o.MaxBytes}
	if l.maxFiles <= 0 {
		l.maxFiles = defaultMaxExtractFiles
	}
	if l.maxBytes <= 0 {
		l.maxBytes = defaultMaxExtractBytes
	}
	return l
}

// Complete 是物料目录里的完成标记。
type Complete struct {
	Archive     string    `json:"archive,omitempty"`
	SHA256      string    `json:"sha256"`
	InstalledAt time.Time `json:"installedAt"`
}

// Install 把归档解到 destDir，并以目录级原子替换的方式落盘。
// 目标目录已存在且 .complete 记录的是同一份归档时直接跳过（除非 Force）。
func Install(ctx context.Context, archivePath, destDir string, opts InstallOptions) (*Meta, error) {
	if strings.TrimSpace(destDir) == "" {
		return nil, fmt.Errorf("artifact: 物料目标目录为空")
	}
	// 有期望摘要时以它作为"已安装"判定基准，避免为了算摘要先把大归档读一遍；
	// 真正的校验在解包时边读边算（见 extractArchive）。没有期望摘要时仍需预读
	// 一次，才能判断目标目录里的是否就是同一个归档。
	expected := ""
	if opts.ExpectedSHA256 != "" {
		var err error
		expected, err = NormalizeDigest(opts.ExpectedSHA256)
		if err != nil {
			return nil, err
		}
	}
	digest := expected
	if digest == "" {
		var err error
		digest, err = fsutil.SHA256File(archivePath)
		if err != nil {
			return nil, fmt.Errorf("artifact: 计算归档摘要失败: %w", err)
		}
	}
	if !opts.Force {
		if meta, ok := installedOK(destDir, digest); ok {
			opts.Reporter.Reportf("release %s 的物料已经在 %s，跳过解包（幂等）", meta.Release, destDir)
			return meta, nil
		}
	}
	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("artifact: 创建物料父目录 %s 失败: %w", parent, err)
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(destDir)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("artifact: 创建临时目录失败: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()

	opts.Reporter.Reportf("解包 %s → %s", archivePath, destDir)
	meta, actual, err := extractArchive(ctx, archivePath, tmp, opts.Reporter, opts.limits())
	if err != nil {
		return nil, err
	}
	if expected != "" && !strings.EqualFold(actual, expected) {
		return nil, fmt.Errorf("artifact: 归档 %s 的 sha256 是 %s，期望 %s（归档损坏或已被替换）", archivePath, actual, expected)
	}
	if digest != "" && !strings.EqualFold(actual, digest) {
		return nil, fmt.Errorf("artifact: 归档 %s 在读取期间发生变化（%s → %s）；请重试", archivePath, digest, actual)
	}
	digest = actual
	complete := Complete{Archive: filepath.Base(archivePath), SHA256: digest, InstalledAt: time.Now().UTC()}
	data, err := json.MarshalIndent(complete, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("artifact: 序列化完成标记失败: %w", err)
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(tmp, CompleteName), append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("artifact: 写入完成标记失败: %w", err)
	}

	backup := ""
	if _, err := os.Stat(destDir); err == nil {
		backup = destDir + ".old"
		if err := os.RemoveAll(backup); err != nil {
			return nil, fmt.Errorf("artifact: 清理旧的备份目录 %s 失败: %w", backup, err)
		}
		if err := os.Rename(destDir, backup); err != nil {
			return nil, fmt.Errorf("artifact: 备份旧物料目录 %s 失败: %w", destDir, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("artifact: 检查目标目录 %s 失败: %w", destDir, err)
	}
	if err := os.Rename(tmp, destDir); err != nil {
		if backup != "" {
			_ = os.Rename(backup, destDir)
		}
		return nil, fmt.Errorf("artifact: 落盘到 %s 失败: %w", destDir, err)
	}
	cleanup = false
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	opts.Reporter.Reportf("已导入 release %s 的物料到 %s（%d 个文件）", meta.Release, destDir, len(meta.Files))
	return meta, nil
}

// installedOK 报告 destDir 是否已经完整安装了同一份归档。
func installedOK(destDir, digest string) (*Meta, bool) {
	data, err := os.ReadFile(filepath.Join(destDir, CompleteName))
	if err != nil {
		return nil, false
	}
	var c Complete
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false
	}
	if !strings.EqualFold(c.SHA256, digest) {
		return nil, false
	}
	meta, err := LoadMeta(destDir)
	if err != nil {
		return nil, false
	}
	return meta, true
}

// checkExtractLimits 在写盘前挡住超大的 meta。
// 用逐项累加而不是 Meta.TotalSize，既避免 int64 溢出，也能给出准确的超限项。
func checkExtractLimits(meta *Meta, limits extractLimits) error {
	if len(meta.Files) > limits.maxFiles {
		return fmt.Errorf("artifact: 归档包含 %d 个文件，超过上限 %d", len(meta.Files), limits.maxFiles)
	}
	var total int64
	for _, f := range meta.Files {
		if f.Size > limits.maxBytes-total {
			return fmt.Errorf("artifact: 归档解压后超过 %d 字节上限（在 %s 处超限）", limits.maxBytes, f.Path)
		}
		total += f.Size
	}
	return nil
}

// extractArchive 单遍解压：先读 meta.json，再逐个校验并写出文件。
func extractArchive(ctx context.Context, archivePath, destDir string, reporter Reporter, limits extractLimits) (*Meta, string, error) {
	a, err := openArchiveWithDigest(archivePath, true)
	if err != nil {
		return nil, "", err
	}
	defer a.close()
	meta, raw, err := a.readMeta()
	if err != nil {
		return nil, "", err
	}
	if err := checkExtractLimits(meta, limits); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("artifact: 创建目录 %s 失败: %w", destDir, err)
	}
	if err := os.WriteFile(filepath.Join(destDir, MetaName), raw, 0o644); err != nil {
		return nil, "", fmt.Errorf("artifact: 写入 %s 失败: %w", MetaName, err)
	}
	seen := map[string]bool{MetaName: true}
	written := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		hdr, err := a.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("artifact: 读取归档失败: %w", err)
		}
		entry, err := checkArchiveEntry(hdr, meta, seen)
		if err != nil {
			return nil, "", err
		}
		dst, err := SafeJoin(destDir, entry.Path)
		if err != nil {
			return nil, "", err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, "", fmt.Errorf("artifact: 创建目录 %s 失败: %w", filepath.Dir(dst), err)
		}
		mode, _ := entry.FileMode()
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, "", fmt.Errorf("artifact: 创建 %s 失败: %w", entry.Path, err)
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(f, h), a.tr)
		if copyErr == nil {
			// 先 fsync 再写 .complete：否则断电后可能出现"完成标记已落盘、
			// 文件内容还在页缓存"，之后 Install 的幂等判定会跳过修复。
			copyErr = f.Sync()
		}
		closeErr := f.Close()
		if copyErr != nil {
			return nil, "", fmt.Errorf("artifact: 写入 %s 失败: %w", entry.Path, copyErr)
		}
		if closeErr != nil {
			return nil, "", fmt.Errorf("artifact: 关闭 %s 失败: %w", entry.Path, closeErr)
		}
		if n != entry.Size {
			return nil, "", fmt.Errorf("artifact: %s 实际 %d 字节，meta 记录 %d", entry.Path, n, entry.Size)
		}
		if entry.SHA256 != "" {
			got := "sha256:" + hex.EncodeToString(h.Sum(nil))
			if !strings.EqualFold(got, entry.SHA256) {
				return nil, "", fmt.Errorf("artifact: %s 内容与 meta 记录不一致（实际 %s，期望 %s）", entry.Path, got, entry.SHA256)
			}
		}
		if err := os.Chmod(dst, mode.Perm()); err != nil {
			return nil, "", fmt.Errorf("artifact: 设置 %s 权限失败: %w", entry.Path, err)
		}
		seen[entry.Path] = true
		written++
	}
	if err := missingFiles(meta, seen); err != nil {
		return nil, "", err
	}
	digest, err := a.digest()
	if err != nil {
		return nil, "", err
	}
	reporter.Reportf("已解出 %d 个文件", written)
	return meta, digest, nil
}

// structuralEntryError 校验归档条目的结构规则（普通文件/路径/保留名/重复）。
// VerifyArchive 与解包共用这一份，避免两侧的安全规则各自漂移。
func structuralEntryError(hdr *tar.Header, seen map[string]bool) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("artifact: 归档条目 %q 不是普通文件（type=%d）", hdr.Name, hdr.Typeflag)
	}
	if err := ValidateBundlePath(hdr.Name); err != nil {
		return fmt.Errorf("artifact: 归档条目 %q 非法: %w", hdr.Name, err)
	}
	if IsReservedPath(hdr.Name) {
		return fmt.Errorf("artifact: 归档条目 %q 由格式自身占用", hdr.Name)
	}
	if seen[hdr.Name] {
		return fmt.Errorf("artifact: 归档中存在重复条目 %q", hdr.Name)
	}
	return nil
}

// checkArchiveEntry 校验一个 tar 条目并返回它对应的 meta 记录。
func checkArchiveEntry(hdr *tar.Header, meta *Meta, seen map[string]bool) (FileEntry, error) {
	if err := structuralEntryError(hdr, seen); err != nil {
		return FileEntry{}, err
	}
	entry, ok := meta.Find(hdr.Name)
	if !ok {
		return FileEntry{}, fmt.Errorf("artifact: 归档中的 %q 不在 meta.json 里", hdr.Name)
	}
	if hdr.Size != entry.Size {
		return FileEntry{}, fmt.Errorf("artifact: %s 的 tar 大小是 %d，meta 记录 %d", hdr.Name, hdr.Size, entry.Size)
	}
	wantMode, err := entry.FileMode()
	if err != nil {
		return FileEntry{}, err
	}
	if got := fs.FileMode(hdr.Mode).Perm(); got != wantMode.Perm() {
		return FileEntry{}, fmt.Errorf("artifact: %s 的 tar 权限是 %04o，meta 记录 %04o", hdr.Name, got, wantMode.Perm())
	}
	return entry, nil
}

func missingFiles(meta *Meta, seen map[string]bool) error {
	var missing []string
	for _, f := range meta.Files {
		if !seen[f.Path] {
			missing = append(missing, f.Path)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("artifact: 归档缺少 %d 个 meta 声明的文件：%s", len(missing), strings.Join(missing, ", "))
	}
	return nil
}
