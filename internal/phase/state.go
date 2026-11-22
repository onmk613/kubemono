package phase

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// PhaseRecord 是一次 phase 执行结果的持久化记录，用于排障与审计。
// 续跑由各 phase 的 Check 决定；这里不做读取，避免把“记录过”误当成“已完成”。
type PhaseRecord struct {
	Name        string    `json:"name"`
	Done        bool      `json:"done"`
	AlreadyDone bool      `json:"alreadyDone,omitempty"`
	Error       string    `json:"error,omitempty"`
	AppliedAt   time.Time `json:"appliedAt"`
	Release     string    `json:"release,omitempty"`
}

// Store 按 scope（目前只有 "install"）持久化 phase 记录。
// 记录用于排障与审计；续跑由各 phase 的 Check 决定，不依赖读取历史。
type Store interface {
	Record(scope string, rec PhaseRecord) error
}

// FileStore 把记录以 JSON 形式存放在 <kubemono-lib>/state/<scope>.json 下。
type FileStore struct {
	mu    sync.Mutex
	paths constants.Paths
}

// NewFileStore 返回一个写入给定路径的存储。
func NewFileStore(paths constants.Paths) *FileStore {
	return &FileStore{paths: paths}
}

// StateDir 返回存放状态文件的目录。
func (s *FileStore) StateDir() string {
	return s.paths.State()
}

func (s *FileStore) path(scope string) string {
	return filepath.Join(s.StateDir(), sanitizeScope(scope)+".json")
}

// sanitizeScope 防止通过 scope 名做路径穿越。
func sanitizeScope(scope string) string {
	out := make([]rune, 0, len(scope))
	for _, r := range scope {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "default"
	}
	return string(out)
}

// Load 读取某个 scope 的全部 phase 记录（只读，供 status/排障使用）。
func (s *FileStore) Load(scope string) (map[string]PhaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(scope)
}

// Record 保存或替换某个 phase 的记录。
// 状态文件损坏时用本次记录重建它，同时把损坏的事实返回给调用方：runner 只告警
// 并继续，下一次调用会读到干净的文件。不能直接返回错误，否则文件永远停留在
// 损坏状态，每次记录都只是重复告警。
func (s *FileStore) Record(scope string, rec PhaseRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, loadErr := s.loadLocked(scope)
	if loadErr != nil {
		records = map[string]PhaseRecord{}
	}
	records[rec.Name] = rec
	if saveErr := s.saveLocked(scope, records); saveErr != nil {
		return errors.Join(loadErr, saveErr)
	}
	return loadErr
}

func (s *FileStore) loadLocked(scope string) (map[string]PhaseRecord, error) {
	records := map[string]PhaseRecord{}
	data, err := os.ReadFile(s.path(scope))
	if err != nil {
		if os.IsNotExist(err) {
			return records, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return records, nil
	}
	if err := json.Unmarshal(data, &records); err != nil {
		// 状态文件损坏不应该阻塞安装：重建它，但要让调用方知道。
		return map[string]PhaseRecord{}, fmt.Errorf("phase state %s is corrupt: %w", s.path(scope), err)
	}
	return records, nil
}

func (s *FileStore) saveLocked(scope string, records map[string]PhaseRecord) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.path(scope), append(data, '\n'), 0o600)
}
