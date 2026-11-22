package bootstrap

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	tokenapi "k8s.io/cluster-bootstrap/token/api"
	jws "k8s.io/cluster-bootstrap/token/jws"

	"kubemono/internal/pki"
)

// ClusterInfo 是加入节点从 kube-public/cluster-info 学到的内容。
// 关键安全属性：CA 只有在用引导令牌验证了 cluster-info 的
// JWS 签名之后才可信；join 凭证里附带的 caCertHashes 提供第二次锁定。
type ClusterInfo struct {
	// Server 是集群公布的 apiserver 端点。
	Server string
	// CA 是集群的 PEM 编码证书颁发机构。
	CA []byte
}

// ParseClusterInfoConfigMap 提取并校验 cluster-info。
// token 必须携带 id 与 secret：secret 用于校验 JWS 签名，
// 因此“只拿到 cluster-info 的 MitM”无法伪造 CA。
func ParseClusterInfoConfigMap(cm *corev1.ConfigMap, token Token) (*ClusterInfo, error) {
	if cm == nil {
		return nil, fmt.Errorf("cluster-info ConfigMap is missing")
	}
	raw, ok := cm.Data[tokenapi.KubeConfigKey]
	if !ok || raw == "" {
		return nil, fmt.Errorf("cluster-info ConfigMap has no %q key", tokenapi.KubeConfigKey)
	}
	ci, err := ParseClusterInfoKubeconfig([]byte(raw))
	if err != nil {
		return nil, err
	}
	sigKey := tokenapi.JWSSignatureKeyPrefix + token.ID
	sig, ok := cm.Data[sigKey]
	if !ok {
		return nil, fmt.Errorf("cluster-info ConfigMap has no signature for token %s (key %s)", token.ID, sigKey)
	}
	// 与上游一致：签名覆盖的是 **kubeconfig 原文**（不是 CA PEM）。
	// 这样 server 地址与 CA 一起被绑定，中间人既不能换 CA 也不能改指向。
	if !jws.DetachedTokenIsValid(sig, raw, token.ID, token.Secret) {
		return nil, fmt.Errorf("cluster-info signature is invalid for bootstrap token %s: refusing to trust the advertised CA", token.ID)
	}
	return ci, nil
}

// VerifyDetachedSignature 校验用引导令牌生成的分离 JWS。
// cluster-info 里除了 kubeconfig blob 之外的 kubemono 自有字段（例如集群身份）
// 也必须签名，否则拿到 ConfigMap 的攻击者可以随意伪造它们。
func VerifyDetachedSignature(signature, content string, token Token) bool {
	return jws.DetachedTokenIsValid(signature, content, token.ID, token.Secret)
}

// ComputeDetachedSignature 用引导令牌对 content 签名。
func ComputeDetachedSignature(content string, token Token) (string, error) {
	return jws.ComputeDetachedSignature(content, token.ID, token.Secret)
}

// ParseClusterInfoKubeconfig 解析 cluster-info 的 kubeconfig 内容。
func ParseClusterInfoKubeconfig(data []byte) (*ClusterInfo, error) {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("parse cluster-info kubeconfig: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("cluster-info kubeconfig has no cluster entry")
	}
	ci := &ClusterInfo{}
	for _, c := range cfg.Clusters {
		ci.Server = c.Server
		ci.CA = c.CertificateAuthorityData
		break
	}
	if ci.Server == "" {
		return nil, fmt.Errorf("cluster-info kubeconfig has no server address")
	}
	if len(ci.CA) == 0 {
		return nil, fmt.Errorf("cluster-info kubeconfig has no certificate-authority-data")
	}
	return ci, nil
}

// PinHash 返回证书公钥的 "sha256:<hex>" pin。
// 对 SubjectPublicKeyInfo 求 sha256，因此同一密钥重签证书
// 不会导致 pin 失效。
func PinHash(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CACertHashes 返回 PEM bundle 中每张证书的 pin。
func CACertHashes(caPEM []byte) ([]string, error) {
	// 复用 pki 的 PEM 解析原语：自造“逐块解码”会让两份实现对非法输入的
	// 容忍度漂移，统一到一处后没有这个风险。
	certs, err := pki.ParseCertsPEM(caPEM)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(certs))
	for _, c := range certs {
		out = append(out, PinHash(c))
	}
	return out, nil
}

// VerifyCACertHashes 执行 join 凭证可选的二次锁定。
// 空 hashes 表示“没有二次锁定”，此时只依赖 JWS。
func VerifyCACertHashes(caPEM []byte, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	actual, err := CACertHashes(caPEM)
	if err != nil {
		return err
	}
	for _, want := range hashes {
		for _, got := range actual {
			if strings.EqualFold(strings.TrimSpace(want), got) {
				return nil
			}
		}
	}
	return fmt.Errorf("the CA advertised by the cluster does not match any caCertHash in the join credential: got %v, want one of %v", actual, hashes)
}
