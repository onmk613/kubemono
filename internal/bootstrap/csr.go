package bootstrap

import certificatesv1 "k8s.io/api/certificates/v1"

// 已知的 signer 名称。
const (
	// SignerKubeletServing 签发 kubelet 服务端证书。
	SignerKubeletServing = "kubernetes.io/kubelet-serving"
	// SignerKubeletClient 签发 kubelet 客户端证书（自动批准）。
	SignerKubeletClient = "kubernetes.io/kubelet-client"
)

// IsDecided 报告 CSR 是否已经有最终状态。
func IsDecided(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certificatesv1.CertificateApproved || c.Type == certificatesv1.CertificateDenied || c.Type == certificatesv1.CertificateFailed {
			return true
		}
	}
	return false
}
