package token

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/discovery"
	jointoken "kubemono/internal/join/token"
)

// createLong 是 `kubemono token create` 的帮助说明。
const createLong = `签发其他节点加入集群所需的短期凭证，并打印一条可直接执行的 join 命令。

默认签发 worker 凭证：只创建 bootstrap token，不碰 certificateKey 和证书包。
master 加入必须显式指定 --role master；它会同时轮换 certificateKey，
重新加密并上传 kube-system/kubemono-certs，已有的 master join 命令随即失效。

bootstrap token 默认 2 小时内有效（硬上限 2h），凭证不落盘。
该命令只能在已初始化、能读取 admin kubeconfig 的 master 上执行。`

func newCreate(opts *options.GlobalOptions) *cobra.Command {
	var (
		role string
		ttl  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "签发指定角色的 join 凭证并打印 join 命令",
		Long:  createLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !opts.DryRun && opts.Reporter != nil {
				// stdout 只留 join 命令；计划、告警都走 stderr。
				opts.Reporter.SetProgressToStderr(true)
			}
			joinRole, err := parseRole(role)
			if err != nil {
				return err
			}
			tokenTTL, err := normalizeTTL(ttl)
			if err != nil {
				return err
			}
			cluster, err := opts.LoadCluster()
			if err != nil {
				return err
			}
			if opts.DryRun {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"将以 %s 身份签发 join 凭证（TTL %s），并打印一条 join 命令（dry-run 未执行）\n",
					joinRole, tokenTTL)
				if joinRole == clusterconfig.RoleMaster {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "master 凭证还会轮换 certificateKey 并刷新 kube-system/kubemono-certs")
				}
				return nil
			}

			client, err := utils.NewClusterClient(opts)
			if err != nil {
				return fmt.Errorf("token create 只用于已初始化的集群（需要 master 上的 admin kubeconfig）: %w", err)
			}
			material, err := jointoken.Issue(cmd.Context(), jointoken.Request{
				Cluster: cluster,
				Paths:   opts.Paths(),
				Client:  client,
				Role:    joinRole,
				TTL:     tokenTTL,
			})
			if err != nil {
				return err
			}
			endpoint := firstJoinEndpoint(cluster)
			if endpoint == "" {
				return fmt.Errorf("cluster.yaml 里没有可用的 master 地址，无法生成 join 命令")
			}

			label := "Worker"
			if material.Role == clusterconfig.RoleMaster {
				label = "Master"
			}
			configPath, _ := opts.ClusterConfigPath()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s join command (token valid until %s):\n  %s\n",
				label,
				material.ExpiresAt.UTC().Format(time.RFC3339),
				formatJoinCommand(endpoint, material, configPath))

			reporter := opts.ReporterOrDiscard()
			suffix := ""
			if material.Role == clusterconfig.RoleMaster {
				suffix = " 与 certificateKey"
				reporter.Warnf("已轮换 certificateKey：之前签发的 master join 命令立即失效；worker 命令不受影响")
			}
			reporter.Warnf("join 命令含明文 bootstrap token%s；令牌 %s 前有效，复制时注意 shell history 与 CI 日志",
				suffix, material.ExpiresAt.UTC().Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", string(clusterconfig.RoleWorker), "签发角色：worker（默认）或 master")
	cmd.Flags().DurationVar(&ttl, "ttl", constants.DefaultBootstrapTokenTTL, "bootstrap token 有效期（硬上限 2h）")
	return options.Add(cmd, opts,
		options.ConfigFlag|options.KubeconfigFlag|options.DryRunFlag|options.DataDirFlag)
}

// parseRole 把 --role 转成角色；默认 worker，master 必须显式指定。
func parseRole(raw string) (clusterconfig.Role, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(clusterconfig.RoleWorker):
		return clusterconfig.RoleWorker, nil
	case string(clusterconfig.RoleMaster):
		return clusterconfig.RoleMaster, nil
	default:
		return "", fmt.Errorf("--role 只支持 worker 或 master（默认 worker）")
	}
}

// normalizeTTL 把 0 当成默认值，并拒绝超出硬上限的 TTL。
func normalizeTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		return constants.DefaultBootstrapTokenTTL, nil
	}
	if ttl < 0 || ttl > constants.MaxBootstrapTokenTTL {
		return 0, fmt.Errorf("--ttl %s 超出范围：必须大于 0 且不超过 %s", ttl, constants.MaxBootstrapTokenTTL)
	}
	return ttl, nil
}

// formatJoinCommand 渲染一条 join 命令。
func formatJoinCommand(endpoint string, m *jointoken.Material, configPath string) string {
	parts := []string{"kubemono", "join", endpoint, "--token", m.Token.String()}
	for _, h := range m.CACertHashes {
		parts = append(parts, "--discovery-token-ca-cert-hash", h)
	}
	if m.Role == clusterconfig.RoleMaster {
		parts = append(parts, "--control-plane", "--certificate-key", hex.EncodeToString(m.CertificateKey))
	}
	parts = append(parts, "-f", configPath)
	return strings.Join(parts, " ")
}

// firstJoinEndpoint 返回打印 join 命令时使用的第一个入口：
// 优先 externalEndpoints（VIP/DNS），否则按配置顺序取第一台 master。
func firstJoinEndpoint(cluster *clusterconfig.Cluster) string {
	if cluster == nil {
		return ""
	}
	if len(cluster.ControlPlane.ExternalEndpoints) > 0 {
		if ep := strings.TrimSpace(cluster.ControlPlane.ExternalEndpoints[0]); ep != "" {
			return ep
		}
	}
	for _, ep := range discovery.MasterEndpoints(cluster) {
		if ep = strings.TrimSpace(ep); ep != "" {
			return ep
		}
	}
	return ""
}
