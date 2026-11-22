package cert

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/identity"
	"kubemono/internal/systemd"
	"kubemono/internal/util/fsutil"
)

const (
	certTimerService = "kubemono-cert-renew.service"
	certTimerUnit    = "kubemono-cert-renew.timer"
)

const timerLong = `管理可选的证书自动续期 timer（systemd timer，不是常驻守护进程）。

timer 每天触发一次 ` + "`kubemono cert renew`" + `：只续期进入窗口（默认 30 天）
或已过期的本机叶子证书，并重启受影响组件。续期失败会保留待重启标记，
下一次 timer 或人工 ` + "`kubemono cert renew`" + ` 会重试。

只允许在已安装的 master 上安装（worker 没有 CA 私钥）。
`

func newCertTimerCmd(opts *options.GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "timer",
		Short: "安装/移除证书自动续期 systemd timer",
		Long:  timerLong,
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newCertTimerInstallCmd(opts), newCertTimerRemoveCmd(opts))
	return cmd
}

func newCertTimerInstallCmd(opts *options.GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "安装并启动 kubemono-cert-renew.timer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCertTimerInstall(cmd.Context(), opts)
		},
	}
	return options.Add(cmd, opts, options.ConfigFlag|options.DryRunFlag|options.DataDirFlag|options.OutputFlag)
}

func newCertTimerRemoveCmd(opts *options.GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "停止并删除 kubemono-cert-renew.timer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCertTimerRemove(cmd.Context(), opts)
		},
	}
	return options.Add(cmd, opts, options.DryRunFlag|options.DataDirFlag|options.OutputFlag)
}

func runCertTimerInstall(ctx context.Context, opts *options.GlobalOptions) error {
	node, err := (&identity.Store{Dir: opts.Paths().Kubemono()}).Load()
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("本机尚未安装：先执行 kubemono init/join")
	}
	master := false
	for _, role := range node.Roles {
		if role == "master" {
			master = true
			break
		}
	}
	if !master {
		return fmt.Errorf("证书 timer 只能安装在 master 上（worker 没有 CA 私钥）")
	}
	cfgPath, exists := opts.ClusterConfigPath()
	if !exists {
		return fmt.Errorf("本机没有 cluster.yaml（%s）：先完成安装或显式给 -f", cfgPath)
	}
	// systemd 的工作目录是 /，显式传相对 -f 时必须先转绝对路径。
	cfgPath, err = filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("解析 kubemono 可执行路径失败: %w", err)
	}
	service, timer := certTimerUnits(exe, cfgPath)
	servicePath := opts.Paths().Resolve("/etc/systemd/system/" + certTimerService)
	timerPath := opts.Paths().Resolve("/etc/systemd/system/" + certTimerUnit)

	if opts.DryRun {
		opts.ReporterOrDiscard().Infof("将写入 %s：\n%s", servicePath, service)
		opts.ReporterOrDiscard().Infof("将写入 %s：\n%s", timerPath, timer)
		opts.ReporterOrDiscard().Infof("将执行 systemctl daemon-reload && systemctl enable --now %s", certTimerUnit)
		return nil
	}
	if err := fsutil.WriteFileAtomic(servicePath, []byte(service), 0o644); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(timerPath, []byte(timer), 0o644); err != nil {
		return err
	}
	mgr := systemd.Manager{Timeout: 30 * time.Second}
	if err := mgr.DaemonReload(ctx); err != nil {
		return err
	}
	// 旧版本安装的 service 可能没有 -y（会一直取消续期）；重置 failed 状态，
	// 让 status --check 不把历史失败当成当前状态。
	_ = mgr.Action(ctx, certTimerService, "reset-failed")
	if err := mgr.Action(ctx, certTimerUnit, "enable"); err != nil {
		return err
	}
	if err := mgr.Action(ctx, certTimerUnit, "start"); err != nil {
		return err
	}
	opts.ReporterOrDiscard().Infof("已安装 %s：每天 03:17（±30m）执行 kubemono cert renew", certTimerUnit)
	return nil
}

func runCertTimerRemove(ctx context.Context, opts *options.GlobalOptions) error {
	servicePath := opts.Paths().Resolve("/etc/systemd/system/" + certTimerService)
	timerPath := opts.Paths().Resolve("/etc/systemd/system/" + certTimerUnit)
	if opts.DryRun {
		opts.ReporterOrDiscard().Infof("将停止/禁用 %s 并删除 %s、%s", certTimerUnit, timerPath, servicePath)
		return nil
	}
	mgr := systemd.Manager{Timeout: 30 * time.Second}
	// 未安装时 systemctl 会失败；清理命令应幂等。
	if err := mgr.Action(ctx, certTimerUnit, "disable"); err != nil {
		opts.ReporterOrDiscard().Debugf("停用 %s：%v", certTimerUnit, err)
	}
	_ = mgr.Action(ctx, certTimerUnit, "stop")
	if err := mgr.DaemonReload(ctx); err != nil {
		return err
	}
	for _, path := range []string{timerPath, servicePath} {
		if err := fsutil.RemoveIfExists(path); err != nil {
			return err
		}
	}
	if err := mgr.DaemonReload(ctx); err != nil {
		return err
	}
	opts.ReporterOrDiscard().Infof("已移除 %s", certTimerUnit)
	return nil
}

// certTimerUnits 渲染 service/timer 文本。ExecStart 的路径用 %q 引号，
// 允许安装目录/配置路径包含空格。
func certTimerUnits(exe, configPath string) (service, timer string) {
	service = fmt.Sprintf(`[Unit]
Description=kubemono certificate renewal
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=%s cert renew -y -f %s
Nice=10
PrivateTmp=true
`, strconv.Quote(exe), strconv.Quote(configPath))
	timer = fmt.Sprintf(`[Unit]
Description=Daily kubemono certificate renewal

[Timer]
OnCalendar=*-*-* 03:17:00
RandomizedDelaySec=30m
AccuracySec=1h
Persistent=true
Unit=%s

[Install]
WantedBy=timers.target
`, certTimerService)
	return service, timer
}
