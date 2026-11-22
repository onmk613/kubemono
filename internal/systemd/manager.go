package systemd

import (
	"context"
	"time"

	"kubemono/internal/execx"
)

// Actioner 是 systemctl 的三个原语（Action/DaemonReload/IsActive）。
// runtime/etcd/controlplane 共用这一份声明：注入 systemd.Manager 或测试 fake。
type Actioner interface {
	Action(ctx context.Context, unit, action string) error
	DaemonReload(ctx context.Context) error
	IsActive(ctx context.Context, unit string) (bool, error)
}

// EnabledChecker 是 Actioner 的可选扩展：查询 unit 是否开机自启。
// 单独的窄接口让既有 fake 不必立刻实现它；phase 用类型断言探测。
type EnabledChecker interface {
	IsEnabled(ctx context.Context, unit string) (bool, error)
}

// Manager 是 Actioner 的生产实现（只使用默认执行器）。
//
// internal/etcd 与 internal/runtime 共用同一份窄接口（Actioner）与实现，
// 避免各自维护一份语义容易漂移的 SystemdManager。
type Manager struct {
	// Runner 可注入；nil 时使用 execx.Default()。
	Runner execx.Runner
	// Timeout 限制单次 systemctl 调用；<=0 表示不额外限时。
	Timeout time.Duration
}

func (m Manager) runner() execx.Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return execx.Default()
}

func (m Manager) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, m.Timeout)
}

// Action 实现 ServiceManager。
func (m Manager) Action(ctx context.Context, unit, action string) error {
	ctx, cancel := m.withTimeout(ctx)
	defer cancel()
	return Action(ctx, m.runner(), unit, action)
}

// DaemonReload 实现 ServiceManager。
func (m Manager) DaemonReload(ctx context.Context) error {
	ctx, cancel := m.withTimeout(ctx)
	defer cancel()
	return DaemonReload(ctx, m.runner())
}

// IsActive 实现 ServiceManager。
func (m Manager) IsActive(ctx context.Context, unit string) (bool, error) {
	ctx, cancel := m.withTimeout(ctx)
	defer cancel()
	return IsActive(ctx, m.runner(), unit)
}

// IsEnabled 实现 EnabledChecker。
func (m Manager) IsEnabled(ctx context.Context, unit string) (bool, error) {
	ctx, cancel := m.withTimeout(ctx)
	defer cancel()
	return IsEnabled(ctx, m.runner(), unit)
}

// Show 实现属性查询；供 status 展示上次结果/下次触发。
func (m Manager) Show(ctx context.Context, unit string, props ...string) (map[string]string, error) {
	ctx, cancel := m.withTimeout(ctx)
	defer cancel()
	return Show(ctx, m.runner(), unit, props...)
}
