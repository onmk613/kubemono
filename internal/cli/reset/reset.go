// Package reset 实现 `kubemono reset` 命令组：重置本机：卸载 kubemono 组件并清理主机改动。
// 命令层只解析参数与渲染结果，真正的逻辑在 internal/ 对应包里。
package reset

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
	clusterconfig "kubemono/internal/config"
	leavepkg "kubemono/internal/leave"
	kubemonoreset "kubemono/internal/reset"
)

// resetLong 是 `kubemono reset` 的帮助说明。
const resetLong = `备份 PKI 与静态加密配置（/etc/kubernetes/encryption）后删除
/etc/kubernetes 与 kubelet/etcd 数据目录，并清理 kubemono 创建的 iptables/ipvs
规则、网络接口、CNI 配置与 unit。

--leave 先执行退群流程（cordon、移除 etcd 成员、删除 Node；驱逐由 kubectl drain 完成）再做本机
reset；命令默认要求确认，-y/--assume-yes 跳过。

--purge 在 reset 基础上继续删除运行二进制（/usr/local/bin 中 BOM 声明的文件）、
CNI 插件、containerd 配置/数据、/usr/local/kubemono 状态与物料、运行时 unit；
PKI 会先备份到 <data-dir>/backup/<timestamp>/。--purge 不能与 --keep-data /
--keep-hostinit 同时使用。`

func New(opts *options.GlobalOptions) *cobra.Command {
	var (
		keepData     bool
		keepHostInit bool
		leaveCluster bool
		purge        bool
		forceLeave   bool
	)
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "重置本机，或让本节点退出集群",
		Long:  resetLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if purge && (keepData || keepHostInit) {
				return fmt.Errorf("--purge 会删除二进制、数据目录和主机改动，不能与 --keep-data/--keep-hostinit 同时使用")
			}
			if leaveCluster {
				// --leave 会先退群再走同一套本机 reset，因此这里必须把两个 keep 开关
				// 一起传下去：漏传会让 `reset --leave --keep-data` 静默删掉数据目录。
				return runLeave(cmd, opts, keepData, keepHostInit, purge, forceLeave)
			}
			if cmd.Flags().Changed("kubeconfig") {
				return fmt.Errorf("--kubeconfig 只在 --leave 时使用：不退群的本机重置不访问 apiserver")
			}
			if cmd.Flags().Changed("force-leave") {
				return fmt.Errorf("--force-leave 只在 --leave 时使用")
			}
			// 显式 -f 读不到时必须报错：reset 是破坏性命令，不能因为配置
			// 打错就退回默认路径/默认运行时类型去清理。
			var cluster *clusterconfig.Cluster
			var err error
			if opts.ConfigPath != "" {
				cluster, err = opts.LoadCluster()
				if err != nil {
					return err
				}
			} else {
				cluster, _ = opts.LoadCluster()
			}
			prompt := "确定重置本机并删除 kubemono 管理的文件？"
			if purge {
				prompt = "确定彻底清理本机（二进制/CNI/containerd 数据/配置/服务/物料）？"
			}
			if err := utils.ConfirmOrCancel(cmd.InOrStdin(), cmd.OutOrStdout(),
				opts.AssumeYes, opts.DryRun, prompt); err != nil {
				return err
			}
			paths := opts.Paths()
			if cluster != nil {
				paths = cluster.WithDataDirs(paths)
			}
			ro := &kubemonoreset.Options{
				Paths:             paths,
				RuntimeType:       localRuntimeType(cluster),
				KeepData:          keepData,
				KeepHostInit:      keepHostInit,
				Purge:             purge,
				ContainerdDataDir: containerdDataDirFor(cluster),
				RuntimeUnit:       runtimeUnitFor(cluster),
				DryRun:            opts.DryRun,
				Report:            opts.Reporter,
			}
			lk, err := opts.LockForWrite()
			if err != nil {
				return err
			}
			defer func() { _ = lk.Close() }()
			res, err := kubemonoreset.Run(cmd.Context(), ro)
			printResetResult(opts, res)
			return err
		},
	}
	cmd.Flags().BoolVar(&keepData, "keep-data", false, "保留 etcd 与 kubelet 数据目录")
	cmd.Flags().BoolVar(&keepHostInit, "keep-hostinit", false, "保留主机初始化改动")
	cmd.Flags().BoolVar(&leaveCluster, "leave", false, "先让本节点退出集群，再重置本机")
	cmd.Flags().BoolVar(&purge, "purge", false, "彻底清理运行二进制、CNI、containerd 配置/数据和 /usr/local/kubemono（不可与 keep 开关同用）")
	cmd.Flags().BoolVar(&forceLeave, "force-leave", false, "退群时跳过 cordon 失败与 etcd 法定人数校验（节点已损坏时使用）")
	return options.Add(cmd, opts,
		options.ConfigFlag|options.AssumeYesFlag|options.DryRunFlag|options.KubeconfigFlag|options.DataDirFlag)
}

// runLeave 执行 --leave：退群（含 etcd 成员移除与 Node 删除）后再 reset。
func runLeave(cmd *cobra.Command, opts *options.GlobalOptions, keepData, keepHostInit, purge, forceLeave bool) error {
	if err := utils.ConfirmOrCancel(cmd.InOrStdin(), cmd.OutOrStdout(),
		opts.AssumeYes, opts.DryRun, "确定让本节点退出集群并清理本机？"); err != nil {
		return err
	}
	lk, err := opts.LockForWrite()
	if err != nil {
		return err
	}
	defer func() { _ = lk.Close() }()
	lo, err := newLeaveOptions(opts, keepData, keepHostInit, purge, forceLeave)
	if err != nil {
		return err
	}
	res, err := leavepkg.Leave(cmd.Context(), lo)
	printLeaveResult(opts, res, "本节点已退出集群")
	return err
}

// localRuntimeType 返回本机运行时类型（containerd|custom）。
func localRuntimeType(cluster *clusterconfig.Cluster) string {
	if cluster != nil {
		return cluster.Node.Runtime.Mode()
	}
	return ""
}

// printResetResult 输出重置摘要（dry-run 输出"将删除"清单的条数，清单本身由
// internal/reset 逐步打印）。
func printResetResult(opts *options.GlobalOptions, res *kubemonoreset.Result) {
	if res == nil {
		return
	}
	report := opts.Reporter
	if res.DryRun {
		backup := res.BackupBase
		if backup == "" {
			backup = opts.Paths().Backup()
		}
		report.Infof("dry-run：将删除 %d 项受管路径，PKI 将备份到 %s/<时间戳>/pki",
			len(res.WouldDelete), backup)
		return
	}
	report.Infof("已删除 %d 项受管路径", len(res.Deleted))
	if res.BackupDir != "" {
		report.Infof("备份目录：%s（pki、encryption 按存在情况）", res.BackupDir)
	}
	for _, item := range res.Skipped {
		report.Infof("已跳过：%s", item)
	}
	for _, item := range res.NetworkErrors {
		report.Warnf("网络清理未完成：%s", item)
	}
	for _, item := range res.Warnings {
		report.Warnf("%s", item)
	}
	report.Result(true, "本机重置完成（%s）", resetStepSummary(res))
}

// resetStepSummary 汇总五步的执行情况。
func resetStepSummary(res *kubemonoreset.Result) string {
	steps := make([]string, 0, len(res.Steps))
	for _, step := range res.Steps {
		state := string(step.Step)
		if !step.Done {
			state += "(未完成)"
		}
		steps = append(steps, state)
	}
	if len(steps) == 0 {
		return "无步骤"
	}
	return strings.Join(steps, " → ")
}
