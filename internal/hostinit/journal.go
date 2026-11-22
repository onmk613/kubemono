package hostinit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"kubemono/internal/util/fsutil"
)

// EntryAbsent 是 Entry.Before 的哨兵值，表示目标在变更前不存在。
// 它含有 NUL 字节，正常文本内容不会与之相等，因此可以区分
// “操作前不存在”和“操作前存在但内容为空”，从而支持精确还原。
const EntryAbsent = "\x00absent\x00"

// defaultFileMode 是还原文件时使用的权限位。
// kubemono 在 host init 阶段只写 modules-load.d 与 sysctl.d 两个受管文件，均为 0644。
const defaultFileMode fs.FileMode = 0o644

// journalVersion 是 hostinit.json 的格式版本。
const journalVersion = 1

// Entry 是一条已应用的变更记录。
type Entry struct {
	// ID 是记录的唯一标识（为空时由 Record 生成）。
	ID string `json:"id"`
	// StepID 是产生该记录的步骤 ID。
	StepID string `json:"stepID,omitempty"`
	// Kind 是动作类型：file|sysctl|module|dir|hook。
	Kind string `json:"kind"`
	// Target 是动作对象（文件路径、sysctl 键、unit 名、端口等）。
	Target string `json:"target"`
	// Before 是变更前的状态；不存在时为 EntryAbsent。
	Before string `json:"before,omitempty"`
	// After 是变更后的状态。
	After string `json:"after,omitempty"`
	// Revertible 表示 Reset 能否自动还原这条记录。
	Revertible bool `json:"revertible"`
	// AppliedAt 是记录时间。
	AppliedAt time.Time `json:"appliedAt"`
}

// Journal 是变更记录（/usr/local/kubemono/hostinit.json）。
// Path 为空表示内存记录（测试或 dry-run 用）。
type Journal struct {
	Path    string  `json:"-"`
	Entries []Entry `json:"entries"`
}

// journalFile 是 hostinit.json 的磁盘格式。
type journalFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// NewMemory 返回不落盘的内存 Journal（测试用）。
func NewMemory() *Journal { return &Journal{} }

// Load 读取变更记录。文件不存在时返回空记录（而不是错误），
// 这样第一次 host init 可以从零开始。
func Load(path string) (*Journal, error) {
	j := &Journal{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return j, nil
		}
		return nil, fmt.Errorf("读取主机初始化记录 %s 失败: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return j, nil
	}
	var f journalFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析主机初始化记录 %s 失败: %w", path, err)
	}
	j.Entries = f.Entries
	return j, nil
}

// Record 追加一条记录并原子落盘（临时文件 + fsync + rename，权限 0600）。
// Apply 在真正改动主机之前调用它，因此即使中途崩溃，记录也已经可读。
func (j *Journal) Record(entry Entry) error {
	// 同一步骤重复 Apply（幂等修复、host-init 重跑）不能不断追加记录：
	// 既会让 journal 无限增长，也会让 Reset 对同一对象回放多次。保留**第一次**
	// 的 Before（那才是受管前的原始状态），只刷新 After/时间。
	if entry.StepID != "" {
		for i := range j.Entries {
			e := &j.Entries[i]
			if e.StepID == entry.StepID && e.Kind == entry.Kind && e.Target == entry.Target {
				e.After = entry.After
				if !entry.AppliedAt.IsZero() {
					e.AppliedAt = entry.AppliedAt
				}
				return j.Save()
			}
		}
	}
	if entry.ID == "" {
		id, err := newEntryID()
		if err != nil {
			return err
		}
		entry.ID = id
	}
	if entry.AppliedAt.IsZero() {
		entry.AppliedAt = time.Now().UTC()
	}
	j.Entries = append(j.Entries, entry)
	return j.Save()
}

// Save 把当前记录原子写入 Path；Path 为空时是空操作。
func (j *Journal) Save() error {
	if j == nil || j.Path == "" {
		return nil
	}
	data, err := json.MarshalIndent(journalFile{Version: journalVersion, Entries: j.Entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化主机初始化记录失败: %w", err)
	}
	data = append(data, '\n')

	if err := fsutil.WriteFileAtomic(j.Path, data, 0o600); err != nil {
		return fmt.Errorf("写入主机初始化记录 %s 失败: %w", j.Path, err)
	}
	return nil
}

// newEntryID 生成 16 位十六进制随机 ID。
func newEntryID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成记录 ID 失败: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
