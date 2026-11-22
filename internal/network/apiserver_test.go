package network

import (
	"testing"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/render"
)

func testAPIServerContext(localProxy bool) *render.Context {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	if !localProxy {
		c.ControlPlane.ExternalEndpoints = []string{"10.0.0.100:6443"}
	}
	clusterconfig.SetDefaults(c)
	host, _ := c.Cluster.Hosts.FindHost("w1")
	return &render.Context{Cluster: c, Host: host}
}

func TestDaemonSetAPIServerHonorsLocalProxySwitch(t *testing.T) {
	if got := daemonSetAPIServer(testAPIServerContext(true)); got != "https://127.0.0.1:6443" {
		t.Fatalf("localProxy=true server = %q", got)
	}
	if got := daemonSetAPIServer(testAPIServerContext(false)); got != "https://10.0.0.100:6443" {
		t.Fatalf("localProxy=false server = %q，应使用外部 LB 地址", got)
	}
}

func TestDaemonSetAPIServerUsesExternalEndpoints(t *testing.T) {
	c := testAPIServerContext(true) // localProxy 默认启用，但 externalEndpoints 应覆盖它
	c.Cluster.ControlPlane.ExternalEndpoints = []string{"vip.example.com:6443"}
	if got := daemonSetAPIServer(c); got != "https://vip.example.com:6443" {
		t.Fatalf("server = %q，应使用 externalEndpoints", got)
	}
}
