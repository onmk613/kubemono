package token

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	tokenapi "k8s.io/cluster-bootstrap/token/api"

	"kubemono/internal/bootstrap"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/discovery"
	joincerts "kubemono/internal/join/certs"
	"kubemono/internal/kubeconfig"
	"kubemono/internal/pki"
)

func testCA(t *testing.T) []byte {
	t.Helper()
	ca, err := pki.NewCA("kubernetes-ca", "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return pki.EncodeCertPEM(ca.Cert)
}

func testPaths(t *testing.T, caPEM []byte) constants.Paths {
	t.Helper()
	paths := constants.NewPaths(t.TempDir())
	target := paths.CertPath(constants.CACertName)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	return paths
}

func clusterInfo(t *testing.T, caPEM []byte) *corev1.ConfigMap {
	t.Helper()
	blob, err := kubeconfig.Marshal(kubeconfig.Build(kubeconfig.Config{
		User:       constants.UserKubeletBootstrap,
		Server:     "https://10.0.0.1:6443",
		CACertData: caPEM,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: constants.ClusterInfoName, Namespace: constants.KubePublicNamespace},
		Data: map[string]string{
			tokenapi.KubeConfigKey: string(blob),
			discovery.ReleaseKey:   "v1.0.0-test",
		},
	}
}

func stateConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: constants.ClusterStateName, Namespace: constants.KubeSystemNamespace},
		Data:       map[string]string{"clusterID": "cid-1"},
	}
}

