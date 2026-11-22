package render

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
	"kubemono/internal/util/output"
)

// ChangeKind 描述两次 spec 之间某个文件的变化。
type ChangeKind string

// 变更类型。
const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeRemoved  ChangeKind = "removed"
)

// FileChange 是旧状态与新期望状态之间的一处差异。
type FileChange struct {
	File File
	Kind ChangeKind
	// Previous 是操作前的文件内容。
	Previous *File
}

// ComputeChanges 比较两份 spec 并返回按路径排序的变化。
// 传入 nil 的 old 表示“首次渲染本节点”：所有文件都是新增。
func ComputeChanges(old, new *Spec) []FileChange {
	var out []FileChange
	oldFiles := map[string]File{}
	if old != nil {
		for _, f := range old.Files {
			oldFiles[f.Path] = f
		}
	}
	newFiles := map[string]File{}
	for _, f := range new.Files {
		newFiles[f.Path] = f
	}
	for _, f := range new.Files {
		prev, existed := oldFiles[f.Path]
		switch {
		case !existed:
			out = append(out, FileChange{File: f, Kind: ChangeAdded})
		case contentDigestForDrift(prev.Content) != contentDigestForDrift(f.Content):
			p := prev
			out = append(out, FileChange{File: f, Kind: ChangeModified, Previous: &p})
		case prev.Mode != f.Mode:
			// 只有权限变化也必须落盘：kubeconfig 从 0644 收窄到 0600 是安全修复。
			p := prev
			out = append(out, FileChange{File: f, Kind: ChangeModified, Previous: &p})
		}
	}
	for path, f := range oldFiles {
		if _, ok := newFiles[path]; !ok {
			out = append(out, FileChange{File: f, Kind: ChangeRemoved})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File.Path < out[j].File.Path })
	return out
}

// PendingActions 返回每个组件最强的后续动作。
// 内容变了但动作是 none 的文件（例如 certs.d 镜像仓库配置）不产生动作，
// 因此也不会触发任何重启。
func PendingActions(changes []FileChange) map[string]Action {
	rank := map[Action]int{ActionNone: 0, ActionReload: 1, ActionRestart: 2, ActionRecreate: 3}
	out := map[string]Action{}
	for _, ch := range changes {
		if ch.Kind == ChangeRemoved {
			continue
		}
		cur, ok := out[ch.File.Component]
		if !ok || rank[ch.File.OnChange] > rank[cur] {
			out[ch.File.Component] = ch.File.OnChange
		}
	}
	return out
}

// ApplyOptions 控制如何把变化写入主机。
type ApplyOptions struct {
	// BackupDir 是被替换文件覆盖前的备份位置。
	// 为空表示不备份（`--dry-run` 和测试使用）。
	BackupDir string
	// Timestamp 是备份子目录的名称。
	Timestamp string
	// DryRun 只报告变化，不写入任何内容。
	DryRun bool
	// Reporter 接收进度消息。
	Reporter output.Reporter
}

// ApplyChanges 是 Apply 的上下文版：备份目录固定为 <paths>/backup，时间戳取 now。
// 四个 phase 共用它，避免各自拼 ApplyOptions 时漏掉备份目录或时间戳。
func ApplyChanges(changes []FileChange, paths constants.Paths, now time.Time, report output.Reporter) ([]AppliedChange, error) {
	return Apply(changes, ApplyOptions{
		BackupDir: paths.Backup(),
		Timestamp: fsutil.Timestamp(now),
		Reporter:  report,
	})
}

// AppliedChange 记录一次文件变更的结果。
type AppliedChange struct {
	FileChange
	// BackupPath 是旧内容的保存位置（如果有）。
	BackupPath string
}

func (o ApplyOptions) reporter() output.Reporter {
	if o.Reporter == nil {
		return output.Discard()
	}
	return o.Reporter
}

// Apply 把变化写入主机。
// 语义：
// - 新增/修改：先备份旧内容到 /usr/local/kubemono/backup/<ts>/，再原子替换；
// - 删除：只删除 kubemono 自己渲染过的文件，并且同样先备份；
// - 任何一个文件失败都立即返回错误，调用方据此决定是否回滚本节点。
func Apply(changes []FileChange, opts ApplyOptions) ([]AppliedChange, error) {
	if opts.Timestamp == "" {
		opts.Timestamp = fsutil.Timestamp(time.Now())
	}
	out := make([]AppliedChange, 0, len(changes))
	for _, ch := range changes {
		applied := AppliedChange{FileChange: ch}
		switch ch.Kind {
		case ChangeRemoved:
			if opts.DryRun {
				opts.reporter().Infof("将删除 %s（dry-run）", ch.File.Path)
				out = append(out, applied)
				continue
			}
			if opts.BackupDir != "" && fsutil.FileExists(ch.File.Path) {
				path, err := fsutil.Backup(ch.File.Path, opts.BackupDir, opts.Timestamp)
				if err != nil {
					return out, fmt.Errorf("backup %s: %w", ch.File.Path, err)
				}
				applied.BackupPath = path
			}
			if err := fsutil.RemoveIfExists(ch.File.Path); err != nil {
				return out, fmt.Errorf("remove %s: %w", ch.File.Path, err)
			}
			opts.reporter().Infof("已删除 %s", ch.File.Path)
		default:
			if opts.DryRun {
				opts.reporter().Infof("将写入 %s（%04o，%s，dry-run）", ch.File.Path, ch.File.Mode, ch.File.OnChange)
				out = append(out, applied)
				continue
			}
			if opts.BackupDir != "" && fsutil.FileExists(ch.File.Path) {
				path, err := fsutil.Backup(ch.File.Path, opts.BackupDir, opts.Timestamp)
				if err != nil {
					return out, fmt.Errorf("backup %s: %w", ch.File.Path, err)
				}
				applied.BackupPath = path
			}
			if _, err := fsutil.WriteFileIfChanged(ch.File.Path, ch.File.Content, os.FileMode(ch.File.Mode)); err != nil {
				return out, fmt.Errorf("write %s: %w", ch.File.Path, err)
			}
			opts.reporter().Infof("已写入 %s", ch.File.Path)
		}
		out = append(out, applied)
	}
	// 备份只保留最近若干份：每次改配置都留一份会长期吃满磁盘，且旧备份里
	// 可能含 registry 凭据/CA 材料。清理失败只告警，不影响本次应用。
	if len(out) > 0 && opts.BackupDir != "" && !opts.DryRun {
		if err := fsutil.PruneTimestampedDirs(opts.BackupDir, fsutil.DefaultBackupKeep); err != nil {
			opts.reporter().Warnf("清理旧备份失败：%v", err)
		}
	}
	return out, nil
}

// Rollback 恢复已应用变化的旧内容。
// 只回滚文件，不负责重启组件：调用方在回滚之后统一做一次健康检查。
func Rollback(applied []AppliedChange) error {
	var firstErr error
	for _, ch := range slices.Backward(applied) {
		switch ch.Kind {
		case ChangeRemoved:
			// 文件被删除了：从备份恢复。
			if ch.BackupPath == "" {
				continue
			}
			if err := fsutil.CopyFile(ch.BackupPath, ch.File.Path); err != nil && firstErr == nil {
				firstErr = err
			}
		default:
			if ch.Previous == nil {
				// 之前不存在：删除新写入的文件。
				if err := fsutil.RemoveIfExists(ch.File.Path); err != nil && firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := fsutil.WriteFileAtomic(ch.File.Path, ch.Previous.Content, os.FileMode(ch.Previous.Mode)); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
