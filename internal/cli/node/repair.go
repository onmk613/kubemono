package node

import (
	"fmt"
	"time"

	"kubemono/internal/cli/installopts"
	"kubemono/internal/cli/options"
	installpkg "kubemono/internal/install"

	"github.com/spf13/cobra"
)

// repairLong 是 `kubemono node repair` 的帮助说明。
const repairLong = `幂等重跑已安装节点的完整 phase 列表，用于安装中途失败后的恢复。

不判断 init/join、不读取 join secret、也不探测集群；未安装节点（没有 node.yaml）
会被拒绝，请改用 kubemono init 或 kubemono join。

--only 只跑指定的 phase（逗号分隔，顺序按计划而不是命令行）。典型用法是改完
cluster.yaml 的 master 列表后只推 worker 的本地代理：
  kubemono node repair -f cluster.yaml --only local-proxy
--only 只接受本机计划内的 phase，init/join 不支持。

--diff 只读：渲染本机期望状态并与磁盘比较，打印缺失/内容/残留/权限差异
（Secret 只报状态，不打印内容），不做任何写入；此时不接受 --only。
--exit-code 与 --diff 同用：发现差异时返回非 0，便于 CI 判断。
--allow-release-mismatch 允许用不同 release 的 kubemono 修复已安装节点
（默认拒绝，避免半改节点）。
--timeout 限制整轮修复的时长（0 表示不限制），超时后当前 phase 中断并返回错误。`

// newNodeRepairCmd 实现 `kubemono node repair`：只修复已安装节点。
func newNodeRepairCmd(opts *options.GlobalOptions) *cobra.Command {
	var (
		only         []string
		rebind       bool
		diff         bool
		exitCode     bool
		timeout      time.Duration
		allowRelease bool
	)
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "幂等重跑已安装节点的安装 phase",
		Long:  repairLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if diff && len(only) > 0 {
				return fmt.Errorf("--diff 与 --only 不能同时使用：--diff 永远比较整机受管文件")
			}
			if exitCode && !diff {
				return fmt.Errorf("--exit-code 只能与 --diff 一起使用")
			}
			cluster, err := opts.LoadCluster()
			if err != nil {
				return err
			}
			result, ran, err := installopts.RunInstall(cmd, opts, cluster, nil, false, installpkg.ActionSync, only,
				func(io *installpkg.Options) error {
					io.RebindIdentity = rebind
					io.Diff = diff
					io.DiffExitCode = exitCode
					io.Timeout = timeout
					io.AllowReleaseMismatch = allowRelease
					return nil
				})
			if err != nil {
				return err
			}
			if !ran {
				return nil
			}
			opts.Reporter.Result(true, "节点修复完成（执行 %d 个 phase）", len(result.Summary.Applied()))
			return nil
		},
	}
	cmd = options.Add(cmd, opts,
		options.ConfigFlag|options.NodeFlag|options.DryRunFlag|options.KubeconfigFlag|options.DataDirFlag)
	cmd.Flags().StringSliceVar(&only, "only", nil, "只执行这些 phase（逗号分隔），例如 --only local-proxy")
	cmd.Flags().BoolVar(&rebind, "rebind-identity", false, "允许 --node 改写本机已有的节点身份（改名迁移）")
	cmd.Flags().BoolVar(&diff, "diff", false, "只打印受管文件与期望状态的差异，不做变更")
	cmd.Flags().BoolVar(&exitCode, "exit-code", false, "与 --diff 同用：发现差异时返回非 0")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "整轮修复的最长时长（0 表示不限制）")
	cmd.Flags().BoolVar(&allowRelease, "allow-release-mismatch", false, "允许用不同 release 的 kubemono 修复已安装节点")
	return cmd
}
