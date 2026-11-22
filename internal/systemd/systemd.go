// Package systemd 提供 systemctl 的最小共用原语。
// 各业务包仍然保留自己的窄接口以便注入 fake，但“执行 systemctl”和
// “解析 is-active”这两件事只在这里实现，避免 6 份状态集合互相漂移。
package systemd

import (
	"context"
	"fmt"
	"strings"

	"kubemono/internal/execx"
)

// Runner 执行 systemctl；与 execx.Runner 相同，便于直接复用。
type Runner = execx.Runner

// Action 对 unit 执行 start|stop|restart|enable|disable 等动作。
func Action(ctx context.Context, r Runner, unit, action string) error {
	if unit == "" {
		return fmt.Errorf("unit 名为空")
	}
	out, err := r.Run(ctx, "systemctl", action, unit)
	if err != nil {
		return fmt.Errorf("systemctl %s %s 失败: %w（输出：%s）", action, unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DaemonReload 重新读取 unit 文件。
func DaemonReload(ctx context.Context, r Runner) error {
	out, err := r.Run(ctx, "systemctl", "daemon-reload")
	if err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败: %w（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsActive 报告 unit 是否处于 active。
func IsActive(ctx context.Context, r Runner, unit string) (bool, error) {
	out, err := r.Run(ctx, "systemctl", "is-active", unit)
	return ParseActive(out, err)
}

// IsEnabled 报告 unit 是否已设置开机自启。
// `systemctl is-enabled` 对未 enable 的 unit 返回非零退出码，这不是错误：
// 只有输出无法识别时才当作真正的错误。
func IsEnabled(ctx context.Context, r Runner, unit string) (bool, error) {
	out, err := r.Run(ctx, "systemctl", "is-enabled", unit)
	return ParseEnabled(out, err)
}

// ParseEnabled 解析 systemctl is-enabled 的输出。
// enabled/enabled-runtime/static/indirect/alias 都表示“重启后会被拉起”：
// static 的 unit 没有 [Install] 段，但由别的 unit 依赖；kubemono 托管的
// unit 都显式 enable，因此这里宁可把 static 视为已启用，避免误报漂移。
func ParseEnabled(out []byte, err error) (bool, error) {
	switch strings.TrimSpace(string(out)) {
	case "enabled", "enabled-runtime", "static", "indirect", "alias":
		return true, nil
	case "disabled", "masked", "masked-runtime", "linked", "linked-runtime", "bad":
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询 unit 自启状态失败: %w", err)
	}
	return false, nil
}

// State 返回 systemctl is-active 的规范化状态；无法识别时返回 "unknown"。
// 与 ParseActive 共用同一份状态集合，避免调用方各自维护一套（hostinit 需要
// 记录原始状态，而不是布尔值）。
func State(out []byte) string {
	state := strings.TrimSpace(string(out))
	switch state {
	case "active", "reloading", "activating",
		"inactive", "failed", "deactivating", "maintenance":
		return state
	}
	return "unknown"
}

// ParseActive 解析 systemctl is-active 的输出。
// 对未运行的 unit，systemctl 返回非零退出码，这不是错误；只有拿不到可识别
// 状态时才当作真正的错误。active/reloading 视为运行中。
func ParseActive(out []byte, err error) (bool, error) {
	switch state := State(out); state {
	case "active", "reloading":
		return true, nil
	case "inactive", "failed", "activating", "deactivating", "maintenance":
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询 unit 状态失败: %w", err)
	}
	return false, nil
}

// Show 读取 systemctl show 的属性表（例如 Result / ExecMainStatus /
// NextElapseUSecRealtime）。属性不存在时不会出现在结果里。
func Show(ctx context.Context, r Runner, unit string, props ...string) (map[string]string, error) {
	args := []string{"show", unit, "--no-page"}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	out, err := r.Run(ctx, "systemctl", args...)
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s 失败: %w（输出：%s）", unit, err, strings.TrimSpace(string(out)))
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" {
			continue
		}
		values[key] = value
	}
	return values, nil
}
