package node

import (
	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
)

// New 组装 `kubemono node` 命令组。
func New(opts *options.GlobalOptions) *cobra.Command {
	return utils.Group("node", "本机节点修复", "kubemono node repair", newNodeRepairCmd(opts))
}
