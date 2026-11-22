package certs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/join/certs"
)

func testPaths(t *testing.T) constants.Paths {
	t.Helper()
	return constants.NewPaths(t.TempDir())
}

func writeRequired(t *testing.T, paths constants.Paths) {
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
			t.Fatalf("TargetPath(%s): %v", key, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func stackedCluster() *clusterconfig.Cluster {
	c := &clusterconfig.Cluster{}
	return c
}

func TestBuildAndWriteRoundTrip(t *testing.T) {
	src := testPaths(t)
	writeRequired(t, src)
	bundle, err := certs.Build(src, stackedCluster(), "cluster-1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	dst := testPaths(t)
	if err := bundle.WriteTo(dst, false, nil); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	for _, key := range []string{certs.FileCACert, certs.FileCAKey, certs.FileEtcdCAKey} {
		target, _ := certs.TargetPath(dst, key)
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read %s: %v", target, err)
		}
		want, _ := bundle.Files[key]
		if string(got) != string(want) {
			t.Errorf("%s 内容不一致", key)
		}
	}
	// 私钥必须是 0600。
	keyPath, _ := certs.TargetPath(dst, certs.FileCAKey)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("ca.key 权限 = %04o, want 0600", info.Mode().Perm())
	}
}

func TestBuildMissingRequiredFileFails(t *testing.T) {
	paths := testPaths(t)
	cluster := stackedCluster()
	if _, err := certs.Build(paths, cluster, "c"); err == nil {
		t.Fatal("缺少 CA 时 Build 必须失败")
	}
}

func TestCAKeyConflictRequiresForce(t *testing.T) {
	src := testPaths(t)
	writeRequired(t, src)
	bundle, err := certs.Build(src, stackedCluster(), "c")
	if err != nil {
		t.Fatal(err)
	}
	dst := testPaths(t)
	caKey, _ := certs.TargetPath(dst, certs.FileCAKey)
	if err := os.MkdirAll(filepath.Dir(caKey), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caKey, []byte("another-ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bundle.WriteTo(dst, false, nil); !errors.Is(err, certs.ErrCAKeyConflict) {
		t.Fatalf("err = %v, want ErrCAKeyConflict", err)
	}
	if err := bundle.WriteTo(dst, true, nil); err != nil {
		t.Fatalf("--force 应允许覆盖: %v", err)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	src := testPaths(t)
	writeRequired(t, src)
	bundle, err := certs.Build(src, stackedCluster(), "c")
	if err != nil {
		t.Fatal(err)
	}
	key, encoded, err := certs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := certs.DecodeKey(encoded)
	if err != nil || string(decoded) != string(key) {
		t.Fatalf("DecodeKey round trip: %v", err)
	}
	ciphertext, err := certs.Encrypt(bundle, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := certs.Decrypt(ciphertext, key)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got.ClusterID != "c" || len(got.Files) != len(bundle.Files) {
		t.Fatalf("解密结果不一致: %+v", got)
	}
}

func TestDecryptWrongKeyOrTamperFails(t *testing.T) {
	src := testPaths(t)
	writeRequired(t, src)
	bundle, err := certs.Build(src, stackedCluster(), "c")
	if err != nil {
		t.Fatal(err)
	}
	key, _, _ := certs.GenerateKey()
	other, _, _ := certs.GenerateKey()
	ciphertext, err := certs.Encrypt(bundle, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := certs.Decrypt(ciphertext, other); err == nil {
		t.Fatal("错误的 key 必须解密失败")
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := certs.Decrypt(tampered, key); err == nil {
		t.Fatal("被篡改的密文必须解密失败")
	}
}

func TestUnknownFileRejected(t *testing.T) {
	b := certs.New("c")
	b.Files["pki/evil"] = []byte("x")
	if err := b.Validate(); !errors.Is(err, certs.ErrUnknownFile) {
		t.Fatalf("err = %v, want ErrUnknownFile", err)
	}
}

func TestUploadOverwritesExistingSecret(t *testing.T) {
	ctx := context.Background()
	src := testPaths(t)
	writeRequired(t, src)
	bundle, err := certs.Build(src, stackedCluster(), "c")
	if err != nil {
		t.Fatal(err)
	}
	key, _, _ := certs.GenerateKey()
	client := fake.NewSimpleClientset()
	if err := certs.Upload(ctx, client, bundle, key); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// 再上传一次必须走 Update 而不是 AlreadyExists。
	if err := certs.Upload(ctx, client, bundle, key); err != nil {
		t.Fatalf("second Upload: %v", err)
	}
	secret, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).Get(ctx, certs.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if secret.Data[certs.DataKey] == nil {
		t.Fatal("Secret 缺少密文")
	}
	data, err := certs.Download(ctx, client)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if _, err := certs.Decrypt(data, key); err != nil {
		t.Fatalf("下载后解密失败: %v", err)
	}
}

func TestDownloadMissingSecret(t *testing.T) {
	_, err := certs.Download(context.Background(), fake.NewSimpleClientset())
	if err == nil {
		t.Fatal("没有 Secret 时必须报错")
	}
}

// TestWriteToPreflightsEncryptionConflict：证书包按 key 排序写盘，encryption/
// ca.crt/sa.key 都排在 ca.key 之前；冲突必须在写任何文件之前发现。
func TestWriteToPreflightsEncryptionConflict(t *testing.T) {
	src := testPaths(t)
	writeRequired(t, src)
	bundle, err := certs.Build(src, stackedCluster(), "c")
	if err != nil {
		t.Fatal(err)
	}
	dst := testPaths(t)
	writeRequired(t, dst)
	enc, _ := certs.TargetPath(dst, certs.FileEncryptionConfig)
	if err := os.WriteFile(enc, []byte("another-encryption-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 记录 ca.crt 的原始内容：冲突时不能被改写。
	caCertPath, _ := certs.TargetPath(dst, certs.FileCACert)
	before, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.WriteTo(dst, false, nil); !errors.Is(err, certs.ErrCAKeyConflict) {
		t.Fatalf("err = %v, want ErrCAKeyConflict", err)
	}
	after, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("冲突时不能改写 ca.crt（预检必须先于所有写操作）")
	}
	gotEnc, _ := os.ReadFile(enc)
	if string(gotEnc) != "another-encryption-key" {
		t.Fatal("冲突时不能改写 encryption/config.yaml")
	}
}
