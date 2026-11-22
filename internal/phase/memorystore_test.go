package phase

import "sync"

// MemoryStore 是测试使用的进程内存储。
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]map[string]PhaseRecord
}

// NewMemoryStore 返回一个空的进程内存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[string]map[string]PhaseRecord{}}
}

// Record 实现 Store。
func (m *MemoryStore) Record(scope string, rec PhaseRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records[scope] == nil {
		m.records[scope] = map[string]PhaseRecord{}
	}
	m.records[scope][rec.Name] = rec
	return nil
}

// Get 返回测试中写入的记录。
func (m *MemoryStore) Get(scope, name string) (PhaseRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[scope][name]
	return rec, ok
}
