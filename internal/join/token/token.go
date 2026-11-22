// Package token 在已初始化的集群上签发其他节点加入所需的短期凭证。
//
// 一次调用只签发一个角色：
//   - worker：只创建 bootstrap token，不碰 certificateKey 和证书包；
//   - master：额外轮换 certificateKey，重新加密并上传 kube-system/kubemono-certs。
//
// 两类 token 都会同步写入 cluster-info 的 kubeconfig JWS 与 kubemono 集群身份 JWS，
// 因此不需要等待 kube-controller-manager 的 bootstrap-signer 异步补签，
// join 可以在 token create 返回后立即执行。
package token

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	tokenapi "k8s.io/cluster-bootstrap/token/api"

	"kubemono/internal/bootstrap"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/discovery"
	joincerts "kubemono/internal/join/certs"
)

// 集群级资源在 token 签发期间可能被 kube-controller-manager 并发更新
// （bootstrap-signer 会写同一个 cluster-info）。冲突时重读并重试。
const (
	clusterInfoAttempts = 5
	tokenCreateAttempts = 3
)

// Request 是一次 join 凭证签发所需的输入。
type Request struct {
	// Cluster 是集群配置，用于 master 构建证书包（etcd 拓扑等）。
	Cluster *clusterconfig.Cluster
	// Paths 解析本机 PKI 与配置文件路径。
	Paths constants.Paths
	// Client 是管理集群所需的客户端（master 上的 admin kubeconfig）。
	Client kubernetes.Interface
	// Role 是要签发的加入角色；空值等价于 worker。
	Role clusterconfig.Role
	// TTL 是 bootstrap token 的有效期；0 表示默认 2h。
	TTL time.Duration
}

// Material 是一次签发的产物，只对应一个角色、一个 token。
type Material struct {
	Role           clusterconfig.Role
	Token          bootstrap.Token
	ExpiresAt      time.Time
	CertificateKey []byte
	CACertHashes   []string
}

// Issue 签发一个角色的 join 凭证并写入集群。
func Issue(ctx context.Context, req Request) (*Material, error) {
	if req.Client == nil {
		return nil, fmt.Errorf("join-token: 缺少 Kubernetes 客户端（需要 master 上的 admin kubeconfig）")
	}
	if req.Cluster == nil {
		return nil, fmt.Errorf("join-token: 缺少集群配置")
	}
	role, err := normalizeRole(req.Role)
	if err != nil {
		return nil, err
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = constants.DefaultBootstrapTokenTTL
	}
	if ttl < 0 || ttl > constants.MaxBootstrapTokenTTL {
		return nil, fmt.Errorf("bootstrap token TTL %s 超出范围（最大 %s）", ttl, constants.MaxBootstrapTokenTTL)
	}

	clusterID, caPEM, err := readCluster(ctx, req.Client)
	if err != nil {
		return nil, err
	}
	// 本机有 CA 时校验它与 cluster-info 一致：token create 会为 cluster-info 签名，
	// 签名前必须确认自己签的是这台集群的信任根，而不是一份漂移的 ConfigMap。
	// 在堡垒机上只签 worker token 时本机可以没有 CA，此时信任 cluster-info
	// （admin kubeconfig 已经能读整个集群，cluster-info 不是额外的信任边界）。
	localCA, err := readOptionalLocalCA(req.Paths)
	if err != nil {
		return nil, err
	}
	if len(localCA) > 0 && !bytes.Equal(bytes.TrimSpace(localCA), bytes.TrimSpace(caPEM)) {
		return nil, fmt.Errorf("本机 CA 与 %s/%s 里的 CA 不一致：拒绝为未知信任根签发 join 凭证",
			constants.KubePublicNamespace, constants.ClusterInfoName)
	}

	// master 的证书包必须在创建任何集群对象之前构建好：本机缺文件时不应该
	// 留下一个已经注册、却无法解密的 token。
	var certificateKey []byte
	var bundle *joincerts.Bundle
	if role == clusterconfig.RoleMaster {
		certificateKey, _, err = joincerts.GenerateKey()
		if err != nil {
			return nil, err
		}
		bundle, err = joincerts.Build(req.Paths, req.Cluster, clusterID)
		if err != nil {
			return nil, fmt.Errorf("组装 master 证书包: %w", err)
		}
	}

	token, secret, err := createTokenSecret(ctx, req.Client, role, ttl)
	if err != nil {
		return nil, err
	}
	// 后续步骤失败时清掉刚注册的 token：不把“没人知道但已生效”的凭证留在集群里。
	removeToken := func() {
		_ = req.Client.CoreV1().Secrets(secret.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{})
	}

	if role == clusterconfig.RoleMaster {
		if err := joincerts.Upload(ctx, req.Client, bundle, certificateKey); err != nil {
			removeToken()
			return nil, fmt.Errorf("上传 master 证书包: %w", err)
		}
		// 写后校验：避免 Upload 返回成功但 Secret 实际没有更新。
		upToDate, err := joincerts.Uploaded(ctx, req.Client, certificateKey)
		if err != nil {
			removeToken()
			return nil, fmt.Errorf("校验 master 证书包: %w", err)
		}
		if !upToDate {
			removeToken()
			return nil, fmt.Errorf("master 证书包写入后校验失败：%s/%s 与本次 certificateKey 不匹配",
				constants.KubeSystemNamespace, joincerts.SecretName)
		}
	}

	hashes, err := publishSignatures(ctx, req.Client, token, clusterID, caPEM)
	if err != nil {
		removeToken()
		return nil, err
	}

	return &Material{
		Role:           role,
		Token:          token,
		ExpiresAt:      expirationOf(secret, ttl),
		CertificateKey: certificateKey,
		CACertHashes:   hashes,
	}, nil
}

