package kubelet

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"kubemono/internal/bootstrap"
)

// NodeClient 报告节点是否已完成入集群。
// kubelet 的就绪有两个信号：Node 对象 Ready（kubelet 已向 apiserver 报到），
// 以及 status.nodeInfo.kubeletVersion 与 BOM 一致（跑的是期望版本）。
// 接口化之后，phase 的测试不需要 apiserver。
type NodeClient interface {
	// NodeReady 报告节点是否存在、是否 Ready，以及是否运行期望的 kubelet 版本。
	NodeReady(ctx context.Context, name string) (bool, error)
	// PendingServingCSRs 统计该节点尚未裁决的 kubelet serving CSR。
	// 审批不是 kubelet 阶段的职责，这里只告警。
	PendingServingCSRs(ctx context.Context) (int, error)
}

// ClientNodeClient 是基于 client-go 的 NodeClient。
type ClientNodeClient struct {
	// Client 是 Kubernetes clientset（来自 admin.conf 或 kubelet.conf）。
	Client kubernetes.Interface
	// KubeletVersion 是 BOM 中的 kubelet 版本，例如 v1.31.4。为空时跳过版本比较。
	KubeletVersion string
	// Node 将 PendingServingCSRs 限制到单个节点；为空时统计所有节点。
	Node string
	// AllowNetworkNotReady 把"仅因 CNI 尚未初始化而 Ready=False"视为已完成。
	// install 的顺序是 kubelet → … → network：kubelet phase
	// 跑完时 CNI 还不存在，节点必然 NotReady（NetworkPluginNotReady）。此时
	// kubelet 本身已经注册、运行正常，等到超时只会让 install 失败；真正的
	// "节点 Ready" 验收在网络层部署之后。
	AllowNetworkNotReady bool
}

// NewNodeClient 返回基于 client-go 的 NodeClient。
func NewNodeClient(client kubernetes.Interface, kubeletVersion string) *ClientNodeClient {
	return &ClientNodeClient{Client: client, KubeletVersion: kubeletVersion}
}

// NodeReady 实现 NodeClient。
func (c *ClientNodeClient) NodeReady(ctx context.Context, name string) (bool, error) {
	if c.Client == nil {
		return false, fmt.Errorf("没有配置 Kubernetes 客户端，无法判断节点 %s 是否 Ready；请先完成 kubeconfig 阶段", name)
	}
	if name == "" {
		return false, fmt.Errorf("节点名为空，无法查询 Node 对象")
	}
	node, err := c.Client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// 节点还没注册，属于“尚未就绪”而不是错误。
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取节点 %s 失败: %w。请确认所用 kubeconfig 有 get nodes 权限，且 apiserver 可达", name, err)
	}
	ready := false
	for _, cond := range node.Status.Conditions {
		if cond.Type != corev1.NodeReady {
			continue
		}
		ready = cond.Status == corev1.ConditionTrue
		if !ready && c.AllowNetworkNotReady && NetworkNotReady(cond) {
			ready = true
		}
	}
	if !ready {
		return false, nil
	}
	if c.KubeletVersion != "" && node.Status.NodeInfo.KubeletVersion != c.KubeletVersion {
		// 版本不符说明运行中的 kubelet 尚未按期望版本重启，按未就绪处理。
		return false, nil
	}
	return true, nil
}

// PendingServingCSRs 实现 NodeClient。
func (c *ClientNodeClient) PendingServingCSRs(ctx context.Context) (int, error) {
	if c.Client == nil {
		return 0, fmt.Errorf("没有配置 Kubernetes 客户端，无法查询待审批的 serving CSR")
	}
	list, err := c.Client.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{
		FieldSelector: "spec.signerName=" + bootstrap.SignerKubeletServing,
	})
	if err != nil {
		return 0, fmt.Errorf("查询 kubelet serving CSR 失败: %w", err)
	}
	count := 0
	for i := range list.Items {
		csr := &list.Items[i]
		// field selector 在真实 apiserver 上已经过滤过；这里再判一次，既防止
		// 客户端/服务端 selector 语义漂移，也让 fake clientset 下的测试成立。
		if csr.Spec.SignerName != bootstrap.SignerKubeletServing {
			continue
		}
		if bootstrap.IsDecided(csr) {
			continue
		}
		if c.Node != "" && csr.Spec.Username != "system:node:"+c.Node {
			continue
		}
		count++
	}
	return count, nil
}

// NetworkNotReady 报告某个 Ready=False 条件是否仅由容器网络（CNI）尚未初始化导致。
// kubelet 的消息形如 "container runtime network not ready: NetworkReady=false
// 其它原因（磁盘压力、PLEG、运行时挂掉）都不算，仍然按未就绪处理。
func NetworkNotReady(cond corev1.NodeCondition) bool {
	if cond.Type != corev1.NodeReady || cond.Status == corev1.ConditionTrue {
		return false
	}
	msg := strings.ToLower(cond.Message)
	return strings.Contains(msg, "networkready=false") ||
		strings.Contains(msg, "networkpluginnotready") ||
		strings.Contains(msg, "cni plugin not initialized") ||
		strings.Contains(msg, "network plugin is not ready")
}
