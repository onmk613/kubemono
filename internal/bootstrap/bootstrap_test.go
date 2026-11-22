package bootstrap

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	tokenapi "k8s.io/cluster-bootstrap/token/api"
	jws "k8s.io/cluster-bootstrap/token/jws"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/pki"
)

func TestTokenGenerateParseRoundTrip(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	parsed, err := ParseToken(tok.String())
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if parsed != tok {
		t.Fatalf("round trip mismatch: %v vs %v", parsed, tok)
	}
	for _, bad := range []string{"", "abc", "1234567890123456789012", "ABCDEF.0123456789abcdef", "abcdef.0123456789abcde"} {
		if _, err := ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) must fail", bad)
		}
	}
}

func TestNewSecretPerRole(t *testing.T) {
	tok := Token{ID: "abcdef", Secret: "0123456789abcdef"}
	master, err := NewSecret(SecretConfig{Token: tok, Role: clusterconfig.RoleMaster, TTL: time.Hour, Signing: true})
	if err != nil {
		t.Fatalf("NewSecret(master): %v", err)
	}
	if master.Name != constants.BootstrapTokenSecretPrefix+"abcdef" || master.Namespace != constants.KubeSystemNamespace {
		t.Fatalf("unexpected secret identity: %s/%s", master.Namespace, master.Name)
	}
	if master.Type != tokenapi.SecretTypeBootstrapToken {
		t.Fatalf("unexpected secret type %s", master.Type)
	}
	if string(master.Data[tokenapi.BootstrapTokenExtraGroupsKey]) != constants.GroupBootstrappersMaster {
		t.Fatalf("master token group = %s", master.Data[tokenapi.BootstrapTokenExtraGroupsKey])
	}
	if string(master.Data[tokenapi.BootstrapTokenUsageSigningKey]) != "true" {
		t.Fatal("signing usage must be set when requested")
	}

	worker, err := NewSecret(SecretConfig{Token: tok, Role: clusterconfig.RoleWorker})
	if err != nil {
		t.Fatalf("NewSecret(worker): %v", err)
	}
	if string(worker.Data[tokenapi.BootstrapTokenExtraGroupsKey]) != constants.GroupBootstrappersWorker {
		t.Fatalf("worker token group = %s", worker.Data[tokenapi.BootstrapTokenExtraGroupsKey])
	}
	if _, err := NewSecret(SecretConfig{Token: tok, Role: "bogus"}); err == nil {
		t.Fatal("unknown role must be rejected")
	}
}

// --- cluster-info 发现与 CA 锁定 ---
func TestParseClusterInfoConfigMapVerifiesJWS(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	caPEM := testCAPEM(t)
	kubeconfigData := testClusterInfoKubeconfig(t, caPEM)

	// 签名覆盖 kubeconfig 原文。
	sig, err := jws.ComputeDetachedSignature(string(kubeconfigData), tok.ID, tok.Secret)
	if err != nil {
		t.Fatalf("ComputeDetachedSignature: %v", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: constants.ClusterInfoName, Namespace: constants.KubePublicNamespace},
		Data: map[string]string{
			tokenapi.KubeConfigKey:                  string(kubeconfigData),
			tokenapi.JWSSignatureKeyPrefix + tok.ID: sig,
		},
	}

	ci, err := ParseClusterInfoConfigMap(cm, tok)
	if err != nil {
		t.Fatalf("ParseClusterInfoConfigMap: %v", err)
	}
	if ci.Server != "https://10.0.0.1:6443" {
		t.Fatalf("server = %s", ci.Server)
	}
	if len(ci.CA) == 0 {
		t.Fatal("CA must be parsed")
	}

	// 篡改签名：必须拒绝，而不是信任被篡改的 CA。
	tampered := cm.DeepCopy()
	tampered.Data[tokenapi.JWSSignatureKeyPrefix+tok.ID] = sig[:len(sig)-4] + "AAAA"
	if _, err := ParseClusterInfoConfigMap(tampered, tok); err == nil {
		t.Fatal("tampered JWS must be rejected")
	}
	// 用别的令牌 secret 验签也必须失败。
	other := Token{ID: tok.ID, Secret: "ffffffffffffffff"}
	if _, err := ParseClusterInfoConfigMap(cm, other); err == nil {
		t.Fatal("wrong token secret must fail JWS verification")
	}
	// 缺少签名键。
	missing := cm.DeepCopy()
	delete(missing.Data, tokenapi.JWSSignatureKeyPrefix+tok.ID)
	if _, err := ParseClusterInfoConfigMap(missing, tok); err == nil {
		t.Fatal("missing signature must be rejected")
	}
	// 缺少 kubeconfig。
	empty := &corev1.ConfigMap{Data: map[string]string{}}
	if _, err := ParseClusterInfoConfigMap(empty, tok); err == nil {
		t.Fatal("empty cluster-info must be rejected")
	}
	if _, err := ParseClusterInfoConfigMap(nil, tok); err == nil {
		t.Fatal("nil cluster-info must be rejected")
	}
}

