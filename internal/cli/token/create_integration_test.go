package token

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	tokenapi "k8s.io/cluster-bootstrap/token/api"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/discovery"
	"kubemono/internal/kubeconfig"
	"kubemono/internal/pki"
	"kubemono/internal/util/output"
)

// TestCreateDefaultsToWorkerAndPrintsSingleCommand 覆盖命令层的默认行为：
// 不带 --role 时只签 worker，stdout 只有一条命令，不能出现 master/certificate-key。
func TestCreateDefaultsToWorkerAndPrintsSingleCommand(t *testing.T) {
	ca, err := pki.NewCA("kubernetes-ca", "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pki.EncodeCertPEM(ca.Cert)

	opts := options.Default()
	opts.TestRoot = t.TempDir()
	opts.Reporter = &output.Console{Out: io.Discard, Err: io.Discard, MinLevel: output.LevelError}
	paths := opts.Paths()
	caPath := paths.CertPath(constants.CACertName)
	if err := os.MkdirAll(filepath.Dir(caPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	cluster := clusterconfig.DefaultCluster()
	cluster.ControlPlane.ExternalEndpoints = []string{"10.0.0.1:6443"}
	config, err := clusterconfig.MarshalCluster(cluster)
	if err != nil {
		t.Fatal(err)
	}
	configPath := paths.ClusterConfig()
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	blob, err := kubeconfig.Marshal(kubeconfig.Build(kubeconfig.Config{
		User:       constants.UserKubeletBootstrap,
		Server:     "https://10.0.0.1:6443",
		CACertData: caPEM,
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: constants.ClusterStateName, Namespace: constants.KubeSystemNamespace},
			Data:       map[string]string{"clusterID": "cid-1"},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: constants.ClusterInfoName, Namespace: constants.KubePublicNamespace},
			Data: map[string]string{
				tokenapi.KubeConfigKey: string(blob),
				discovery.ReleaseKey:   "v1.0.0-test",
			},
		},
	)
	original := utils.NewClusterClient
	utils.NewClusterClient = func(*options.GlobalOptions) (kubernetes.Interface, error) { return client, nil }
	t.Cleanup(func() { utils.NewClusterClient = original })

	cmd := newCreate(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("token create: %v", err)
	}
	got := out.String()
	if strings.Count(got, "join command") != 1 {
		t.Fatalf("stdout 应只有一条 join 命令:\n%s", got)
	}
	if !strings.Contains(got, "Worker join command") {
		t.Fatalf("默认必须签发 worker:\n%s", got)
	}
	for _, want := range []string{"--token ", "--discovery-token-ca-cert-hash sha256:", "-f " + configPath} {
		if !strings.Contains(got, want) {
			t.Fatalf("join 命令缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--control-plane") || strings.Contains(got, "--certificate-key") {
		t.Fatalf("默认 worker 不能输出 master 参数:\n%s", got)
	}

	secrets, err := client.CoreV1().Secrets(constants.KubeSystemNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 {
		t.Fatalf("应只注册 1 个 bootstrap token，实际 %d", len(secrets.Items))
	}
	if got := string(secrets.Items[0].Data[tokenapi.BootstrapTokenExtraGroupsKey]); got != constants.GroupBootstrappersWorker {
		t.Fatalf("令牌分组 = %q", got)
	}
}
