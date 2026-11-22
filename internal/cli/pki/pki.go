// Package pki 提供离线 PKI 辅助命令：查看证书、用已有 CA 签发叶子证书。
// 它不创建 CA、不管理集群 PKI、不自动写入 /etc/kubernetes；集群证书仍由
// certs phase 和 cert renew 管理。这样外部 etcd、测试环境或企业临时需求可以
// 复用同一套离线工具，而不会和安装流程耦合。
package pki

import (
	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
)

// New 返回 `kubemono pki` 命令组。
func New(opts *options.GlobalOptions) *cobra.Command {
	return utils.Group("pki",
		"离线 PKI 辅助：用已有 CA 签发叶子证书（查看证书用 kubemono cert show）",
		"kubemono pki issue", newIssueCmd(opts))
}
