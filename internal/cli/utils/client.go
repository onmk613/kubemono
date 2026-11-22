// Package utils 是命令层共用的薄工具：kubeconfig/client 构造、表格输出、
// 破坏性操作确认与集群配置读取。
// 它只做"把 CLI 参数接到 internal 层"的粘合工作，不放业务逻辑；测试可以通过
// 包级变量注入假的 client（见 NewClusterClient）。
package utils

import (
	"fmt"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"

	"kubemono/internal/cli/options"
)

// KubernetesClient 从 kubeconfig 文件构造 clientset。
func KubernetesClient(path string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("读取 kubeconfig %s: %w", path, err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("构造 Kubernetes 客户端: %w", err)
	}
	return client, nil
}

// NewClusterClient 构造集群级命令使用的 Kubernetes 客户端。
// 顺序：--kubeconfig → 本机 /etc/kubernetes/admin.conf。--kubeconfig 让
// reset --leave 在本机 apiserver 已损坏时仍能通过远端 master 退群；
// 堡垒机场景（把 kubemono 放在集群外、用导出的 operator kubeconfig 执行只读
// 命令）也依赖它。
// 单元测试通过替换这个变量注入 fake clientset。
var NewClusterClient = func(opts *options.GlobalOptions) (kubernetes.Interface, error) {
	path := strings.TrimSpace(opts.KubeconfigPath)
	if path == "" {
		path = opts.Paths().KubeconfigPath(constants.AdminKubeconfigName)
	}
	if !fsutil.FileExists(path) {
		return nil, fmt.Errorf("需要 master 上的 admin kubeconfig：%s 不存在；集群级命令只能在 master 上以 root 执行，或用 --kubeconfig 指定", path)
	}
	client, err := KubernetesClient(path)
	if err != nil {
		return nil, fmt.Errorf("需要 master 上的 admin kubeconfig：%w", err)
	}
	return client, nil
}

// InstallClient 返回本节点上可用于 `kubemono init` / `kubemono join` 集群级阶段的最佳凭据。
// 顺序：--kubeconfig → admin.conf（master，kubeconfig phase 写出）→ kubelet.conf
// （worker，TLS Bootstrap 的产物，身份是 system:node:<name>）。
func InstallClient(opts *options.GlobalOptions) (kubernetes.Interface, error) {
	client, err := NewClusterClient(opts)
	if err == nil {
		return client, nil
	}
	if opts.KubeconfigPath == "" {
		kubelet := opts.Paths().KubeconfigPath(constants.KubeletKubeconfigName)
		if fsutil.FileExists(kubelet) {
			return KubernetesClient(kubelet)
		}
	}
	return nil, err
}
