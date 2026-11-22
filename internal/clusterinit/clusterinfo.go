package clusterinit

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/discovery"
	"kubemono/internal/kubeconfig"
)

// clusterInfoConfigMap 渲染 kube-public/cluster-info 的静态部分。
// 这个 ConfigMap 是加入方唯一的"集群自述"：kubeconfig 原文（含 CA data 与
// server）、release，以及 init 节点写入的 clusterID（不带签名）。逐令牌的
// kubeconfig JWS 与集群身份 JWS 由 internal/join/token 在签发凭证时同步写入；
// 没有令牌时不会有人校验未签名的 clusterID，因此 init 阶段写它是安全的。
func clusterInfoConfigMap(opts Options) (*corev1.ConfigMap, error) {
	caPEM := opts.CACertPEM
	if len(caPEM) == 0 {
		path := opts.Paths.CertPath(constants.CACertName)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cluster-info 必须内嵌集群 CA，但读取 %s 失败: %w", path, err)
		}
		caPEM = data
	}
	server, err := clusterInfoServer(opts)
	if err != nil {
		return nil, err
	}
	blob, err := bootstrapKubeconfigBlob(caPEM, server)
	if err != nil {
		return nil, err
	}

	// cluster-info 发布加入方需要的信任起点（kubeconfig、release）。
	// clusterID 由 init 节点写入（不带签名）；每个令牌的 kubeconfig JWS 和
	// 集群身份 JWS 由 kubemono token create 在签发时一起写入。没有令牌时
	// 不会有人用未签名的 clusterID 做校验，因此 init 阶段写它是安全的。
	data := map[string]string{"kubeconfig": string(blob)}
	if opts.Release != "" {
		data[discovery.ReleaseKey] = opts.Release
	}
	if opts.PublishClusterID && opts.ClusterID != "" {
		data[discovery.ClusterIDKey] = opts.ClusterID
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.ClusterInfoName,
			Namespace: constants.KubePublicNamespace,
		},
		Data: data,
	}, nil
}

// clusterInfoServer 决定 cluster-info 里公布的 apiserver 地址。
// 优先 externalEndpoints（外部可达），否则用名字最小的 master 的地址——
// 加入方会用这个地址建立连接，因此绝不能是 127.0.0.1。
func clusterInfoServer(opts Options) (string, error) {
	if opts.Cluster == nil {
		return "", fmt.Errorf("clusterinit: 缺少集群配置")
	}
	if ep, ok := opts.Cluster.ExternalEndpointsAPIEndpoint(constants.APIServerPort); ok {
		return ep, nil
	}
	masters := opts.Cluster.Cluster.Hosts.Masters()
	if len(masters) == 0 {
		return "", fmt.Errorf("clusterinit: 集群配置里没有 master，无法公布 apiserver 地址")
	}
	sort.Slice(masters, func(i, j int) bool { return masters[i].Name < masters[j].Name })
	ep, ok := clusterconfig.NormalizeEndpoint(net.JoinHostPort(masters[0].Address, strconv.Itoa(constants.APIServerPort)), constants.APIServerPort)
	if !ok {
		return "", fmt.Errorf("clusterinit: master %s 没有可用地址，无法公布 apiserver 地址", masters[0].Name)
	}
	return ep, nil
}

// bootstrapKubeconfigBlob 构建加入方用的初始 kubeconfig（内嵌 CA data）。
// 用户是 kubelet-bootstrap + 引导令牌：能做什么完全由 RBAC 决定，
// 这里只保证"能读到 cluster-info 并申请证书"。
func bootstrapKubeconfigBlob(caPEM []byte, server string) ([]byte, error) {
	// 令牌由加入方自己填入（cluster-info 里不放任何令牌明文）；这里只固定
	// 身份与 CA。条目命名交给 internal/kubeconfig，避免第三份手写 kubeconfig。
	blob, err := kubeconfig.Marshal(kubeconfig.Build(kubeconfig.Config{
		User:       constants.UserKubeletBootstrap,
		Server:     server,
		CACertData: caPEM,
	}))
	if err != nil {
		return nil, fmt.Errorf("cluster-info: %w", err)
	}
	return blob, nil
}
