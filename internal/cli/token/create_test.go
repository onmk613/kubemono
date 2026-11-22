package token

import (
	"strings"
	"testing"
	"time"

	"kubemono/internal/bootstrap"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	jointoken "kubemono/internal/join/token"
)

func TestParseRoleDefaultsToWorkerAndMasterMustBeExplicit(t *testing.T) {
	role, err := parseRole("worker")
	if err != nil || role != clusterconfig.RoleWorker {
		t.Fatalf("worker: role=%s err=%v", role, err)
	}
	role, err = parseRole("master")
	if err != nil || role != clusterconfig.RoleMaster {
		t.Fatalf("master: role=%s err=%v", role, err)
	}
	if _, err := parseRole("etcd"); err == nil {
		t.Fatal("未知角色必须报错")
	}
}

func TestNormalizeTTL(t *testing.T) {
	if got, err := normalizeTTL(0); err != nil || got != constants.DefaultBootstrapTokenTTL {
		t.Fatalf("0 应回落默认值: %s %v", got, err)
	}
	if _, err := normalizeTTL(time.Nanosecond); err != nil {
		t.Fatalf("正数 TTL 应被接受: %v", err)
	}
	if _, err := normalizeTTL(constants.MaxBootstrapTokenTTL + time.Second); err == nil {
		t.Fatal("超过上限的 TTL 必须报错")
	}
	if _, err := normalizeTTL(-time.Second); err == nil {
		t.Fatal("负数 TTL 必须报错")
	}
}

func TestFormatJoinCommandWorkerAndMaster(t *testing.T) {
	material := &jointoken.Material{
		Role:         clusterconfig.RoleWorker,
		Token:        bootstrap.Token{ID: "abcdef", Secret: "0123456789abcdef"},
		CACertHashes: []string{"sha256:aaa", "sha256:bbb"},
	}
	worker := formatJoinCommand("10.0.0.1:6443", material, "/usr/local/kubemono/cluster.yaml")
	for _, want := range []string{
		"kubemono join 10.0.0.1:6443",
		"--token abcdef.0123456789abcdef",
		"--discovery-token-ca-cert-hash sha256:aaa",
		"--discovery-token-ca-cert-hash sha256:bbb",
		"-f /usr/local/kubemono/cluster.yaml",
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("worker 命令缺少 %q: %s", want, worker)
		}
	}
	if strings.Contains(worker, "--control-plane") || strings.Contains(worker, "--certificate-key") {
		t.Fatalf("worker 命令不应带控制面参数: %s", worker)
	}

	material.Role = clusterconfig.RoleMaster
	material.CertificateKey = []byte("0123456789abcdef0123456789abcdef")
	master := formatJoinCommand("10.0.0.1:6443", material, "/usr/local/kubemono/cluster.yaml")
	for _, want := range []string{
		"--control-plane",
		"--certificate-key 3031323334353637383961626364656630313233343536373839616263646566",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master 命令缺少 %q: %s", want, master)
		}
	}
}

func TestFirstJoinEndpointPrefersExternal(t *testing.T) {
	cluster := clusterconfig.DefaultCluster()
	if got := firstJoinEndpoint(cluster); got == "" {
		t.Fatal("默认集群应能从 master 推出一个端点")
	}
	cluster.ControlPlane.ExternalEndpoints = []string{"k8s.example.com:6443"}
	if got := firstJoinEndpoint(cluster); got != "k8s.example.com:6443" {
		t.Fatalf("externalEndpoints 优先，got %q", got)
	}
	if got := firstJoinEndpoint(nil); got != "" {
		t.Fatalf("nil 集群应返回空，got %q", got)
	}
}
