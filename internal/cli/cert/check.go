package cert

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"kubemono/internal/certrenew"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	"kubemono/internal/constants"

	"github.com/spf13/cobra"
)

// certDuration 是 --warn-before 的 pflag 值：接受 Go duration（720h）或
// “天”后缀（30d）。
type certDuration struct{ d time.Duration }

func (v *certDuration) String() string {
	if v == nil || v.d == 0 {
		return ""
	}
	return v.d.String()
}

func (v *certDuration) Set(s string) error {
	d, err := parseDurationFlag(s, 0)
	if err != nil {
		return err
	}
	v.d = d
	return nil
}

func (v *certDuration) Type() string { return "duration" }

// renderCertStatuses 渲染证书有效期扫描结果。
// 结果已按证书身份聚合：USED BY 列说明这张证书被多少 kubeconfig 引用；
// kubeconfig 引用与期望不一致的条目单独成行，并在表格下方给出原因。
func renderCertStatuses(statuses []certrenew.CertStatus) string {
	if len(statuses) == 0 {
		return "没有找到任何证书（检查 <root>/etc/kubernetes/pki、etcd PKI 与 kubeconfig）\n"
	}
	rows := make([][]string, 0, len(statuses))
	for _, status := range statuses {
		state := "ok"
		switch {
		case status.Error != "":
			state = "error: " + status.Error
		case status.Mismatch != "":
			state = "mismatch"
		case status.Expired:
			state = "expired"
		case status.Warn:
			state = "warn"
		}
		rows = append(rows, []string{
			status.Source,
			status.Path,
			status.CommonName,
			status.NotAfter.UTC().Format("2006-01-02"),
			certrenew.FormatRemaining(status),
			state,
			usedBy(status.Kubeconfigs),
		})
	}
	out := term.Table([]string{"SOURCE", "PATH", "CN", "NOT AFTER", "REMAINING", "STATUS", "USED BY"}, rows)
	for _, status := range statuses {
		if status.Mismatch != "" {
			out += fmt.Sprintf("  不一致：%s：%s\n", status.Path, status.Mismatch)
		}
	}
	return out
}

// usedBy 把引用证书的 kubeconfig 压缩成表格里可读的一列。
func usedBy(kubeconfigs []string) string {
	switch len(kubeconfigs) {
	case 0:
		return "-"
	case 1:
		return filepath.Base(kubeconfigs[0])
	case 2:
		return filepath.Base(kubeconfigs[0]) + "," + filepath.Base(kubeconfigs[1])
	default:
		return fmt.Sprintf("%d 个 kubeconfig", len(kubeconfigs))
	}
}

// certStatusError 在证书过期、无法解析或与 kubeconfig 引用不一致时返回非 nil 错误。
func certStatusError(statuses []certrenew.CertStatus) error {
	var expired, broken, mismatch []string
	for _, status := range statuses {
		if status.Expired {
			expired = append(expired, status.Path)
		}
		if status.Error != "" {
			broken = append(broken, status.Path)
		}
		if status.Mismatch != "" {
			mismatch = append(mismatch, status.Path)
		}
	}
	switch {
	case len(expired) > 0:
		return fmt.Errorf("%d 张证书已过期：%s", len(expired), strings.Join(expired, "、"))
	case len(broken) > 0:
		return fmt.Errorf("%d 个证书无法解析：%s", len(broken), strings.Join(broken, "、"))
	case len(mismatch) > 0:
		return fmt.Errorf("%d 张证书与 kubeconfig 引用不一致：%s", len(mismatch), strings.Join(mismatch, "、"))
	}
	return nil
}

// checkLong 是 `kubemono cert check-expiration` 的帮助说明。
const checkLong = `只读扫描本机证书并列出剩余有效期。

结果按证书身份聚合：同一张 CA 被多个 kubeconfig 引用只显示一行，USED BY 列
给出引用数量。kubeconfig 引用的证书与 kubemono 期望不一致时（路径不对、内嵌证书
不同、集群 CA 不同）单独成行并标 mismatch，表格下方给出原因。

默认把 30 天内到期的证书标为 warn；已过期、无法解析或与 kubeconfig 不一致时
返回非零退出码，--fail-on-warn 让进入续期窗口也返回非零，便于 cron/监控告警。`

func newCertCheckExpirationCmd(opts *options.GlobalOptions) *cobra.Command {
	var (
		warnBefore certDuration
		failOnWarn bool
	)
	cmd := &cobra.Command{
		Use:   "check-expiration",
		Short: "检查所有组件证书的剩余有效期",
		Long:  checkLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			window := warnBefore.d
			if window == 0 {
				window = constants.DefaultCertWarnBefore
			}
			statuses, err := certrenew.CheckExpiration(cmd.Context(), certrenew.Options{
				Paths:      opts.Paths(),
				WarnBefore: window,
				Report:     opts.Reporter,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if err := utils.Print(out, opts.Output, statuses, func(w io.Writer) error {
				_, err := fmt.Fprint(w, renderCertStatuses(statuses))
				return err
			}); err != nil {
				return err
			}
			if failOnWarn {
				if expiring := certrenew.Expiring(statuses); len(expiring) > 0 {
					return fmt.Errorf("%d 张证书已进入续期窗口或已过期", len(expiring))
				}
			}
			return certStatusError(statuses)
		},
	}
	cmd.Flags().Var(&warnBefore, "warn-before", "续期告警窗口（如 720h 或 30d；默认 30 天）")
	cmd.Flags().BoolVar(&failOnWarn, "fail-on-warn", false, "进入告警窗口也返回非零退出码")
	return options.Add(cmd, opts, options.OutputFlag|options.DataDirFlag)
}
