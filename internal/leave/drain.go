package leave

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// cordonNode 把节点标记为不可调度。
// 驱逐工作负载由运维用 `kubectl drain` 完成：kubemono 不重复实现
// eviction API 的 PDB 重试、镜像 Pod 过滤与 emptydir 处理。
func cordonNode(ctx context.Context, client kubernetes.Interface, nodeName string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if node.Spec.Unschedulable {
			return nil
		}
		node.Spec.Unschedulable = true
		_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		return err
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// warnPodsOnNode 统计节点上仍有工作负载时给出提示（不阻塞退群）。
func warnPodsOnNode(ctx context.Context, opts *Options, nodeName string) {
	list, err := opts.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return
	}
	active := 0
	for _, pod := range list.Items {
		if pod.DeletionTimestamp == nil {
			active++
		}
	}
	if active == 0 {
		return
	}
	opts.Report.Warnf("节点 %s 上仍有 %d 个 Pod；如需优雅驱逐，请先在 master 上执行：kubectl drain %s --ignore-daemonsets --delete-emptydir-data --force",
		nodeName, active, nodeName)
}

// deleteNodeObject 删除 Node 对象；NotFound 视为成功（重跑幂等）。
func deleteNodeObject(ctx context.Context, client kubernetes.Interface, nodeName string) error {
	if client == nil {
		return ErrNoClient
	}
	err := client.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("删除 Node %s 被取消: %w", nodeName, err)
	}
	return fmt.Errorf("删除 Node %s 失败: %w", nodeName, err)
}