// normalizeRole 把空值当成 worker；其他未知角色直接拒绝。
func normalizeRole(role clusterconfig.Role) (clusterconfig.Role, error) {
	switch role {
	case "":
		return clusterconfig.RoleWorker, nil
	case clusterconfig.RoleWorker, clusterconfig.RoleMaster:
		return role, nil
	default:
		return "", fmt.Errorf("未知的 join 角色 %q（可选 worker、master）", role)
	}
}

// readCluster 读取集群身份与 cluster-info 里公布的 CA。
func readCluster(ctx context.Context, client kubernetes.Interface) (string, []byte, error) {
	state, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).
		Get(ctx, constants.ClusterStateName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil, fmt.Errorf("集群尚未初始化：缺少 %s/%s；请先执行 kubemono init",
			constants.KubeSystemNamespace, constants.ClusterStateName)
	}
	if err != nil {
		return "", nil, fmt.Errorf("读取 %s/%s 失败: %w", constants.KubeSystemNamespace, constants.ClusterStateName, err)
	}
	clusterID := strings.TrimSpace(state.Data["clusterID"])
	if clusterID == "" {
		return "", nil, fmt.Errorf("%s/%s 缺少 clusterID；请先完成 kubemono init",
			constants.KubeSystemNamespace, constants.ClusterStateName)
	}

	cm, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).
		Get(ctx, constants.ClusterInfoName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil, fmt.Errorf("缺少 %s/%s；请先完成 kubemono init",
			constants.KubePublicNamespace, constants.ClusterInfoName)
	}
	if err != nil {
		return "", nil, fmt.Errorf("读取 %s/%s 失败: %w", constants.KubePublicNamespace, constants.ClusterInfoName, err)
	}
	blob := cm.Data[tokenapi.KubeConfigKey]
	if strings.TrimSpace(blob) == "" {
		return "", nil, fmt.Errorf("%s/%s 缺少 %s", constants.KubePublicNamespace, constants.ClusterInfoName, tokenapi.KubeConfigKey)
	}
	info, err := bootstrap.ParseClusterInfoKubeconfig([]byte(blob))
	if err != nil {
		return "", nil, fmt.Errorf("解析 %s/%s 失败: %w", constants.KubePublicNamespace, constants.ClusterInfoName, err)
	}
	if len(info.CA) == 0 {
		return "", nil, fmt.Errorf("%s/%s 没有内嵌集群 CA", constants.KubePublicNamespace, constants.ClusterInfoName)
	}
	return clusterID, info.CA, nil
}

