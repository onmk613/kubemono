package controlplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kubemono/internal/systemd"
	"kubemono/internal/util/fsutil"
)

// ServiceManager 驱动 binary 模式控制面组件的 systemd；
// 与 runtime/etcd 共用 internal/systemd.Actioner。
type ServiceManager = systemd.Actioner

// BinaryRunner 把控制面组件作为 systemd unit 应用。
type BinaryRunner struct {
	opts    Options
	Service ServiceManager
}

// NewBinaryRunner 返回 systemd runner。
func NewBinaryRunner(opts Options, service ServiceManager) *BinaryRunner {
	opts.Defaults()
	return &BinaryRunner{opts: opts, Service: service}
}

// unitPath 返回组件的 systemd unit 路径。
func (r *BinaryRunner) unitPath(component Component) string {
	return r.opts.Paths.Resolve("/etc/systemd/system/" + string(component) + ".service")
}

// Apply 实现 Runner。
func (r *BinaryRunner) Apply(ctx context.Context, spec *RenderedSpec) error {
	if spec == nil {
		return fmt.Errorf("binary: 渲染结果为空")
	}
	if len(spec.Unit) == 0 {
		return fmt.Errorf("binary: %s 没有 unit 内容（binary 模式必须渲染 unit）", spec.Component)
	}
	target := r.unitPath(spec.Component)
	previous, hadPrevious := readFileIfExists(target)

	if _, err := fsutil.WriteFileIfChanged(target, spec.Unit, 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", target, err)
	}
	r.opts.Report("已写入 %s", target)
	if err := r.reload(ctx); err != nil {
		return err
	}
	if err := r.Service.Action(ctx, unitName(spec.Component), "enable"); err != nil {
		return err
	}
	if err := r.Service.Action(ctx, unitName(spec.Component), "restart"); err != nil {
		return r.rollbackUnit(ctx, target, previous, hadPrevious, fmt.Errorf("启动 %s 失败: %w", spec.Component, err))
	}
	if err := waitHealthy(ctx, r.opts, spec.Component, specHealthOr(spec)); err != nil {
		return r.rollbackUnit(ctx, target, previous, hadPrevious, err)
	}
	return nil
}

// rollbackUnit 恢复上一个 unit（或删除全新的 unit）并重启组件，
// 这样重启失败不会留下一个坏掉的 unit。
func (r *BinaryRunner) rollbackUnit(ctx context.Context, target string, previous []byte, hadPrevious bool, cause error) error {
	if hadPrevious {
		if _, err := fsutil.WriteFileIfChanged(target, previous, 0o644); err != nil {
			return fmt.Errorf("%w；回滚 unit 也失败: %v", cause, err)
		}
	} else if err := fsutil.RemoveIfExists(target); err != nil {
		return fmt.Errorf("%w；清理新 unit 也失败: %v", cause, err)
	}
	if err := r.reload(ctx); err != nil {
		return fmt.Errorf("%w；daemon-reload 失败: %v", cause, err)
	}
	if hadPrevious {
		if err := r.Service.Action(ctx, unitName(componentOf(target)), "restart"); err != nil {
			return fmt.Errorf("%w；回滚后重启失败: %v", cause, err)
		}
	}
	return fmt.Errorf("%w（已回滚 unit）", cause)
}

// reload 执行 daemon-reload；缺少 systemd 时会给出明确报错。
func (r *BinaryRunner) reload(ctx context.Context) error {
	if r.Service == nil {
		return fmt.Errorf("binary: 未提供 ServiceManager（binary 模式需要 systemd）")
	}
	if err := r.Service.DaemonReload(ctx); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败: %w", err)
	}
	return nil
}

// Restart 实现 Runner。
func (r *BinaryRunner) Restart(ctx context.Context, component Component) error {
	if err := r.Service.Action(ctx, unitName(component), "restart"); err != nil {
		return err
	}
	r.opts.Report("已重启 %s", component)
	return waitHealthy(ctx, r.opts, component, defaultHealth(component))
}

// Stop 实现 Runner。
func (r *BinaryRunner) Stop(ctx context.Context, component Component) error {
	if err := r.Service.Action(ctx, unitName(component), "stop"); err != nil {
		return err
	}
	r.opts.Report("已停止 %s", component)
	return nil
}

// Remove 实现 Runner。
func (r *BinaryRunner) Remove(ctx context.Context, component Component) error {
	// 先停再禁用，避免 unit 被删掉后 systemd 还在尝试拉起。
	_ = r.Service.Action(ctx, unitName(component), "stop")
	_ = r.Service.Action(ctx, unitName(component), "disable")
	target := r.unitPath(component)
	if err := fsutil.RemoveIfExists(target); err != nil {
		return err
	}
	if err := r.reload(ctx); err != nil {
		return err
	}
	r.opts.Report("已移除 %s", component)
	return nil
}

// IsEnabled 报告组件 unit 是否开机自启。
// Service 不是 systemd.Manager（例如测试 fake）时返回 true：不把“无法查询”
// 当成“未启用”，否则测试与自定义实现会被误判为漂移。
func (r *BinaryRunner) IsEnabled(ctx context.Context, component Component) (bool, error) {
	if ec, ok := r.Service.(systemd.EnabledChecker); ok {
		return ec.IsEnabled(ctx, unitName(component))
	}
	return true, nil
}

// Health 实现 Runner。
func (r *BinaryRunner) Health(ctx context.Context, component Component) error {
	if r.opts.Health == nil {
		return nil
	}
	return r.opts.Health.Check(ctx, defaultHealth(component))
}

// UnitPath 返回组件的 unit 所在路径。
func (r *BinaryRunner) UnitPath(component Component) string { return r.unitPath(component) }

func unitName(component Component) string { return string(component) + ".service" }

// readFileIfExists 读取文件；不存在时返回 false，而不是错误。
func readFileIfExists(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

// componentOf 从 unit 路径还原组件名。
func componentOf(unitPath string) Component {
	base := filepath.Base(unitPath)
	return Component(strings.TrimSuffix(base, ".service"))
}

// NewRunner 返回 systemd runner。
func NewRunner(opts Options, service ServiceManager) (Runner, error) {
	if service == nil {
		return nil, fmt.Errorf("binary: 需要 ServiceManager（systemd）")
	}
	return NewBinaryRunner(opts, service), nil
}
