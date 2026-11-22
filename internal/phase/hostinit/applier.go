package hostinit

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/execx"
	"kubemono/internal/util/fsutil"
)

// Applier 是真实的主机应用器：文件、sysctl、内核模块、目录与命令。
// 它只做"动作"，不判断"该不该做"——那是 Plan 和 phase 的职责。
// 所有命令都通过 execx.Runner，便于在测试里替换为内存实现。
type Applier struct {
	// Runner 运行 systemctl / modprobe / sysctl。
	Runner execx.Runner
	// Timeout 限制普通主机命令（modprobe/systemctl 等）的执行时间。
	Timeout time.Duration
	// HookTimeout 限制 hook 命令的执行时间；hook 可能包含软件安装等慢操作。
	HookTimeout time.Duration
	// Report 接收进度消息。
	Report func(format string, args ...any)
}

// NewApplier 返回生产环境使用的 applier。
func NewApplier() *Applier {
	return &Applier{
		Runner:      execx.Default(),
		Timeout:     60 * time.Second,
		HookTimeout: 10 * time.Minute,
	}
}

func (a *Applier) runner() execx.Runner {
	if a.Runner == nil {
		return execx.Default()
	}
	return a.Runner
}

func (a *Applier) report(format string, args ...any) {
	if a.Report != nil {
		a.Report(format, args...)
	}
}

func (a *Applier) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return a.runWithTimeout(ctx, a.Timeout, name, args...)
}

func (a *Applier) runWithTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return a.runner().Run(cctx, name, args...)
}

// WriteFile 实现 hostinit.Applier。
// 文件写入走 fsutil（无 ctx 变体）；统一接收 ctx 保证接口将来演进不用再改。
func (a *Applier) WriteFile(ctx context.Context, path string, content []byte, mode os.FileMode) error {
	_ = ctx
	if path == "" {
		return fmt.Errorf("hostinit: 写入路径为空")
	}
	changed, err := fsutil.WriteFileIfChanged(path, content, mode)
	if err != nil {
		return err
	}
	if changed {
		a.report("已写入 %s", path)
	}
	return nil
}

// RemoveFile 实现 hostinit.Applier。
func (a *Applier) RemoveFile(ctx context.Context, path string) error {
	_ = ctx
	if err := fsutil.RemoveIfExists(path); err != nil {
		return err
	}
	a.report("已删除 %s", path)
	return nil
}

// Sysctl 实现 hostinit.Applier。
func (a *Applier) Sysctl(ctx context.Context, key, value string) error {
	if strings.ContainsAny(value, "\n\r\x00") {
		return fmt.Errorf("hostinit: sysctl %s 的值不能包含换行或 NUL", key)
	}
	// 写入 /proc/sys 是原子的，避免 shell 解析带来的注入风险。
	path, err := sysctlPath(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		// 回退到 sysctl(8)：某些内核参数不支持直接写。
		if _, cmdErr := a.run(ctx, "sysctl", "-w", key+"="+value); cmdErr != nil {
			return fmt.Errorf("设置 %s=%s 失败: %w", key, value, cmdErr)
		}
	}
	a.report("已设置 sysctl %s=%s", key, value)
	return nil
}

// sysctlPath 把 sysctl 键映射到 /proc/sys 下的路径，并再次确认没有越界。
// 配置校验已经拒绝 ".."，这里是纵深防御：即使未来新增了绕过校验的调用方，
// 也不会把 root 可写路径拼到 /proc/sys 之外。
func sysctlPath(key string) (string, error) {
	if err := clusterconfig.ValidateSysctlKey(key); err != nil {
		return "", fmt.Errorf("hostinit: %w", err)
	}
	clean := path.Clean("/proc/sys/" + strings.ReplaceAll(key, ".", "/"))
	if !strings.HasPrefix(clean, "/proc/sys/") {
		return "", fmt.Errorf("hostinit: sysctl 键 %q 解析出的路径 %q 越界", key, clean)
	}
	return clean, nil
}

// ReadSysctl 实现 hostinit.SysctlReader。
func (a *Applier) ReadSysctl(ctx context.Context, key string) (string, error) {
	_ = ctx
	path, err := sysctlPath(key)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// LoadModule 实现 hostinit.Applier。
func (a *Applier) LoadModule(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("hostinit: 模块名为空")
	}
	if _, err := a.run(ctx, "modprobe", name); err != nil {
		return err
	}
	a.report("已加载内核模块 %s", name)
	return nil
}

// RunCommand 实现 hostinit.Applier。
// hook 使用独立的 HookTimeout，避免被普通主机命令的 60s 超时截断。
func (a *Applier) RunCommand(ctx context.Context, name string, args ...string) error {
	if name == "" {
		return fmt.Errorf("hostinit: 命令为空")
	}
	timeout := a.HookTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if _, err := a.runWithTimeout(ctx, timeout, name, args...); err != nil {
		return err
	}
	a.report("已执行 %s %s", name, strings.Join(args, " "))
	return nil
}

// MakeDir 实现 hostinit.DirMaker。
func (a *Applier) MakeDir(ctx context.Context, path string, mode os.FileMode) error {
	_ = ctx
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	a.report("已创建目录 %s", path)
	return nil
}

// ReadFile 实现 hostinit.FileReader。
func (a *Applier) ReadFile(ctx context.Context, path string) ([]byte, error) {
	_ = ctx
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Plan 用 hostinit.EntryAbsent 表示"不存在"。
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return data, nil
}