// readOptionalLocalCA 读取本机 CA；不存在不是错误（堡垒机上可以只签 worker token）。
func readOptionalLocalCA(paths constants.Paths) ([]byte, error) {
	data, err := os.ReadFile(paths.CertPath(constants.CACertName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取本机 CA %s 失败: %w", paths.CertPath(constants.CACertName), err)
	}
	return data, nil
}

// createTokenSecret 生成并注册一个 bootstrap token Secret。
// 极低概率的 token ID 冲突会换一个 token 重试，而不是把冲突暴露给操作者。
func createTokenSecret(ctx context.Context, client kubernetes.Interface, role clusterconfig.Role, ttl time.Duration) (bootstrap.Token, *corev1.Secret, error) {
	for attempt := 0; attempt < tokenCreateAttempts; attempt++ {
		token, err := bootstrap.NewToken()
		if err != nil {
			return bootstrap.Token{}, nil, err
		}
		secret, err := bootstrap.NewSecret(bootstrap.SecretConfig{
			Token:       token,
			Role:        role,
			Description: "kubemono join token for " + string(role),
			TTL:         ttl,
			// cluster-info 的 kubeconfig 既由本命令同步签名，也由 KCM 的
			// bootstrap-signer 逐令牌签名；只有带 usage-bootstrap-signing 的
			// 令牌才会被 KCM 处理。master 令牌也必须带上，否则 HA 拓扑下
			// 新 master 会在 JWS 校验处失败。
			Signing: true,
		})
		if err != nil {
			return bootstrap.Token{}, nil, err
		}
		if _, err := client.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return bootstrap.Token{}, nil, fmt.Errorf("注册引导令牌 %s: %w", secret.Name, err)
		}
		return token, secret, nil
	}
	return bootstrap.Token{}, nil, fmt.Errorf("连续 %d 次生成的 bootstrap token ID 都已存在，请重试", tokenCreateAttempts)
}

// publishSignatures 把令牌对应的两类 JWS 写入 cluster-info。
// 每次重试都重新读取当前 kubeconfig 并重新计算签名，避免覆盖 KCM 的并发写入
// 或对旧内容签名。
func publishSignatures(ctx context.Context, client kubernetes.Interface, token bootstrap.Token, clusterID string, expectedCA []byte) ([]string, error) {
	var lastErr error
	for i := 0; i < clusterInfoAttempts; i++ {
		cm, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).
			Get(ctx, constants.ClusterInfoName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("读取 %s/%s 失败: %w", constants.KubePublicNamespace, constants.ClusterInfoName, err)
		}
		blob := cm.Data[tokenapi.KubeConfigKey]
		if strings.TrimSpace(blob) == "" {
			return nil, fmt.Errorf("%s/%s 缺少 %s", constants.KubePublicNamespace, constants.ClusterInfoName, tokenapi.KubeConfigKey)
		}
		info, err := bootstrap.ParseClusterInfoKubeconfig([]byte(blob))
		if err != nil {
			return nil, fmt.Errorf("解析 %s/%s 失败: %w", constants.KubePublicNamespace, constants.ClusterInfoName, err)
		}
		if len(expectedCA) > 0 && !bytes.Equal(bytes.TrimSpace(info.CA), bytes.TrimSpace(expectedCA)) {
			return nil, fmt.Errorf("%s/%s 的 CA 在签发期间发生变化：拒绝为未知信任根签名",
				constants.KubePublicNamespace, constants.ClusterInfoName)
		}
		hashes, err := bootstrap.CACertHashes(info.CA)
		if err != nil {
			return nil, fmt.Errorf("计算 CA pin 失败: %w", err)
		}
		if existing := strings.TrimSpace(cm.Data[discovery.ClusterIDKey]); existing != "" && existing != clusterID {
			return nil, fmt.Errorf("集群身份不一致：%s 是 %s，本机是 %s",
				constants.ClusterInfoName, existing, clusterID)
		}
		kubeconfigSig, err := bootstrap.ComputeDetachedSignature(blob, token)
		if err != nil {
			return nil, fmt.Errorf("为令牌 %s 签 kubeconfig 失败: %w", token.ID, err)
		}
		clusterIDSig, err := bootstrap.ComputeDetachedSignature(clusterID, token)
		if err != nil {
			return nil, fmt.Errorf("为令牌 %s 签集群身份失败: %w", token.ID, err)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[discovery.ClusterIDKey] = clusterID
		cm.Data[discovery.ClusterIDJWSPrefix+token.ID] = clusterIDSig
		cm.Data[tokenapi.JWSSignatureKeyPrefix+token.ID] = kubeconfigSig
		if _, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err == nil {
			return hashes, nil
		} else if !apierrors.IsConflict(err) {
			return nil, fmt.Errorf("更新 %s 失败: %w", constants.ClusterInfoName, err)
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(i+1) * 200 * time.Millisecond)
	}
	return nil, fmt.Errorf("更新 %s 失败（与 kube-controller-manager 反复冲突）: %w", constants.ClusterInfoName, lastErr)
}

// expirationOf 读取 token Secret 里已经写下的精确过期时间；解析失败时回退到 TTL。
func expirationOf(secret *corev1.Secret, ttl time.Duration) time.Time {
	if secret != nil {
		raw := strings.TrimSpace(string(secret.Data[tokenapi.BootstrapTokenExpirationKey]))
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return parsed
		}
	}
	return time.Now().UTC().Add(ttl)
}
