package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsUnsupportedKubeRouterFields(t *testing.T) {
	c := DefaultCluster()
	c.Network.KubeRouter.OverlayType = "vxlan"
	if err := Validate(c).Err(); err == nil || !strings.Contains(err.Error(), "overlayType") {
		t.Fatalf("overlayType=vxlan 必须被拒绝，got %v", err)
	}
}

func TestValidateRejectsNonCanonicalCIDR(t *testing.T) {
	c := DefaultCluster()
	c.Network.PodCIDR = "10.244.5.6/16"
	if err := Validate(c).Err(); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("带 host bits 的 CIDR 必须被拒绝，got %v", err)
	}
}

func TestValidateCertSANs(t *testing.T) {
	for _, san := range []string{"", " ", "api.example.com:6443", "*.example.com", "https://api.example.com", "bad name"} {
		c := DefaultCluster()
		c.ControlPlane.CertSANs = []string{san}
		if err := Validate(c).Err(); err == nil {
			t.Errorf("certSANs %q 必须被拒绝", san)
		}
	}
	c := DefaultCluster()
	c.ControlPlane.CertSANs = []string{"api.example.com", "10.0.0.10"}
	if err := Validate(c).Err(); err != nil {
		t.Fatalf("合法 certSANs 被拒绝: %v", err)
	}
}

func TestValidateExternalEndpointsStrict(t *testing.T) {
	for _, ep := range []string{"https://", "host:abc", "https://k8s.example.com:8443/api", "http://k8s.example.com:6443"} {
		c := DefaultCluster()
		c.ControlPlane.ExternalEndpoints = []string{ep}
		if err := Validate(c).Err(); err == nil {
			t.Errorf("externalEndpoints %q 必须被拒绝", ep)
		}
	}
	c := DefaultCluster()
	c.ControlPlane.ExternalEndpoints = []string{"k8s.example.com:6443"}
	if err := Validate(c).Err(); err != nil {
		t.Fatalf("合法 endpoint 被拒绝: %v", err)
	}
}

func TestDefaultClusterRoundTrip(t *testing.T) {
	original := DefaultCluster()
	data, err := MarshalCluster(original)
	if err != nil {
		t.Fatalf("MarshalCluster: %v", err)
	}
	loaded, err := LoadCluster(data)
	if err != nil {
		t.Fatalf("LoadCluster(MarshalCluster(default)): %v\n%s", err, data)
	}
	if loaded.Network.Plugin != PluginKubeRouter {
		t.Errorf("plugin = %q", loaded.Network.Plugin)
	}
	if loaded.ControlPlane.PKI.KeyAlgorithm != KeyAlgorithmRSA2048 {
		t.Errorf("keyAlgorithm = %q", loaded.ControlPlane.PKI.KeyAlgorithm)
	}
	if len(loaded.Cluster.Hosts) != 1 || loaded.Cluster.Hosts[0].Name != DefaultInitNode {
		t.Errorf("hosts = %+v", loaded.Cluster.Hosts)
	}
}

func TestValidateRejectsInvalidContainerdConfigPatch(t *testing.T) {
	c := DefaultCluster()
	c.Node.Runtime.Containerd.ConfigPatch = "this is not [valid TOML"
	if err := Validate(c).Err(); err == nil || !strings.Contains(err.Error(), "TOML") {
		t.Fatalf("非法 TOML 必须被 config validate 拒绝，got %v", err)
	}
	c = DefaultCluster()
	c.Node.Runtime.Containerd.ConfigPatch = "[plugins.\"io.containerd.cri.v1.images\"]\n"
	if err := Validate(c).Err(); err != nil {
		t.Fatalf("合法 TOML 被拒绝: %v", err)
	}
}
