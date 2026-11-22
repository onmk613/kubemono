package hostinit

import (
	"io/fs"
)

// 以下是每种 Step 的 typed payload。Step.Payload 是 these 之一，而不是
// map[string]any；取值时一次类型断言即可，缺字段/错类型在编译期或构造期
// 就能发现。

// SysctlPayload 对应 KindSysctl。
type SysctlPayload struct {
	Key   string
	Value string
}

// ModulePayload 对应 KindModule。
type ModulePayload struct {
	Name string
}

// FilePayload 对应 KindFile：写入整文件内容（空内容表示清空受管文件）。
type FilePayload struct {
	Path    string
	Mode    fs.FileMode
	Content string
}

// DirPayload 对应 KindDir。
type DirPayload struct {
	Path string
	Mode fs.FileMode
}

// HookPayload 对应 KindHook。
type HookPayload struct {
	Command string
}

// fileModeOr 返回文件/目录步骤的权限位（零值用 def）。
func fileModeOr(mode, def fs.FileMode) fs.FileMode {
	if mode == 0 {
		return def
	}
	return mode
}
