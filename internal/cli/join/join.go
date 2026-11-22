// Package join 实现 `kubemono join` 命令组：以加入者身份安装本机，并管理 join 请求。
// 命令层只解析参数与渲染结果，真正的逻辑在 internal/ 对应包里。
package join

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"kubemono/internal/bootstrap"
	"kubemono/internal/cli/installopts"
	"kubemono/internal/cli/options"
	clusterconfig "kubemono/internal/config"
	installpkg "kubemono/internal/install"
	"kubemono/internal/join/certs"
)

// joinLong 是 `kubemono join` 的帮助说明。
const joinLong = `以加入者身份安装本节点。默认使用以下命令行参数：

  # worker
  kubemono join 10.0.0.1:6443 --token abcdef.0123456789abcdef \
    --discovery-token-ca-cert-hash sha256:... -f cluster.yaml

  # master / control-plane
  kubemono join 10.0.0.1:6443 --token abcdef.0123456789abcdef \
    --discovery-token-ca-cert-hash sha256:... \
    --control-plane --certificate-key <64 位 hex> -f cluster.yaml

master 还需要用 certificate-key 解密 kube-system/kubemono-certs 里的证书包。
bootstrap token 默认只有 2 小时有效期；过期后在已初始化的 master 上执行
kubemono token create（worker）或 kubemono token create --role master（master）重新生成。`

// New 返回 `kubemono join` 命令（以加入者身份安装本机）。
func New(opts *options.GlobalOptions) *cobra.Command {
	var (
		token              string
		tokenFile          string
		caCertHashes       []string
		skipCAVerify       bool
		controlPlane       bool
		certificateKey     string
		certificateKeyFile string
		replacePKI         bool
	)
	cmd := &cobra.Command{
		Use:   "join [api-server-endpoint]",
		Short: "以加入者身份安装本节点",
		Long:  joinLong,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint := ""
			if len(args) == 1 {
				endpoint = strings.TrimSpace(args[0])
			}

			if token != "" && tokenFile != "" {
				return fmt.Errorf("--token 与 --token-file 只能给一个")
			}
			if certificateKey != "" && certificateKeyFile != "" {
				return fmt.Errorf("--certificate-key 与 --certificate-key-file 只能给一个")
			}
			if token == "" && tokenFile != "" {
				data, err := os.ReadFile(tokenFile)
				if err != nil {
					return fmt.Errorf("读取 --token-file %s 失败: %w", tokenFile, err)
				}
				token = strings.TrimSpace(string(data))
			}
			if certificateKey == "" && certificateKeyFile != "" {
				data, err := os.ReadFile(certificateKeyFile)
				if err != nil {
					return fmt.Errorf("读取 --certificate-key-file %s 失败: %w", certificateKeyFile, err)
				}
				certificateKey = strings.TrimSpace(string(data))
			}
			if token == "" {
				return fmt.Errorf("加入集群需要 --token 或 --token-file；" +
					"如果这是第一台 master，请先执行 kubemono init")
			}
			join, err := buildJoinSecret(token, endpoint, caCertHashes, skipCAVerify, controlPlane, certificateKey)
			if err != nil {
				return err
			}
			cluster, err := opts.LoadCluster()
			if err != nil {
				return err
			}
			if skipCAVerify && opts.Reporter != nil {
				opts.Reporter.Warnf("已跳过 --discovery-token-ca-cert-hash 校验：首跳只依赖 JWS，存在中间人风险")
			}
			_, _, err = installopts.RunInstall(cmd, opts, cluster, join, false, installpkg.ActionJoin, nil,
				func(io *installpkg.Options) error {
					io.ReplacePKI = replacePKI
					io.PreferredEndpoint = endpoint
					return nil
				})
			return err
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "bootstrap token，格式 <6 位 id>.<16 位 secret>")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "从文件读取 bootstrap token（避免出现在 argv//proc）")
	cmd.Flags().StringSliceVar(&caCertHashes, "discovery-token-ca-cert-hash", nil,
		"CA 公钥 pin，格式 sha256:<hex>，可重复")
	cmd.Flags().BoolVar(&skipCAVerify, "discovery-token-unsafe-skip-ca-verification", false,
		"跳过 CA pin 校验（不推荐；仅在无法提前获取 hash 时使用）")
	cmd.Flags().BoolVar(&controlPlane, "control-plane", false, "以 master/control-plane 身份加入")
	cmd.Flags().StringVar(&certificateKey, "certificate-key", "", "master 证书包解密 key（64 位 hex）")
	cmd.Flags().StringVar(&certificateKeyFile, "certificate-key-file", "", "从文件读取 certificate-key（避免出现在 argv//proc）")
	cmd.Flags().BoolVar(&replacePKI, "replace-pki", false, "允许用集群证书包覆盖本机已有且不同的 CA 私钥")
	return options.Add(cmd, opts,
		options.ConfigFlag|options.NodeFlag|options.AllowAddressMismatchFlag|options.DryRunFlag|options.WaitFlag|options.DataDirFlag)
}

// buildJoinSecret 把命令行参数转成 JoinSecret。
func buildJoinSecret(rawToken, endpoint string, hashes []string, skipCAVerify, controlPlane bool, certificateKey string) (*clusterconfig.JoinSecret, error) {
	token, err := bootstrap.ParseToken(strings.TrimSpace(rawToken))
	if err != nil {
		return nil, fmt.Errorf("--token 无效: %w", err)
	}
	role := clusterconfig.RoleWorker
	if controlPlane {
		role = clusterconfig.RoleMaster
	}
	key, err := normalizeCertificateKey(certificateKey)
	if err != nil {
		return nil, err
	}
	switch {
	case role == clusterconfig.RoleMaster && key == "":
		return nil, fmt.Errorf("master 加入需要 --certificate-key（64 位 hex）")
	case role == clusterconfig.RoleWorker && key != "":
		return nil, fmt.Errorf("worker 加入不能使用 --certificate-key：只有 master 需要解密证书包")
	}
	if len(hashes) == 0 && !skipCAVerify {
		return nil, fmt.Errorf("必须提供 --discovery-token-ca-cert-hash，" +
			"或显式使用 --discovery-token-unsafe-skip-ca-verification")
	}
	for i, h := range hashes {
		h = strings.TrimSpace(h)
		if !strings.HasPrefix(strings.ToLower(h), "sha256:") {
			return nil, fmt.Errorf("--discovery-token-ca-cert-hash 必须是 sha256:<hex> 格式，第 %d 个是 %q", i+1, h)
		}
		hashes[i] = h
	}
	if strings.HasPrefix(strings.ToLower(endpoint), "http://") {
		return nil, fmt.Errorf("拒绝使用明文 http:// 端点 %q 加入：引导令牌会以明文传输", endpoint)
	}
	join := &clusterconfig.JoinSecret{
		Role:           role,
		Token:          token.String(),
		CertificateKey: key,
		CACertHashes:   hashes,
	}
	if endpoint != "" {
		join.DiscoveryEndpoints = []string{endpoint}
	}
	return join, nil
}

// normalizeCertificateKey 把 hex certificate-key 转成 JoinSecret 使用的 base64。
func normalizeCertificateKey(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == certs.KeySize {
		return base64.StdEncoding.EncodeToString(b), nil
	}
	return "", fmt.Errorf("--certificate-key 必须是 %d 位 hex", certs.KeySize*2)
}
