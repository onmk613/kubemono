// Package kubeconfig 生成并读取 kubemono 的 kubeconfig 文件。
// 约定：
// - 所有节点本机组件都访问 https://127.0.0.1:6443（master 是本机 apiserver，
// worker 是本地代理），因此 server 由调用方显式给出；
// - kubeconfig 落盘一律 0600、原子写入：它等价于一份可用的集群凭据；
// - 集群/用户/上下文使用固定的条目名，保证同一身份重复渲染得到同样的文件。
package kubeconfig

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"kubemono/internal/util/fsutil"
)

// 固定的条目名与用户。
const (
	// ClusterName 是 kubeconfig 中集群条目的名称。
	ClusterName = "kubernetes"
	// DefaultUserName 是 Config.User 为空时使用的用户名。
	DefaultUserName = "kubemono"
)

// Config 描述一份 kubeconfig 的身份与连接信息。
// 证书分组（O）不在这里：clientcmdapi 没有「身份分组」字段
// （ImpersonateGroups 是模拟，会改变鉴权语义），分组必须由客户端证书的
// Subject.Organization 表达。
type Config struct {
	// User 是证书 CN，也是 kubeconfig 里用户条目的名称。
	User string
	// Server 是 apiserver 地址，形如 https://127.0.0.1:6443。
	Server string
	// CACertPath 是集群 CA 证书路径；与 CACertData 二选一。
	CACertPath string
	// CACertData 是内嵌的 CA 证书（cluster-info 使用）。
	CACertData []byte
	// ClientCertPath / ClientKeyPath 是客户端证书与私钥路径。
	ClientCertPath string
	ClientKeyPath  string
	// Token 是 bearer token；与客户端证书可以二选一。
	Token string
}

// UserName 返回用户条目名称，未指定时用 DefaultUserName。
func (c Config) UserName() string {
	if u := strings.TrimSpace(c.User); u != "" {
		return u
	}
	return DefaultUserName
}

// ContextName 返回上下文条目名称，形如 <user>@kubernetes。
func (c Config) ContextName() string {
	return c.UserName() + "@" + ClusterName
}

// Build 根据 Config 组装 clientcmdapi.Config。
// 生成的条目名固定为 ClusterName 与 ContextName，CurrentContext 指向后者。
func Build(cfg Config) clientcmdapi.Config {
	user := cfg.UserName()
	ctx := cfg.ContextName()
	authInfo := &clientcmdapi.AuthInfo{
		ClientCertificate: cfg.ClientCertPath,
		ClientKey:         cfg.ClientKeyPath,
		Token:             cfg.Token,
	}
	cluster := &clientcmdapi.Cluster{Server: cfg.Server}
	if cfg.CACertPath != "" {
		cluster.CertificateAuthority = cfg.CACertPath
	} else {
		cluster.CertificateAuthorityData = cfg.CACertData
	}
	return clientcmdapi.Config{
		Preferences: clientcmdapi.Preferences{},
		Clusters: map[string]*clientcmdapi.Cluster{
			ClusterName: cluster,
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{user: authInfo},
		Contexts: map[string]*clientcmdapi.Context{
			ctx: {Cluster: ClusterName, AuthInfo: user},
		},
		CurrentContext: ctx,
	}
}

// Marshal 序列化 kubeconfig。
func Marshal(cfg clientcmdapi.Config) ([]byte, error) {
	data, err := clientcmd.Write(cfg)
	if err != nil {
		return nil, fmt.Errorf("序列化 kubeconfig 失败: %w", err)
	}
	return data, nil
}

// WriteToFile 把 kubeconfig 以 0600 原子写入 path（父目录 0755）。
func WriteToFile(path string, cfg clientcmdapi.Config) error {
	data, err := Marshal(cfg)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data, 0o600)
}

// Load 从 path 读取并解析 kubeconfig。文件缺失或内容非法都返回错误。
func Load(path string) (clientcmdapi.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return clientcmdapi.Config{}, fmt.Errorf("读取 kubeconfig %s 失败: %w", path, err)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return clientcmdapi.Config{}, fmt.Errorf("解析 kubeconfig %s 失败: %w", path, err)
	}
	// clientcmd 对空文件不会报错，会返回一个空配置；这里显式拒绝，
	// 免得后续阶段拿到一份「合法但没有任何条目」的凭据。
	if cfg == nil || (len(cfg.Clusters) == 0 && len(cfg.AuthInfos) == 0 && len(cfg.Contexts) == 0) {
		return clientcmdapi.Config{}, fmt.Errorf("解析 kubeconfig %s 失败: 内容为空", path)
	}
	return *cfg, nil
}
