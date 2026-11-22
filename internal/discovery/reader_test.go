package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"kubemono/internal/bootstrap"
	"kubemono/internal/constants"
)

// TestClientReaderSendsBootstrapToken 覆盖真机踩到的坑：cluster-info 必须用引导
// 令牌认证才能读（匿名会被 RBAC 拒绝），而 reader 一开始漏传了令牌，导致任何节点
// 都无法加入集群（真机实测报 "User \"system:anonymous\" cannot get resource"）。
func TestClientReaderSendsBootstrapToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":%q,"namespace":%q},"data":{"kubeconfig":"x"}}`,
			constants.ClusterInfoName, constants.KubePublicNamespace)
	}))
	defer srv.Close()

	token, err := bootstrap.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	reader := &ClientReader{
		Timeout: 5 * time.Second,
		Token:   token.String(),
		NewClient: func(cfg *rest.Config) (kubernetes.Interface, error) {
			cfg.Insecure = true
			return kubernetes.NewForConfig(cfg)
		},
	}
	if _, err := reader.ClusterInfo(context.Background(), srv.URL); err != nil {
		t.Fatalf("ClusterInfo: %v", err)
	}
	if want := "Bearer " + token.String(); gotAuth != want {
		t.Fatalf("Authorization = %q, want %q（否则会以 system:anonymous 被拒）", gotAuth, want)
	}
}

// TestNewClientReaderCarriesToken 防止构造器再次漏掉令牌。
func TestNewClientReaderCarriesToken(t *testing.T) {
	r := NewClientReader(3*time.Second, "abc123.def456")
	if r.Token != "abc123.def456" {
		t.Fatalf("NewClientReader 必须带上令牌，实际 %q", r.Token)
	}
	if r.Timeout != 3*time.Second {
		t.Fatalf("超时未设置: %v", r.Timeout)
	}
}
