package discovery

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"kubemono/internal/constants"
)

// ClientReader 用 client-go 读取 cluster-info。
// 首次读取时还没有可信的 CA：TLS 不校验，cluster-info 的真实性由引导令牌
// 的 JWS 保证；读到并校验 CA 后，后续交换再使用该 CA。
type ClientReader struct {
	// Token 用于认证请求；cluster-info 在引导阶段需要认证。
	Token string
	// Timeout 限定单次请求的时长。
	Timeout time.Duration
	// NewClient 可注入，便于测试。
	NewClient func(cfg *rest.Config) (kubernetes.Interface, error)
}

// NewClientReader 返回带指定超时的 reader。
func NewClientReader(timeout time.Duration, token string) *ClientReader {
	return &ClientReader{Timeout: timeout, Token: token}
}

// ClusterInfo 实现 ConfigMapReader。
func (r *ClientReader) ClusterInfo(ctx context.Context, server string) (*corev1.ConfigMap, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cfg := &rest.Config{
		Host:        server,
		Timeout:     timeout,
		BearerToken: r.Token,
	}
	// 此时还不知道该信任谁：首跳只能先信任 JWS，读到 CA 后再切换。
	cfg.Insecure = true
	newClient := r.NewClient
	if newClient == nil {
		newClient = func(cfg *rest.Config) (kubernetes.Interface, error) { return kubernetes.NewForConfig(cfg) }
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", server, err)
	}
	cm, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).Get(ctx, constants.ClusterInfoName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("从 %s 读取 %s/%s 失败: %w", server, constants.KubePublicNamespace, constants.ClusterInfoName, err)
	}
	return cm, nil
}
