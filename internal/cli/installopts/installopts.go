// Package installopts 构造 install/init/join 共用的 install.Options。
package installopts

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"k8s.io/client-go/kubernetes"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/identity"
	"kubemono/internal/install"
)

// OptionsFor 根据命令 flag 与内存里的 JoinSecret 构造 install 选项。
// join 凭证不从文件读取：--token/--control-plane 直接从命令行传进来。
func OptionsFor(opts *options.GlobalOptions, cluster *clusterconfig.Cluster, join *clusterconfig.JoinSecret, forceInit bool) (*install.Options, error) {
	effectiveBOM, err := opts.EffectiveBOM(cluster)
	if err != nil {
		return nil, err
	}
	io := &install.Options{
		Cluster:              cluster,
		Release:              effectiveBOM.Release,
		Node:                 opts.Node,
		AllowAddressMismatch: opts.AllowAddressMismatch,
		ForceInit:            forceInit,
		DryRun:               opts.DryRun,
		Wait:                 opts.Wait,
		Paths:                opts.Paths(),
		BOM:                  effectiveBOM,
		Report:               opts.Reporter,
		JoinSecret:           join,
	}
	// 集群级 phase（cluster-init、mark-control-plane、network、addon）
	// 需要凭据，而 admin.conf 由同一个 phase 序列里的 kubeconfig phase
	// 写出，所以这里只提供惰性构造函数，真正读凭据发生在 phase 执行时。
	io.ClientFunc = func() (kubernetes.Interface, error) { return utils.InstallClient(opts) }
	io.Resolver = &identity.Resolver{NodeConfigPath: opts.Paths().NodeConfig()}
	return io, nil
}

// RunInstall 执行 init / join / node repair 共用的四步：
// 构造 install.Options → 强制动作 → Decide → 打印计划 →（dry-run 短路）→ prepare → Run。
//
// prepare 在 Decide 之后、Run 之前调用：`kubemono join` 用它设置 join 专属选项
// （--replace-pki / 首选 endpoint）。`kubemono init` 不需要它。
//
// ran=false 表示 dry-run：调用方不要再写任何旁路产物。
func RunInstall(
	cmd *cobra.Command,
	opts *options.GlobalOptions,
	cluster *clusterconfig.Cluster,
	join *clusterconfig.JoinSecret,
	forceInit bool,
	action install.Action,
	only []string,
	prepare func(*install.Options) error,
) (result *install.Result, ran bool, err error) {
	io, err := OptionsFor(opts, cluster, join, forceInit)
	if err != nil {
		return nil, false, err
	}
	return runInstall(cmd, opts, io, action, only, prepare)
}

// runInstall 是 init / join / repair 共用的执行序列。
func runInstall(
	cmd *cobra.Command,
	opts *options.GlobalOptions,
	io *install.Options,
	action install.Action,
	only []string,
	prepare func(*install.Options) error,
) (result *install.Result, ran bool, err error) {
	io.ForceAction = action
	io.Only = only
	plan, err := install.Decide(cmd.Context(), io)
	if err != nil {
		return nil, false, err
	}
	planOut := cmd.OutOrStdout()
	if opts.Reporter != nil && opts.Reporter.ProgressRedirected() {
		// 进度已改到 stderr（例如 token create 打印 join 命令）：安装计划同样不能污染 stdout。
		planOut = cmd.ErrOrStderr()
	}
	_, _ = fmt.Fprint(planOut, plan.Describe())
	if opts.DryRun {
		return nil, false, nil
	}
	if prepare != nil {
		if err := prepare(io); err != nil {
			return nil, false, err
		}
	}
	lk, err := opts.LockForWrite()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = lk.Close() }()
	result, err = install.Run(cmd.Context(), io)
	if result != nil && len(result.Summary.Pending) > 0 {
		opts.ReporterOrDiscard().Warnf("以下 phase 执行成功但尚未收敛（下次 repair 会继续）：%s",
			strings.Join(result.Summary.Pending, "、"))
	}
	return result, true, err
}
