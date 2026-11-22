package phase

import (
	"fmt"
	"sort"
	"sync"
)

// CanonicalOrder 是内置 phase 的执行顺序。
// 顺序即依赖：preflight → host-init → runtime → certs →
// kubeconfig → local-proxy → kubelet → etcd → etcd-health → control-plane →
// cluster-init → mark-control-plane → network → addon。
// 与直觉不同，kubelet 排在 etcd/control-plane 之前：控制面与 etcd 是 systemd
// unit，而 worker 本地代理是 kubelet 管理的静态 Pod；静态 Pod
// 只有 kubelet 在运行时才存在，否则 local-proxy phase 的健康等待会超时。
// local-proxy phase 只负责写配置与清单；worker 的 TLS Bootstrap 直连 master，
// 不依赖本地代理。
var CanonicalOrder = []string{
	"preflight",
	"host-init",
	"runtime",
	"certs",
	"kubeconfig",
	"local-proxy",
	"kubelet",
	"etcd",
	"etcd-health",
	"control-plane",
	"cluster-init",
	"mark-control-plane",
	"network",
	"addon",
}

// Registry 保存一个 kubemono 二进制可知的 phase。
type Registry struct {
	mu     sync.RWMutex
	phases map[string]Phase
}

// NewRegistry 返回空 registry。
func NewRegistry() *Registry {
	return &Registry{phases: map[string]Phase{}}
}

var defaultRegistry = NewRegistry()

// Default 返回进程级 registry。
// phase 实现包通过 Default.MustRegister(...) 在 init 中登记自己；
// 单一二进制（D-单一二进制原则）因此不需要任何插件机制。
func Default() *Registry { return defaultRegistry }

// MustRegister 把 phase 注册到进程级 registry，失败即 panic。
func MustRegister(p Phase) { defaultRegistry.MustRegister(p) }

// Register 注册一个 phase；重名视为错误（init 期 bug）。
func (r *Registry) Register(p Phase) error {
	if p == nil {
		return fmt.Errorf("phase: cannot register a nil phase")
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("phase: name must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.phases[name]; exists {
		return fmt.Errorf("phase %s is registered twice", name)
	}
	r.phases[name] = p
	return nil
}

// MustRegister 在注册出错时 panic；供 phase 包的 init 使用。
func (r *Registry) MustRegister(p Phase) {
	if err := r.Register(p); err != nil {
		panic(err)
	}
}

// Get 按名字返回 phase。
func (r *Registry) Get(name string) (Phase, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.phases[name]
	return p, ok
}

// Has 报告某 phase 是否已注册。
func (r *Registry) Has(name string) bool {
	_, ok := r.Get(name)
	return ok
}

// SortCanonical 按 CanonicalOrder 排序名字；未知名字排在最后并按字母序。
// 这是 phase 顺序的唯一排序实现：install 的 phasePlans 只表达“哪些 phase
// 适用”，不再表达顺序。
func SortCanonical(names []string) []string {
	rank := make(map[string]int, len(CanonicalOrder))
	for i, n := range CanonicalOrder {
		rank[n] = i
	}
	out := append([]string(nil), names...)
	sort.Slice(out, func(i, j int) bool {
		ri, iok := rank[out[i]]
		rj, jok := rank[out[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true
		case jok:
			return false
		default:
			return out[i] < out[j]
		}
	})
	return out
}

// Names 按规范顺序返回已注册的 phase 名，未知名字排在最后。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.phases))
	for name := range r.phases {
		out = append(out, name)
	}
	return SortCanonical(out)
}

// Select 把选择表达式解析为有序的 phase 列表。
// 选择表达式与内部的 phase 选择逻辑共用：
//
//	空 → 全部已注册 phase，按规范顺序
//	"a,b" → 只执行 a、b（按规范顺序，而不是书写顺序）
//	"a" → 只执行 a
//
// 选择不存在的 phase 会报错，避免拼错名字导致“什么都没做”。
func (r *Registry) Select(only []string) ([]Phase, error) {
	names := r.Names()
	onlySet := map[string]bool{}
	for _, n := range only {
		if n == "" {
			continue
		}
		if !r.Has(n) {
			return nil, fmt.Errorf("unknown phase %q; run `kubemono init`/`kubemono join` 的安装输出 to see available phases", n)
		}
		onlySet[n] = true
	}
	var out []Phase
	for _, name := range names {
		if len(onlySet) > 0 && !onlySet[name] {
			continue
		}
		p, _ := r.Get(name)
		out = append(out, p)
	}
	if len(onlySet) > 0 && len(out) == 0 {
		return nil, fmt.Errorf("phase selection is empty")
	}
	return out, nil
}
