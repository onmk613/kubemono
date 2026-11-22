// Package token 实现 `kubemono token` 命令组：签发其他节点加入集群的短期凭证。
package token

import (
	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
)

// New 返回 `kubemono token` 命令组。
func New(opts *options.GlobalOptions) *cobra.Command {
	return utils.Group(
		"token",
		"签发其他节点加入集群所需的凭证",
		"kubemono token create",
		newCreate(opts),
	)
}
