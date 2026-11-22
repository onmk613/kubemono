package etcdhealth

import (
	"context"
	"strings"
	"testing"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/phase"
)

func testContext(t *testing.T, external bool) *phase.Context {
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
	}
	if external {
		cluster.ControlPlane.Etcd.External = clusterconfig.ExternalEtcdSpec{
			Endpoints: []string{"https://10.0.0.31:2379"},
			CAFile:    "/missing/ca.crt",
			CertFile:  "/missing/client.crt",
			KeyFile:   "/missing/client.key",
		}
	}
	clusterconfig.SetDefaults(cluster)
	host := &cluster.Cluster.Hosts[0]
	return &phase.Context{
		Cluster: cluster,
		Host:    host,
		Paths:   constants.NewPaths(t.TempDir()),
	}
}

func TestAppliesOnlyToExternalMasters(t *testing.T) {
	p := &Phase{}
	if !p.Applies(testContext(t, true)) {
		t.Fatal("external 的 master 必须检查 etcd")
	}
	if p.Applies(testContext(t, false)) {
		t.Fatal("stacked etcd 由本机 etcd phase 检查，etcd-health 不适用")
	}
}

func TestRunFailsFastWhenCredentialsMissing(t *testing.T) {
	p := &Phase{}
	err := p.Run(context.Background(), testContext(t, true))
	if err == nil || !strings.Contains(err.Error(), "无法建立客户端") {
		t.Fatalf("缺少外部 etcd 凭据时必须直接失败，got %v", err)
	}
}
