package cert

import (
	"fmt"
	"io"
	"strings"

	"kubemono/internal/certrenew"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/identity"
	"kubemono/internal/kubelet"
	"kubemono/internal/pki"

	"github.com/spf13/cobra"
)

// certHost 解析本机主机条目。
// 只解析身份，不构建完整 phase 上下文：后者还会计算生效 BOM（带版本覆盖
// 告警）与探测主机 MTU，续期路径用不到这些副作用。
func certHost(opts *options.GlobalOptions, cluster *clusterconfig.Cluster) (*clusterconfig.Host, error) {
	resolver := &identity.Resolver{NodeConfigPath: opts.Paths().NodeConfig()}
	res, err := resolver.Resolve(cluster, "", false)
	if err != nil {
		return nil, err
	}
	return res.Host, nil
}

// certRenewOptions 构造 certrenew 选项与组件重启器。
func certRenewOptions(opts *options.GlobalOptions, certIDs []string, all bool) (certrenew.Options, error) {
	out := certrenew.Options{
		Paths:  opts.Paths(),
		Force:  all,
		DryRun: opts.DryRun,
		Report: opts.Reporter,
	}
	for _, name := range certIDs {
		leaf, ok := pki.DefaultCatalogue.FindLeaf(name)
		if !ok {
			return out, fmt.Errorf("未知证书 %q；可用 --cert 指定 catalogue ID 或证书文件名", name)
		}
		out.CertIDs = append(out.CertIDs, string(leaf.ID))
	}
	cluster, err := opts.LoadCluster()
	if err != nil {
		// 没有 cluster.yaml 也要能续期（有效期用默认值）：这是应急路径。
		opts.Reporter.Warnf("%v；将使用默认证书有效期 %s", err, constants.DefaultCertTTL)
	} else {
		out.Cluster = cluster
		if host, hostErr := certHost(opts, cluster); hostErr == nil {
			if host != nil && !host.IsMaster() {
				return out, fmt.Errorf("cert renew 只能在 master 上执行（需要 CA 私钥）；worker 上的 kube-router 等组件使用 ServiceAccount token，不需要证书")
			}
			out.Host = host
		}
	}
	// 没有 cluster.yaml 的应急路径也要能续期：控制面走 systemctl，代理走静态 Pod 注解。
	out.Restarter = newComponentRestarter(opts)
	return out, nil
}

// newComponentRestarter 构造重启器。
func newComponentRestarter(opts *options.GlobalOptions) certrenew.Restarter {
	return &componentRestarter{
		paths:     opts.Paths(),
		systemctl: kubelet.NewExecRunner(),
		report:    opts.ReporterOrDiscard(),
	}
}

// renderRenewReport 渲染本机续期报告。
func renderRenewReport(cmd *cobra.Command, opts *options.GlobalOptions, report *certrenew.Report) error {
	out := cmd.OutOrStdout()
	return utils.Print(out, opts.Output, report, func(w io.Writer) error {
		_, _ = fmt.Fprint(w, term.KeyValues(map[string]string{
			"node":      report.Node,
			"renewed":   fmt.Sprintf("%d", report.Renewed),
			"skipped":   fmt.Sprintf("%d", report.Skipped),
			"failed":    fmt.Sprintf("%d", report.Failed),
			"dry-run":   fmt.Sprintf("%t", report.DryRun),
			"restarted": strings.Join(report.Restarted, ","),
		}))
		rows := make([][]string, 0, len(report.Results))
		for _, r := range report.Results {
			state := "skipped"
			switch {
			case r.Error != "":
				state = "failed"
			case r.Renewed:
				state = "renewed"
			}
			rows = append(rows, []string{
				r.Name,
				r.CommonName,
				r.OldNotAfter.UTC().Format("2006-01-02"),
				r.NewNotAfter.UTC().Format("2006-01-02"),
				state,
				r.Skipped + r.Error,
			})
		}
		_, _ = fmt.Fprint(w, term.Table([]string{"NAME", "CN", "OLD EXPIRY", "NEW EXPIRY", "STATE", "NOTE"}, rows))
		for _, note := range report.Notes {
			_, _ = fmt.Fprintf(w, "  说明：%s\n", note)
		}
		return nil
	})
}

// renewLong 是 `kubemono cert renew` 的帮助说明；可用证书 ID 见 --cert 的 flag 说明。
const renewLong = `重新签发本机叶子证书并滚动重启相应组件。

必须在持有 CA 私钥的 master 上执行；CA 与 ServiceAccount 密钥不变。
默认只重签 30 天内到期或已过期的证书；--all 重签全部证书。
-y/--assume-yes 只跳过交互确认，不改变续期范围。

本命令保留原证书的 SAN；修改 controlPlane.certSANs / externalEndpoints 后
请执行 kubemono node repair（master 的 certs phase 会按新配置重签并重启）。`

func newCertRenewCmd(opts *options.GlobalOptions) *cobra.Command {
	var (
		certIDs []string
		all     bool
	)
	cmd := &cobra.Command{
		Use:   "renew",
		Short: "续期本机证书并重启相应组件",
		Long:  renewLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			renewOpts, err := certRenewOptions(opts, certIDs, all)
			if err != nil {
				return err
			}
			if err := utils.ConfirmOrCancel(cmd.InOrStdin(), cmd.OutOrStdout(),
				opts.AssumeYes, opts.DryRun, "确定续期证书并重启组件？"); err != nil {
				return err
			}
			lk, err := opts.LockForWrite()
			if err != nil {
				return err
			}
			defer func() { _ = lk.Close() }()
			report, err := certrenew.Local(cmd.Context(), renewOpts)
			if report != nil {
				if renderErr := renderRenewReport(cmd, opts, report); renderErr != nil {
					return renderErr
				}
			}
			return err
		},
	}
	cmd.Flags().StringSliceVar(&certIDs, "cert", nil, "只续期指定证书（可重复/逗号分隔，如 apiserver,etcd-server）")
	cmd.Flags().BoolVar(&all, "all", false, "重签全部证书（默认只重签 30 天内到期或已过期的）")
	return options.Add(cmd, opts,
		options.ConfigFlag|options.OutputFlag|options.AssumeYesFlag|options.DryRunFlag|options.DataDirFlag)
}
