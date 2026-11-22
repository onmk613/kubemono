package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"kubemono/internal/bootstrap"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/join/certs"
	"kubemono/internal/util/output"
)

// writeBundleSources 在 root 下生成证书包所需的全部文件。
func writeBundleSources(t *testing.T, paths constants.Paths) {
	t.Helper()
	files := map[string]string{
		certs.FileCACert:            "ca",
		certs.FileCAKey:             "ca-key",
		certs.FileFrontProxyCACert:  "fp-ca",
		certs.FileFrontProxyCAKey:   "fp-key",
		certs.FileEtcdCACert:        "etcd-ca",
		certs.FileEtcdCAKey:         "etcd-ca-key",
		certs.FileServiceAccountPub: "sa-pub",
		certs.FileServiceAccountKey: "sa-key",
		certs.FileEncryptionConfig:  "encryption-config",
	}
	for key, content := range files {
		target, err := certs.TargetPath(paths, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFetchCertificateBundleWritesFiles(t *testing.T) {
	src := constants.NewPaths(t.TempDir())
	dst := constants.NewPaths(t.TempDir())
	writeBundleSources(t, src)

	cluster := &clusterconfig.Cluster{}
	bundle, err := certs.Build(src, cluster, "cluster-1")
	if err != nil {
		t.Fatal(err)
	}
	key, encoded, err := certs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := certs.Encrypt(bundle, key)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: certs.SecretName, Namespace: constants.KubeSystemNamespace},
		Data:       map[string][]byte{certs.DataKey: ciphertext},
	}
	client := fake.NewSimpleClientset(secret)

	o := &Options{
		Paths:     dst,
		ClusterID: "cluster-1",
		Report:    &output.Console{Out: io.Discard, Err: io.Discard, MinLevel: output.LevelError},
		JoinSecret: &clusterconfig.JoinSecret{
			Role:           clusterconfig.RoleMaster,
			CertificateKey: encoded,
		},
		ClientFactory: func(bootstrap.Token) (kubernetes.Interface, error) { return client, nil },
	}
	if err := o.fetchCertificateBundle(context.Background(), bootstrap.Token{ID: "abcdef", Secret: "0123456789abcdef"}); err != nil {
		t.Fatalf("fetchCertificateBundle: %v", err)
	}
	caKey, _ := certs.TargetPath(dst, certs.FileCAKey)
	data, err := os.ReadFile(caKey)
	if err != nil {
		t.Fatalf("ca.key 未写入: %v", err)
	}
	if string(data) != "ca-key" {
		t.Fatalf("ca.key 内容 = %q", data)
	}
}

func TestFetchCertificateBundleRejectsClusterMismatch(t *testing.T) {
	src := constants.NewPaths(t.TempDir())
	writeBundleSources(t, src)
	cluster := &clusterconfig.Cluster{}
	bundle, err := certs.Build(src, cluster, "cluster-1")
	if err != nil {
		t.Fatal(err)
	}
	key, encoded, _ := certs.GenerateKey()
	ciphertext, _ := certs.Encrypt(bundle, key)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: certs.SecretName, Namespace: constants.KubeSystemNamespace},
		Data:       map[string][]byte{certs.DataKey: ciphertext},
	}
	client := fake.NewSimpleClientset(secret)
	o := &Options{
		Paths:      constants.NewPaths(t.TempDir()),
		ClusterID:  "another-cluster",
		Report:     &output.Console{Out: io.Discard, Err: io.Discard, MinLevel: output.LevelError},
		JoinSecret: &clusterconfig.JoinSecret{Role: clusterconfig.RoleMaster, CertificateKey: encoded},
		ClientFactory: func(bootstrap.Token) (kubernetes.Interface, error) {
			return client, nil
		},
	}
	if err := o.fetchCertificateBundle(context.Background(), bootstrap.Token{ID: "abcdef", Secret: "0123456789abcdef"}); err == nil {
		t.Fatal("集群 ID 不一致时必须拒绝")
	}
}
