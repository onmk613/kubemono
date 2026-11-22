// Package initcmd 实现 `kubemono init` 命令。
// init 只负责把集群建起来：主机基线、PKI、控制面、集群级资源、网络与附加组件。
// 其他节点的加入凭证由 `kubemono token create` 单独签发，init 不生成任何 join 材料。
package initcmd

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/installopts"
	"kubemono/internal/cli/options"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	installpkg "kubemono/internal/install"
)

// initLong 是 `kubemono init` 的帮助说明。
const initLong = "引导一个新集群（只能在一台 master 上执行）。\n\n" +
	"init 只创建集群本身：主机基线、PKI、控制面、集群级资源、网络与附加组件。\n" +
	"初始化完成后，用 `kubemono token create` 签发其他节点的加入凭证：\n" +
	"默认签发 worker 凭证，master 加入需显式 `--role master`。\n" +
	"join 凭证不落盘，bootstrap token 2 小时内有效。"

func New(opts *options.GlobalOptions) *cobra.Command {
	var (
		forceInit     bool
		allowExisting bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "初始化集群（只能在一台 master 上执行）",
		Long:  initLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cluster, err := opts.LoadCluster()
			if err != nil {
				return err
			}
			if !allowExisting {
				if endpoint, reachable := reachableAPIServer(cmd.Context(), cluster); reachable {
					return fmt.Errorf("检测到已有 apiserver 可达（%s）：本机可能已属于运行中的集群，"+
						"请改用 kubemono join --token ...；确需重建集群时加 --allow-existing-cluster", endpoint)
				}
			}
			result, ran, err := installopts.RunInstall(cmd, opts, cluster, nil, forceInit, installpkg.ActionInit, nil, nil)
			if err != nil {
				return err
			}
			if !ran {
				return nil
			}
			reporter := opts.ReporterOrDiscard()
			reporter.Infof("集群初始化完成（clusterID=%s）；init 不生成其他节点的加入凭证。", result.ClusterID)
			reporter.Infof("需要加入节点时，在本机执行：")
			reporter.Infof("  kubemono token create                 # worker（默认）")
			reporter.Infof("  kubemono token create --role master   # 控制面/master")
			return nil
		},
	}
	cmd.Flags().BoolVar(&forceInit, "force-init", false, "本机有残留 etcd 数据时仍执行 init（仅用于灾难恢复）")
	cmd.Flags().BoolVar(&allowExisting, "allow-existing-cluster", false, "跳过“已有 apiserver 可达就拒绝 init”的检查（重建集群时使用）")

	return options.Add(cmd, opts,
		options.ConfigFlag|options.NodeFlag|options.AllowAddressMismatchFlag|options.DryRunFlag|options.DataDirFlag)
}

// reachableAPIServer 检查配置中的 master 是否已有 apiserver 在监听。
func reachableAPIServer(ctx context.Context, cluster *clusterconfig.Cluster) (string, bool) {
	if cluster == nil {
		return "", false
	}
	for _, m := range cluster.Cluster.Hosts.Masters() {
		endpoint := net.JoinHostPort(m.Address, strconv.Itoa(constants.APIServerPort))
		dialer := &net.Dialer{Timeout: 2 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", endpoint)
		if err == nil {
			_ = conn.Close()
			return endpoint, true
		}
	}
	return "", false
}
