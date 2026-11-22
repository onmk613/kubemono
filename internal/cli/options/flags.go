// Package options 保存 kubemono 各命令共享的选项值与 flag 组。
package options

import (
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"kubemono/internal/constants"
)

// Flags 选择命令接受的共享 flag 组。
// 只有真正被命令读取的 flag 才应该挂上去：把 root 上的全部 flag 继承给每个
// 子命令会让 `kubemono version --wait` 这类调用被静默忽略，也会让帮助失真。
type Flags uint32

const (
	// ConfigFlag 是 -f/--config（集群配置文件）。
	ConfigFlag Flags = 1 << iota
	// OutputFlag 是 --output（table|json|yaml）。
	OutputFlag
	// NodeFlag 是 --node（覆盖本机节点标识）。
	NodeFlag
	// AssumeYesFlag 是 -y/--assume-yes（跳过交互确认）。
	AssumeYesFlag
	// AllowAddressMismatchFlag 是 --allow-address-mismatch（NAT 场景下允许
	// 配置里的节点地址不在本机网卡上，必须配合 --node 使用）。
	AllowAddressMismatchFlag
	// DryRunFlag 是 --dry-run。
	DryRunFlag
	// WaitFlag 是 --wait（限制阻塞操作的等待时间）。
	WaitFlag
	// KubeconfigFlag 是 --kubeconfig（集群级命令使用的凭据）。
	KubeconfigFlag
	// VerboseFlag 是 -v/--verbose（调试日志）。
	VerboseFlag
	// DataDirFlag 是 --data-dir（kubemono 自身数据目录）。
	DataDirFlag
)

// Add 在 cmd 的本地 flag 集上注册选中的 flag，并返回 cmd，方便调用方写成
func Add(cmd *cobra.Command, opts *GlobalOptions, set Flags) *cobra.Command {
	addFlags(cmd.Flags(), opts, set)
	return cmd
}

// AddPersistent 在 cmd 的 persistent flag 集上注册选中的 flag，使子命令能够
// 继承。仅在 root 或命令组根上使用；叶子命令应使用 Add。
func AddPersistent(cmd *cobra.Command, opts *GlobalOptions, set Flags) *cobra.Command {
	addFlags(cmd.PersistentFlags(), opts, set)
	return cmd
}

func addFlags(fs *pflag.FlagSet, opts *GlobalOptions, set Flags) {
	// 注册时用字段当前值作为默认值：多个子命令共享同一份 GlobalOptions，
	// 若无条件写回零值，后注册的命令会把调用方（或测试）预设的值清掉。
	if set&ConfigFlag != 0 {
		fs.StringVarP(&opts.ConfigPath, "config", "f", opts.ConfigPath, "cluster 配置文件（默认 "+constants.ClusterConfigFile+"）")
	}
	if set&OutputFlag != 0 {
		def := opts.Output
		if def == "" {
			def = "table"
		}
		fs.StringVar(&opts.Output, "output", def, "输出格式：table|json|yaml")
	}
	if set&NodeFlag != 0 {
		fs.StringVar(&opts.Node, "node", opts.Node, "显式指定本机在配置中的节点名")
	}
	if set&AssumeYesFlag != 0 {
		fs.BoolVarP(&opts.AssumeYes, "assume-yes", "y", false, "跳过交互确认（不改变任何检查）")
	}
	if set&AllowAddressMismatchFlag != 0 {
		fs.BoolVar(&opts.AllowAddressMismatch, "allow-address-mismatch", false,
			"允许 --node 的地址不在本机网卡上（NAT 场景）")
	}
	if set&DryRunFlag != 0 {
		fs.BoolVar(&opts.DryRun, "dry-run", false, "只显示将要执行的变更")
	}
	if set&WaitFlag != 0 {
		def := opts.Wait
		if def <= 0 {
			def = constants.DefaultInstallWait
		}
		fs.DurationVar(&opts.Wait, "wait", def, "阻塞操作的最长等待时间")
	}
	if set&KubeconfigFlag != 0 {
		fs.StringVar(&opts.KubeconfigPath, "kubeconfig", opts.KubeconfigPath, "集群级命令使用的 kubeconfig（默认 master 上的 "+constants.KubeconfigAdminPath+"）")
	}
	if set&VerboseFlag != 0 {
		fs.BoolVarP(&opts.Verbose, "verbose", "v", false, "输出调试信息")
	}
	if set&DataDirFlag != 0 {
		fs.StringVar(&opts.DataDir, "data-dir", opts.DataDir, "kubemono 自身数据目录（默认 "+constants.KubemonoDir+"）")
	}
}

const (
	groupCommands = "commands"
	groupHelp     = "help"
)

// SetupCommandGroups 将 root 下所有未分组的命令归入 Commands 组，
// help 和 completion 归入 Help 组（显示在最后）。
//
// 注意：cobra 的 help 模板会无条件列出名为 help 的命令（Hidden 也不生效），
// 所以内置命令的收敛方式是分组，而不是隐藏。
// 必须在所有 AddCommand 之后调用。
func SetupCommandGroups(root *cobra.Command) {
	root.AddGroup(
		&cobra.Group{ID: groupCommands, Title: "Commands:"},
		&cobra.Group{ID: groupHelp, Title: "Help:"},
	)

	for _, c := range root.Commands() {
		if c.GroupID == "" {
			c.GroupID = groupCommands
		}
	}

	root.SetHelpCommandGroupID(groupHelp)
	root.SetCompletionCommandGroupID(groupHelp)
}
