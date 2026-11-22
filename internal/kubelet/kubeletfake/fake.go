package kubeletfake

import (
	"context"
	"slices"
	"sync"
)

// 本文件是给单元测试和离线预览用的内存实现。它们不依赖 root、systemd 或
// apiserver，因此 internal/phase/kubelet 的测试可以完整跑在开发机上。

// MemoryServiceManager 是内存版 ServiceManager。
type MemoryServiceManager struct {
	mu sync.Mutex
	// Active 记录 IsActive 观察到的 unit 状态。
	Active map[string]bool
	// Enabled 记录被 enable 过的 unit。
	Enabled map[string]bool
	// RestartErr 按 unit 注入重启失败。
	RestartErr map[string]error
	// Calls 以 "<method> <unit>" 的形式记录每次调用。
	Calls []string
}

// NewMemoryServiceManager 返回一个空的内存服务管理器。
func NewMemoryServiceManager() *MemoryServiceManager {
	return &MemoryServiceManager{
		Active:     map[string]bool{},
		Enabled:    map[string]bool{},
		RestartErr: map[string]error{},
	}
}

func (m *MemoryServiceManager) record(call string) {
	m.Calls = append(m.Calls, call)
}

// Enable 实现 ServiceManager。
func (m *MemoryServiceManager) Enable(_ context.Context, unit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("enable " + unit)
	m.Enabled[unit] = true
	return nil
}

// Restart 实现 ServiceManager。
func (m *MemoryServiceManager) Restart(_ context.Context, unit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("restart " + unit)
	if err := m.RestartErr[unit]; err != nil {
		return err
	}
	m.Active[unit] = true
	return nil
}

// Stop 实现 ServiceManager。
func (m *MemoryServiceManager) Stop(_ context.Context, unit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("stop " + unit)
	m.Active[unit] = false
	return nil
}

// IsActive 实现 ServiceManager。
func (m *MemoryServiceManager) IsActive(_ context.Context, unit string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("is-active " + unit)
	return m.Active[unit], nil
}

// Called 报告 "<method> <unit>" 是否被调用过。
func (m *MemoryServiceManager) Called(method, unit string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := method + " " + unit
	return slices.Contains(m.Calls, want)
}

// FakeNodeClient 是内存版 NodeClient。
type FakeNodeClient struct {
	// Ready 把节点名映射到它的 Ready 状态。
	Ready map[string]bool
	// Pending 是待上报的 serving CSR 数量。
	Pending int
	// NodeReadyErr 注入失败。
	NodeReadyErr error
	// NodeReadyCalls 与 PendingCalls 统计调用次数。
	NodeReadyCalls int
	PendingCalls   int
}

// NodeReady 实现 NodeClient。
func (f *FakeNodeClient) NodeReady(_ context.Context, name string) (bool, error) {
	f.NodeReadyCalls++
	if f.NodeReadyErr != nil {
		return false, f.NodeReadyErr
	}
	return f.Ready[name], nil
}

// PendingServingCSRs 实现 NodeClient。
func (f *FakeNodeClient) PendingServingCSRs(context.Context) (int, error) {
	f.PendingCalls++
	return f.Pending, nil
}