// writeBundleFiles 写全 master 证书包所需的文件；ca.crt 使用集群真实 CA，
// 因为 token 包会校验本机 CA 与 cluster-info 一致。
func writeBundleFiles(t *testing.T, paths constants.Paths, caPEM []byte) {
	t.Helper()
	files := map[string][]byte{
		joincerts.FileCACert:            caPEM,
		joincerts.FileCAKey:             []byte("ca-key"),
		joincerts.FileFrontProxyCACert:  []byte("fp-ca"),
		joincerts.FileFrontProxyCAKey:   []byte("fp-key"),
		joincerts.FileEtcdCACert:        []byte("etcd-ca"),
		joincerts.FileEtcdCAKey:         []byte("etcd-ca-key"),
		joincerts.FileServiceAccountPub: []byte("sa-pub"),
		joincerts.FileServiceAccountKey: []byte("sa-key"),
		joincerts.FileEncryptionConfig:  []byte("apiVersion: apiserver.config.k8s.io/v1\nkind: EncryptionConfiguration\n"),
	}
	for key, content := range files {
		target, err := joincerts.TargetPath(paths, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIssueWorkerOnlyCreatesTokenAndSignatures(t *testing.T) {
	ctx := context.Background()
	caPEM := testCA(t)
	paths := testPaths(t, caPEM)
	client := fake.NewSimpleClientset(stateConfigMap(), clusterInfo(t, caPEM),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: joincerts.SecretName, Namespace: constants.KubeSystemNamespace},
			Data:       map[string][]byte{joincerts.DataKey: []byte("old-bundle")},
		})

	// Role 留空：与 `kubemono token create` 的默认行为一致，应签发 worker。
	m, err := Issue(ctx, Request{
		Cluster: clusterconfig.DefaultCluster(),
		Paths:   paths,
		Client:  client,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if m.Role != clusterconfig.RoleWorker {
		t.Fatalf("默认角色 = %s, want worker", m.Role)
	}
	if len(m.CertificateKey) != 0 {
		t.Fatal("worker 不应生成 certificateKey")
	}
	if !slices.Equal(mustHashes(t, caPEM), m.CACertHashes) {
		t.Fatalf("CA pin = %v", m.CACertHashes)
	}
	if remaining := time.Until(m.ExpiresAt); remaining < constants.DefaultBootstrapTokenTTL-time.Minute ||
		remaining > constants.DefaultBootstrapTokenTTL+time.Minute {
		t.Fatalf("默认 TTL 剩余 %s，期望约 %s", remaining, constants.DefaultBootstrapTokenTTL)
	}

	secret, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).
		Get(ctx, bootstrap.SecretName(m.Token.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("bootstrap token Secret 未写入: %v", err)
	}
	if got := string(secret.Data[tokenapi.BootstrapTokenExtraGroupsKey]); got != constants.GroupBootstrappersWorker {
		t.Fatalf("worker 令牌分组 = %q", got)
	}
	if got := string(secret.Data[tokenapi.BootstrapTokenUsageSigningKey]); got != "true" {
		t.Fatal("令牌必须带 usage-bootstrap-signing")
	}

	// worker 不得碰证书包。
	bundle, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).
		Get(ctx, joincerts.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(bundle.Data[joincerts.DataKey]) != "old-bundle" {
		t.Fatal("worker token 不应改写 kubemono-certs")
	}

	// JWS 必须在 Issue 返回时就可校验，不能等 KCM 异步补签。
	cm, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).
		Get(ctx, constants.ClusterInfoName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := bootstrap.ParseClusterInfoConfigMap(cm, m.Token)
	if err != nil {
		t.Fatalf("cluster-info JWS 校验失败: %v", err)
	}
	if !bytes.Equal(info.CA, caPEM) {
		t.Fatal("cluster-info 的 CA 不一致")
	}
	if cm.Data[discovery.ClusterIDKey] != "cid-1" {
		t.Fatalf("clusterID = %q", cm.Data[discovery.ClusterIDKey])
	}
	if !bootstrap.VerifyDetachedSignature(cm.Data[discovery.ClusterIDJWSPrefix+m.Token.ID], "cid-1", m.Token) {
		t.Fatal("集群身份 JWS 校验失败")
	}
}

func TestIssueMasterRotatesCertificateBundle(t *testing.T) {
	ctx := context.Background()
	caPEM := testCA(t)
	paths := testPaths(t, caPEM)
	writeBundleFiles(t, paths, caPEM)
	client := fake.NewSimpleClientset(stateConfigMap(), clusterInfo(t, caPEM),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: joincerts.SecretName, Namespace: constants.KubeSystemNamespace},
			Data:       map[string][]byte{joincerts.DataKey: []byte("old-bundle")},
		})

	m, err := Issue(ctx, Request{
		Cluster: clusterconfig.DefaultCluster(),
		Paths:   paths,
		Client:  client,
		Role:    clusterconfig.RoleMaster,
		TTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if m.Role != clusterconfig.RoleMaster {
		t.Fatalf("角色 = %s", m.Role)
	}
	if len(m.CertificateKey) != joincerts.KeySize {
		t.Fatalf("certificateKey 长度 = %d", len(m.CertificateKey))
	}

	secret, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).
		Get(ctx, joincerts.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("证书包未写入: %v", err)
	}
	if secret.Annotations["kubemono.io/cert-key-hash"] != joincerts.KeyFingerprint(m.CertificateKey) {
		t.Fatal("证书包没有绑定本次 certificateKey")
	}
	bundle, err := joincerts.Decrypt(secret.Data[joincerts.DataKey], m.CertificateKey)
	if err != nil {
		t.Fatalf("解密证书包: %v", err)
	}
	if _, ok := bundle.Files[joincerts.FileCAKey]; !ok {
		t.Fatal("证书包缺少 ca.key")
	}

	tokenSecret, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).
		Get(ctx, bootstrap.SecretName(m.Token.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(tokenSecret.Data[tokenapi.BootstrapTokenExtraGroupsKey]); got != constants.GroupBootstrappersMaster {
		t.Fatalf("master 令牌分组 = %q", got)
	}

	cm, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).
		Get(ctx, constants.ClusterInfoName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.ParseClusterInfoConfigMap(cm, m.Token); err != nil {
		t.Fatalf("cluster-info JWS 校验失败: %v", err)
	}
}

func TestIssueRejectsTTLAboveMaximum(t *testing.T) {
	caPEM := testCA(t)
	client := fake.NewSimpleClientset(stateConfigMap(), clusterInfo(t, caPEM))
	_, err := Issue(context.Background(), Request{
		Cluster: clusterconfig.DefaultCluster(),
		Paths:   testPaths(t, caPEM),
		Client:  client,
		TTL:     constants.MaxBootstrapTokenTTL + time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "TTL") {
		t.Fatalf("应拒绝超过上限的 TTL，got %v", err)
	}
	secrets, listErr := client.CoreV1().Secrets(constants.KubeSystemNamespace).List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("TTL 校验失败时不应写入 Secret: %d", len(secrets.Items))
	}
}

func TestIssueRejectsUnknownRole(t *testing.T) {
	_, err := Issue(context.Background(), Request{
		Cluster: clusterconfig.DefaultCluster(),
		Client:  fake.NewSimpleClientset(),
		Role:    clusterconfig.Role("etcd"),
	})
	if err == nil || !strings.Contains(err.Error(), "角色") {
		t.Fatalf("未知角色必须报错，got %v", err)
	}
}

func TestIssueRejectsCAMismatch(t *testing.T) {
	localCA := testCA(t)
	clusterCA := testCA(t)
	client := fake.NewSimpleClientset(stateConfigMap(), clusterInfo(t, clusterCA))
	_, err := Issue(context.Background(), Request{
		Cluster: clusterconfig.DefaultCluster(),
		Paths:   testPaths(t, localCA),
		Client:  client,
	})
	if err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("本机 CA 与 cluster-info 不一致时必须拒绝，got %v", err)
	}
	if _, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).
		Get(context.Background(), "bootstrap-token-anything", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("拒绝签发时不应写 token Secret: %v", err)
	}
}

func TestIssueRequiresInitializedCluster(t *testing.T) {
	_, err := Issue(context.Background(), Request{
		Cluster: clusterconfig.DefaultCluster(),
		Paths:   constants.NewPaths(t.TempDir()),
		Client:  fake.NewSimpleClientset(),
	})
	if err == nil || !strings.Contains(err.Error(), "kubemono init") {
		t.Fatalf("未初始化集群必须提示 init，got %v", err)
	}
}

func mustHashes(t *testing.T, caPEM []byte) []string {
	t.Helper()
	hashes, err := bootstrap.CACertHashes(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}
