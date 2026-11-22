package certs

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"kubemono/internal/constants"
)

// SecretName 是存放加密证书包的 Secret 名称。
const SecretName = "kubemono-certs"

// DataKey 是 Secret 中密文的键。
const DataKey = "certificates"

// Upload 加密证书包并写入 kube-system/kubemono-certs。
// 已存在时覆盖：master 凭证签发（kubemono token create --role master）会轮换
// certificateKey，已有的 master join 命令随即失效。
func Upload(ctx context.Context, client kubernetes.Interface, bundle *Bundle, key []byte) error {
	if client == nil {
		return fmt.Errorf("join-certs: 缺少 Kubernetes 客户端")
	}
	ciphertext, err := Encrypt(bundle, key)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        SecretName,
			Namespace:   constants.KubeSystemNamespace,
			Annotations: map[string]string{"kubemono.io/cert-key-hash": KeyFingerprint(key)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{DataKey: ciphertext},
	}
	existing, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).Get(ctx, SecretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("创建证书 Secret %s: %w", SecretName, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("读取证书 Secret %s: %w", SecretName, err)
	}
	existing.Data = secret.Data
	existing.Annotations = secret.Annotations
	if _, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("更新证书 Secret %s: %w", SecretName, err)
	}
	return nil
}

// Uploaded 报告集群里的证书包是否由该 certificateKey 加密。
// token 签发在 Upload 之后用它做写后校验，避免“返回成功但 Secret 未更新”。
func Uploaded(ctx context.Context, client kubernetes.Interface, key []byte) (bool, error) {
	secret, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).Get(ctx, SecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if secret.Annotations["kubemono.io/cert-key-hash"] != KeyFingerprint(key) {
		return false, nil
	}
	return len(secret.Data[DataKey]) > 0, nil
}

// Download 读取 kube-system/kubemono-certs 的密文。
func Download(ctx context.Context, client kubernetes.Interface) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("join-certs: 缺少 Kubernetes 客户端")
	}
	secret, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).Get(ctx, SecretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("集群里没有 %s/%s：master 加入需要先在 master 上执行 kubemono token create --role master", constants.KubeSystemNamespace, SecretName)
		}
		if apierrors.IsForbidden(err) {
			return nil, fmt.Errorf("没有权限读取 %s/%s：请确认使用的是 master 角色（--control-plane；worker 令牌无权读取证书包）", constants.KubeSystemNamespace, SecretName)
		}
		return nil, fmt.Errorf("读取证书 Secret %s: %w", SecretName, err)
	}
	data := secret.Data[DataKey]
	if len(data) == 0 {
		return nil, fmt.Errorf("%s/%s 缺少 %s", constants.KubeSystemNamespace, SecretName, DataKey)
	}
	return data, nil
}
