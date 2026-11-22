// Package whoami 实现 `kubemono whoami`：只读打印本机被识别成哪个节点。
package whoami

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	"kubemono/internal/identity"
)

// identityView 是 `whoami` 的输出负载；table 与 json/yaml 共用。
type identityView struct {
	ConfigName string `json:"configName"`
	// NodeName 是本机现在（按配置的 nodeName 策略 + 当前 hostname）会注册的名字。
	NodeName string `json:"nodeName"`
	// InstalledNodeName 是 node.yaml 里记录的安装时 NodeName；与 NodeName
	// 不一致通常意味着配置或主机名在安装后变过。
	InstalledNodeName string   `json:"installedNodeName,omitempty"`
	Address           string   `json:"address"`
	Roles             []string `json:"roles"`
	Installed         bool     `json:"installed"`
	ClusterID         string   `json:"clusterID,omitempty"`
	Release           string   `json:"release,omitempty"`
}

const whoamiLong = `只读解析 cluster.yaml 与本机 node.yaml，打印本机被识别成哪个节点。

用于排障：--node 覆盖、NAT 地址不匹配、node.yaml 与配置漂移时，先看这里。
不做任何写入，也不访问 apiserver。`

// New 返回 `kubemono whoami`。
func New(opts *options.GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "打印本机身份（配置名/NodeName/地址/角色）",
		Long:  whoamiLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cluster, err := opts.LoadCluster()
			if err != nil {
				return err
			}
			resolver := &identity.Resolver{NodeConfigPath: opts.Paths().NodeConfig()}
			res, err := resolver.Resolve(cluster, opts.Node, opts.AllowAddressMismatch)
			if err != nil {
				return err
			}
			for _, warning := range res.Warnings {
				opts.ReporterOrDiscard().Warnf("%s", warning)
			}
			view := identityView{
				NodeName:  res.NodeName(),
				Address:   res.Address,
				Installed: res.NodeConfig != nil,
			}
			if res.Host != nil {
				view.ConfigName = res.Host.Name
				for _, role := range res.Host.Roles {
					view.Roles = append(view.Roles, string(role))
				}
			}
			if res.NodeConfig != nil {
				view.InstalledNodeName = res.NodeConfig.NodeName
				view.ClusterID = res.NodeConfig.ClusterID
				view.Release = res.NodeConfig.Release
			}
			return utils.Print(cmd.OutOrStdout(), opts.Output, view, func(w io.Writer) error {
				_, _ = fmt.Fprint(w, term.KeyValuesAligned(map[string]string{
					"configName": view.ConfigName,
					"nodeName":   view.NodeName,
					"已安装名":       view.InstalledNodeName,
					"address":    view.Address,
					"roles":      joinRoles(view.Roles),
					"installed":  term.YesNo(view.Installed),
					"clusterID":  view.ClusterID,
					"release":    view.Release,
				}))
				return nil
			})
		},
	}
	return options.Add(cmd, opts,
		options.ConfigFlag|options.NodeFlag|options.AllowAddressMismatchFlag|options.OutputFlag|options.DataDirFlag)
}

func joinRoles(roles []string) string {
	if len(roles) == 0 {
		return "-"
	}
	return strings.Join(roles, ",")
}