func TestVerifyCACertHashes(t *testing.T) {
	caPEM := testCAPEM(t)
	hashes, err := CACertHashes(caPEM)
	if err != nil {
		t.Fatalf("CACertHashes: %v", err)
	}
	if len(hashes) != 1 || len(hashes[0]) != len("sha256:")+64 {
		t.Fatalf("unexpected hashes: %v", hashes)
	}
	if err := VerifyCACertHashes(caPEM, hashes); err != nil {
		t.Fatalf("matching pin must pass: %v", err)
	}
	if err := VerifyCACertHashes(caPEM, nil); err != nil {
		t.Fatalf("no pin means no second lock: %v", err)
	}
	bad := []string{"sha256:0000000000000000000000000000000000000000000000000000000000000000"}
	if err := VerifyCACertHashes(caPEM, bad); err == nil {
		t.Fatal("non-matching pin must be refused")
	}
	// pin 与密钥绑定：同一密钥重签证书后 pin 不变。
	first := hashes[0]
	cert := parseFirstCert(t, caPEM)
	if PinHash(cert) != first {
		t.Fatal("PinHash must be stable for a certificate")
	}
}

// --- CSR 审批策略 ---
func TestIsDecided(t *testing.T) {
	csr := certificatesCSR()
	if IsDecided(&csr) {
		t.Fatal("fresh CSR must not be decided")
	}
	csr.Status.Conditions = append(csr.Status.Conditions, certificateApprovedCondition())
	if !IsDecided(&csr) {
		t.Fatal("approved CSR must be reported as decided")
	}
}

// --- 辅助函数 ---
func certificatesCSR() certificatesv1.CertificateSigningRequest {
	return certificatesv1.CertificateSigningRequest{}
}

func certificateApprovedCondition() certificatesv1.CertificateSigningRequestCondition {
	return certificatesv1.CertificateSigningRequestCondition{
		Type:    certificatesv1.CertificateApproved,
		Status:  corev1.ConditionTrue,
		Reason:  "KubemonoApprove",
		Message: "test",
	}
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	ca, err := pki.NewCA("kubernetes-ca", "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return pki.EncodeCertPEM(ca.Cert)
}

func parseFirstCert(t *testing.T, pemData []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemData)
	if block == nil {
		t.Fatal("no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func testClusterInfoKubeconfig(t *testing.T, caPEM []byte) []byte {
	t.Helper()
	cfg := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"kubernetes": {Server: "https://10.0.0.1:6443", CertificateAuthorityData: caPEM},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"kubelet-bootstrap": {Token: "abcdef.0123456789abcdef"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"kubernetes": {Cluster: "kubernetes", AuthInfo: "kubelet-bootstrap"},
		},
		CurrentContext: "kubernetes",
	}
	data, err := clientcmd.Write(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
