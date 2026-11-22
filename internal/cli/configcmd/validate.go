package configcmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	clusterconfig "kubemono/internal/config"
)

func newConfigValidateCmd(opts *options.GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "校验 cluster.yaml（未知字段、默认值、校验规则、版本/镜像覆盖）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// 必须走 Paths()：--data-dir/测试根会把 kubemono 路径重定位。
			// 这里曾直接用 constants.ClusterConfigFile（绝对 /usr/local/kubemono/cluster.yaml），
			// 否则会去读宿主机上的生产配置。
			path, _ := opts.ClusterConfigPath()
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// 严格解析 + 聚合校验：未知字段直接拒绝
			// （与 LoadCluster 的入口约束一致），其余错误一次给全。
			cluster, res, err := clusterconfig.ValidateClusterDocument(data)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			report := opts.ReporterOrDiscard()
			for _, w := range res.Warnings {
				report.Warnf("%s", w)
			}
			if !res.OK() {
				for _, e := range res.Errors {
					report.Errorf("%s", e.Error())
				}
				return fmt.Errorf("%s: 校验失败，共 %d 个错误", path, len(res.Errors))
			}
			// 版本/镜像覆盖：未知组件名报错；任意 k8s 版本都允许，与默认版本不同时
			// 只告警（兼容性由操作者负责）。
			if _, overrideErr := opts.EffectiveBOM(cluster); overrideErr != nil {
				report.Errorf("%v", overrideErr)
				return fmt.Errorf("%s: 组件版本/镜像覆盖校验失败: %w", path, overrideErr)
			}
			out := cmd.OutOrStdout()
			return utils.Print(out, opts.Output, clusterDoc{Cluster: cluster}, func(w io.Writer) error {
				masters := len(cluster.Cluster.Hosts.Masters())
				workers := len(cluster.Cluster.Hosts.Workers())
				_, _ = fmt.Fprint(w, term.KeyValues(map[string]string{
					"masters":        fmt.Sprintf("%d", masters),
					"workers":        fmt.Sprintf("%d", workers),
					"etcd":           cluster.ControlPlane.Etcd.Mode(),
					"network plugin": cluster.Network.Plugin,
					"service proxy":  clusterconfig.ServiceProxyMode(cluster),
					"podCIDR":        cluster.Network.PodCIDR,
					"serviceCIDR":    cluster.Network.ServiceCIDR,
					"clusterDNS":     cluster.Network.ClusterDNS,
					"runtime":        cluster.Node.Runtime.Mode(),
				}))
				// “校验通过”提示只属于 human 输出：机器可读分支（json/yaml）
				// 历来不带这行文案，收敛到 utils.Print 后行为保持不变。
				report.Result(true, "%s 校验通过", path)
				return nil
			})
		},
	}
	return options.Add(cmd, opts, options.ConfigFlag|options.OutputFlag|options.DataDirFlag)
}

// clusterDoc 让 utils.Print 输出机器可读的 cluster.yaml。
// yaml 必须走 clusterconfig.MarshalCluster：sigs.k8s.io/yaml 经 JSON 转换会把顶层
// 字段排成字母序，而各命名段的文档顺序是面向人的契约；
// json 分支则等价于直接序列化内嵌的 Cluster。
type clusterDoc struct {
	*clusterconfig.Cluster
}

func (d clusterDoc) WriteYAMLTo(w io.Writer) error {
	data, err := clusterconfig.MarshalCluster(d.Cluster)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
