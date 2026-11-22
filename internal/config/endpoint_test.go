package config

import "testing"

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"10.0.0.1", "https://10.0.0.1:6443", true},
		{"10.0.0.1:8443", "https://10.0.0.1:8443", true},
		{"https://10.0.0.1", "https://10.0.0.1:6443", true},
		{"https://10.0.0.1:8443/", "https://10.0.0.1:8443", true},
		{"http://10.0.0.1:6443", "https://10.0.0.1:6443", true},
		{"[fd00::1]:6443", "https://[fd00::1]:6443", true},
		{"fd00::1", "https://[fd00::1]:6443", true},
		{"", "", false},
		{"https://", "", false},
	}
	for _, tc := range tests {
		got, ok := NormalizeEndpoint(tc.in, 6443)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeEndpoint(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestClusterEndpointSelection(t *testing.T) {
	c := &Cluster{}
	c.Cluster.Hosts = Hosts{
		{Name: "m1", Address: "10.0.0.1", Roles: []Role{RoleMaster}},
		{Name: "m2", Address: "10.0.0.2", Roles: []Role{RoleMaster}},
	}
	if got, ok := c.FirstMasterAPIEndpoint(6443); !ok || got != "https://10.0.0.1:6443" {
		t.Fatalf("FirstMasterAPIEndpoint = (%q, %v)", got, ok)
	}
	c.ControlPlane.ExternalEndpoints = []string{"api.example.com"}
	if got, ok := c.ExternalEndpointsAPIEndpoint(6443); !ok || got != "https://api.example.com:6443" {
		t.Fatalf("ExternalEndpointsAPIEndpoint = (%q, %v)", got, ok)
	}
}

func TestExternalEndpointsDisableLocalProxy(t *testing.T) {
	c := &ControlPlaneSpec{}
	if !c.LocalProxyEnabled() {
		t.Fatal("没有外部入口时应启用 worker 本地代理")
	}
	c.ExternalEndpoints = []string{"api.example.com:6443"}
	if c.LocalProxyEnabled() {
		t.Fatal("设置 externalEndpoints 后不应再部署 worker 本地代理")
	}
}
