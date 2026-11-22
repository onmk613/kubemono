// Package cert 实现 `kubemono cert` 命令组：到期检查、续期、CSR 审批与 CA 备份。
// 证书操作会影响控制面可用性，因此每个破坏性动作都要求确认，并在错误信息里
// 给出可执行的下一步。
package cert

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
	"kubemono/internal/constants"
	"kubemono/internal/controlplane"
	"kubemono/internal/kubelet"
	"kubemono/internal/util/output"

	"github.com/spf13/cobra"
)

// Restart 实现 certrenew.Restarter。
// 控制面组件用 systemctl 重启；local-proxy 仍由 kubelet 以静态 Pod 拉起，
// 通过更新 restarted-at 注解让 kubelet 重建。
func (r *componentRestarter) Restart(ctx context.Context, components []string) error {
	// daemon-reload 只在确有 systemd 组件要重启时执行一次：unit 文件在续期
	// 流程里不会中途变化，逐组件重复 reload 纯属浪费。全静态 Pod 的组件
	// 列表（如只有 local-proxy）不触发，保持原有语义。
	firstSystemd := ""
	for _, component := range components {
		if !isStaticPodComponent(component) {
			firstSystemd = component
			break
		}
	}
	if firstSystemd != "" {
		if r.systemctl == nil {
			return fmt.Errorf("需要 systemctl 才能重启 %s", firstSystemd)
		}
		if _, err := r.systemctl.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("systemctl daemon-reload 失败: %w", err)
		}
	}
	for _, component := range components {
		if isStaticPodComponent(component) {
			if err := kubelet.BumpStaticPodRestart(r.paths, component, time.Now()); err != nil {
				return fmt.Errorf("重启 %s 失败: %w", component, err)
			}
			continue
		}
		if _, err := r.systemctl.Run(ctx, "systemctl", "restart", component+".service"); err != nil {
			return fmt.Errorf("重启 %s 失败: %w", component, err)
		}
		// systemctl restart 对 Type=simple 的 unit 在进程 fork 后就返回；
		// apiserver 从启动到 /readyz 可用还有一段时间，续期命令必须等它
		// 真正健康再返回，否则紧跟其后的 kubectl 会遇到 connection refused。
		if err := waitComponentReady(ctx, component, controlplane.NewHTTPChecker(), 90*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// waitComponentReady 轮询组件健康端点，直到就绪或超时。
func waitComponentReady(ctx context.Context, component string, checker controlplane.HealthChecker, timeout time.Duration) error {
	h := controlplane.ComponentHealth(controlplane.Component(component))
	if h.Address == "" || checker == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if lastErr = checker.Check(ctx, h); lastErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s 重启后在 %s 内未就绪（%s）：%w", component, timeout, h.URL(), lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// isStaticPodComponent 报告组件是否仍由 kubelet 以静态 Pod 运行。
func isStaticPodComponent(component string) bool {
	return component == constants.ComponentLocalProxy
}

// componentRestarter 通过 systemctl 或静态 Pod 注解重启组件。
type componentRestarter struct {
	paths     constants.Paths
	systemctl controlplane.CommandRunner
	report    output.Reporter
}

// parseDurationFlag 解析时间段参数，支持 "Nd" 天数后缀；
// 参数为空时返回 def。
func parseDurationFlag(value string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return def, nil
	}
	if before, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(before)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("天数必须是正整数")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("必须是正的时间段")
	}
	return d, nil
}

func New(opts *options.GlobalOptions) *cobra.Command {
	return utils.Group("cert", "证书到期检查、续期与内容查看",
		"kubemono cert check-expiration",
		newCertCheckExpirationCmd(opts),
		newCertRenewCmd(opts),
		newCertShowCmd(opts),
		newCertTimerCmd(opts),
	)
}
