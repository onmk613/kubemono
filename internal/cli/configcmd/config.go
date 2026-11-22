// Package configcmd 实现 `kubemono config` 命令组：生成与校验集群配置。
// 命令层只解析参数与渲染结果，真正的逻辑在 internal/ 对应包里。
package configcmd

import (
	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
)

// New 组装 `kubemono config` 命令组。
func New(opts *options.GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "生成和校验集群配置",
	}
	cmd.AddCommand(
		newConfigDefaultCmd(),
		newConfigValidateCmd(opts),
	)
	return cmd
}
