package systemd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"kubemono/internal/execx"
	"kubemono/internal/util/output"
)

// ServiceManager 是 kubemono 托管 unit 的最小契约。
// kubelet phase 与 reset/leave 共用这一份实现；测试注入内存 fake。
type ServiceManager interface {
	// Enable 让 unit 开机自启（并先 reload systemd）。
	Enable(ctx context.Context, unit string) error
	// Restart 启动 unit，已在运行时则重启。
	Restart(ctx context.Context, unit string) error
	// Stop 停止 unit，但不取消开机自启。
	Stop(ctx context.Context, unit string) error
	// IsActive 报告 unit 当前是否处于 active。
	IsActive(ctx context.Context, unit string) (bool, error)
}

// UnitController 以幂等方式操作 systemd unit。
// 每个动作都先读状态：已经运行/已经停止时不重复执行 systemctl，
// 并在日志里明确区分“已停止”和“原本未运行”。
type UnitController struct {
	// Runner 执行 systemctl；为 nil 时所有动作都返回错误。
	Runner execx.Runner
	// Report 接收进度消息。
	Report output.Reporter
	// DryRun 只报告计划动作，不执行任何命令。
	DryRun bool
	// Timeout 限制单次 systemctl 动作；0 表示不额外限时。
	Timeout time.Duration
}

func (u UnitController) reporter() output.Reporter {
	if u.Report == nil {
		return output.Discard()
	}
	return u.Report
}

func (u UnitController) run(ctx context.Context, args ...string) ([]byte, error) {
	if u.Runner == nil {
		return nil, fmt.Errorf("systemd 不可用：没有配置命令执行器")
	}
	if u.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, u.Timeout)
		defer cancel()
	}
	return u.Runner.Run(ctx, "systemctl", args...)
}

// DaemonReload 重新加载 unit 文件，让刚写入的 unit 生效。
func (u UnitController) DaemonReload(ctx context.Context) error {
	if u.Runner == nil {
		return fmt.Errorf("systemd 不可用：没有配置命令执行器")
	}
	return DaemonReload(ctx, u.Runner)
}

// IsActive 报告 unit 是否处于 active。
// `systemctl is-active` 对未运行的 unit 也返回非零退出码，因此先解析输出，
// 只有连状态都拿不到时才认为是真正的错误。
func (u UnitController) IsActive(ctx context.Context, unit string) (bool, error) {
	if u.Runner == nil {
		return false, fmt.Errorf("systemd 不可用：没有配置命令执行器")
	}
	return IsActive(ctx, u.Runner, unit)
}

// IsEnabled 报告 unit 是否开机自启。
func (u UnitController) IsEnabled(ctx context.Context, unit string) (bool, error) {
	if u.Runner == nil {
		return false, fmt.Errorf("systemd 不可用：没有配置命令执行器")
	}
	return IsEnabled(ctx, u.Runner, unit)
}

// Stop 停止 unit（幂等）：原本未运行时只记录，不执行 systemctl stop。
func (u UnitController) Stop(ctx context.Context, unit string) error {
	if u.DryRun {
		u.reporter().Infof("将停止 %s（dry-run）", unit)
		return nil
	}
	active, err := u.IsActive(ctx, unit)
	switch {
	case err != nil:
		u.reporter().Warnf("查询 %s 状态失败（%v），直接尝试停止", unit, err)
	case !active:
		u.reporter().Infof("%s 原本未运行，跳过停止", unit)
		return nil
	}
	if _, err := u.run(ctx, "stop", unit); err != nil {
		return fmt.Errorf("停止 %s 失败: %w。请执行 systemctl status %s 查看原因", unit, err, unit)
	}
	u.reporter().Infof("已停止 %s", unit)
	return nil
}

// Disable 取消开机自启。
func (u UnitController) Disable(ctx context.Context, unit string) error {
	if u.DryRun {
		u.reporter().Infof("将禁用 %s（dry-run）", unit)
		return nil
	}
	if _, err := u.run(ctx, "disable", unit); err != nil {
		// 节点上根本没有这个 unit（例如 worker 没有 etcd）
		// 不是失败：reset 必须继续清理其余组件。
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not found") {
			u.reporter().Infof("%s 不存在，跳过禁用", unit)
			return nil
		}
		return fmt.Errorf("禁用 %s 失败: %w", unit, err)
	}
	u.reporter().Infof("已禁用 %s", unit)
	return nil
}

// Enable 让 unit 开机自启：先 daemon-reload 再 enable。
func (u UnitController) Enable(ctx context.Context, unit string) error {
	if u.DryRun {
		u.reporter().Infof("将启用 %s（dry-run）", unit)
		return nil
	}
	if err := u.DaemonReload(ctx); err != nil {
		return err
	}
	if _, err := u.run(ctx, "enable", unit); err != nil {
		return fmt.Errorf("启用 %s 失败: %w。请确认 unit 文件已写入 /etc/systemd/system，再执行 systemctl enable %s 查看原因", unit, err, unit)
	}
	u.reporter().Infof("已启用 %s（开机自启）", unit)
	return nil
}

// Restart 启动 unit，已在运行时则重启。
func (u UnitController) Restart(ctx context.Context, unit string) error {
	if u.DryRun {
		u.reporter().Infof("将重启 %s（dry-run）", unit)
		return nil
	}
	if _, err := u.run(ctx, "restart", unit); err != nil {
		return fmt.Errorf("重启 %s 失败: %w。请执行 systemctl status %s 与 journalctl -u %s 查看原因", unit, err, unit, unit)
	}
	u.reporter().Infof("已重启 %s", unit)
	return nil
}
