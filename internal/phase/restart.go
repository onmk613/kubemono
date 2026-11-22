package phase

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kubemono/internal/util/fsutil"
)

// RestartMarker 记录“受管文件已经写好，但组件还没有成功重启”。
//
// 为什么需要它：无守护进程模型下，Check 是唯一的收敛判据。如果进程在
// “写文件”与“重启组件”之间被杀，下一次 Check 只看到文件正确、组件健康，
// 就会认为已完成——组件会永远带着旧配置运行。certs phase 的
// cert-restart.json 是同一模式的第一次实现；这里把它抽成通用标记。
type RestartMarker struct {
	// Phase 是产生标记的 phase 名（排障用）。
	Phase string `json:"phase"`
	// Components 是等待重启的组件名。
	Components []string `json:"components"`
	// Reason 是给操作者看的一句话。
	Reason string `json:"reason,omitempty"`
	// Since 是首次记录时间。
	Since time.Time `json:"since"`
}

// restartMarkerName 白名单化标记名，避免调用方拼出路径穿越。
func restartMarkerName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("phase: restart marker name must not be empty")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			continue
		default:
			return "", fmt.Errorf("phase: invalid restart marker name %q", name)
		}
	}
	return name, nil
}

// RestartMarkerPath 返回某个 phase 的待重启标记路径。
func RestartMarkerPath(c *Context, name string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("phase: nil context")
	}
	safe, err := restartMarkerName(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.Paths.State(), "restart-"+safe+".json"), nil
}

// WriteRestartMarker 原子写入待重启标记。已有标记时保留最早的 Since，
// 这样排障时能看到“从什么时候开始没收敛”。
func WriteRestartMarker(c *Context, name string, components []string, reason string) error {
	path, err := RestartMarkerPath(c, name)
	if err != nil {
		return err
	}
	if len(components) == 0 {
		return nil
	}
	since := c.Clock()
	if data, err := os.ReadFile(path); err == nil {
		var old RestartMarker
		if json.Unmarshal(data, &old) == nil && !old.Since.IsZero() {
			since = old.Since
		}
	}
	marker := RestartMarker{Phase: name, Components: components, Reason: reason, Since: since}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建待重启标记目录失败: %w", err)
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// PendingRestart 读取待重启标记。
// 返回 (nil, false, nil) 表示没有标记；标记损坏时返回错误，调用方应把它
// 当作“未收敛”而不是“已完成”——损坏的标记不能静默放过。
func PendingRestart(c *Context, name string) ([]string, bool, error) {
	path, err := RestartMarkerPath(c, name)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读取待重启标记 %s 失败: %w", path, err)
	}
	var marker RestartMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, false, fmt.Errorf("解析待重启标记 %s 失败: %w", path, err)
	}
	if len(marker.Components) == 0 {
		return nil, false, nil
	}
	return marker.Components, true, nil
}

// HasPendingRestart 是 Check/Run 用的便捷封装：标记存在即返回 true；标记损坏
// 也返回 true（保守地要求 Run 再执行一次，Run 会用 WriteRestartMarker 覆盖它）。
// 这里不返回错误：一个损坏的排障标记不应该让 node repair 永久无法执行。
func HasPendingRestart(c *Context, name string) (bool, error) {
	_, pending, err := PendingRestart(c, name)
	if err != nil {
		return true, nil
	}
	return pending, nil
}

// ClearRestartMarker 删除标记；不存在时是 no-op。
func ClearRestartMarker(c *Context, name string) error {
	path, err := RestartMarkerPath(c, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除待重启标记 %s 失败: %w", path, err)
	}
	return nil
}
