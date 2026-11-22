package configcmd

import (
	"encoding/json"
	"fmt"
	"strings"

	clusterconfig "kubemono/internal/config"

	"github.com/spf13/cobra"
)

// defaultSectionComments 给 config default 的 6 个命名段加阅读提示。
var defaultSectionComments = map[string]string{
	"cluster:":      "集群成员",
	"node:":         "节点主机、运行时、kubelet 与工具",
	"controlPlane:": "控制平面：PKI、etcd、组件参数与访问入口",
	"network:":      "网络、Service 代理与路由",
	"addons:":       "内置附加组件",
	"images:":       "按组件镜像与组件版本（可选）",
}

// withSectionComments 在顶层段之间插入空行与中文说明。
func withSectionComments(body []byte) []byte {
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	out := make([]string, 0, len(lines)+2*len(defaultSectionComments))
	for _, line := range lines {
		for key, comment := range defaultSectionComments {
			if line != key && !strings.HasPrefix(line, key+" ") {
				continue
			}
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			out = append(out, "# "+comment)
			break
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// defaultLong 是 `kubemono config default` 的帮助说明。
const defaultLong = `输出一份完整的默认集群配置，包含所有非空默认值和单 master 占位拓扑。

cluster.hosts 是占位值，安装前必须替换；其余字段可直接使用。`

// defaultConfigHeader 是 `config default` 输出的 YAML 说明。
// 注释只加在 YAML 前面；JSON 输出保持纯粹的文档内容。
const defaultConfigHeader = `# kubemono 默认集群配置（由 kubemono config default 生成）。
#
# cluster.hosts 是占位值，
# 安装前请按实际环境修改；其余字段均为 kubemono 内置默认值。
# host.name 只是配置身份；host.nodeName 才是实际 NodeName（hostname/address/显式值）。
# 首次初始化请在任一 master 上执行 kubemono init。
# 校验：kubemono config validate -f cluster.yaml
`

func newConfigDefaultCmd() *cobra.Command {
	var format = "yaml"
	cmd := &cobra.Command{
		Use:   "default",
		Short: "输出一份带全部内置默认值的 cluster.yaml",
		Long:  defaultLong,
		Example: `  # 生成模板（重定向到文件，已存在则覆盖）
  kubemono config default > cluster.yaml

  # 输出 JSON 模板
  kubemono config default --output json > cluster.json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cluster := clusterconfig.DefaultCluster()
			var data []byte
			var err error
			switch format {
			case "json":
				data, err = json.MarshalIndent(cluster, "", "  ")
				if err == nil {
					data = append(data, '\n')
				}
			case "yaml":
				var body []byte
				body, err = clusterconfig.MarshalCluster(cluster)
				if err == nil {
					data = append([]byte(defaultConfigHeader), withSectionComments(body)...)
				}
			default:
				return fmt.Errorf("config default 只支持 --output json|yaml（收到 %q）", format)
			}
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	// --output 在本命令里表示“配置文档格式”，不是报表格式：默认 yaml，
	// 只接受 yaml|json。root 的 --verbose 作为 inherited flag 正常生效。
	cmd.Flags().StringVar(&format, "output", "yaml", "配置文档格式：yaml|json")
	return cmd
}
