package config

import "testing"

func TestValidateNetworkRejectsInvalidDNSDomain(t *testing.T) {
	for _, domain := range []string{"bad domain", "Cluster.Local", "cluster..local", "-cluster.local"} {
		t.Run(domain, func(t *testing.T) {
			c := DefaultCluster()
			c.Network.DNSDomain = domain
			if res := Validate(c); res.OK() {
				t.Fatalf("dnsDomain %q 应被拒绝", domain)
			}
		})
	}
}

func TestValidateNodeCIDRMaskSizeUpperBound(t *testing.T) {
	c := DefaultCluster()
	c.Network.PodCIDR = "10.244.0.0/16"
	c.Network.NodeCIDRMaskSize = int32Ptr(40)
	if res := Validate(c); res.OK() {
		t.Fatal("nodeCIDRMaskSize=40 应被拒绝（IPv4 最多 32 位）")
	}

	c = DefaultCluster()
	c.Network.PodCIDR = "10.244.0.0/16"
	c.Network.NodeCIDRMaskSize = int32Ptr(24)
	if res := Validate(c); !res.OK() {
		t.Fatalf("nodeCIDRMaskSize=24 应通过: %v", res.Err())
	}
}

func TestValidateSysctlsRejectsPathTraversal(t *testing.T) {
	for _, key := range []string{"../../etc/passwd", "net..ipv4", "/etc/passwd", "net.ipv4. ip_forward"} {
		t.Run(key, func(t *testing.T) {
			c := DefaultCluster()
			c.Node.Host.Sysctls = map[string]string{key: "1"}
			if res := Validate(c); res.OK() {
				t.Fatalf("sysctls 键 %q 应被拒绝", key)
			}
		})
	}

	c := DefaultCluster()
	c.Node.Host.Sysctls = map[string]string{"vm.max_map_count": "262144"}
	if res := Validate(c); !res.OK() {
		t.Fatalf("合法 sysctls 应通过: %v", res.Err())
	}
}

func TestValidateRegistryHostRejectsPathSeparators(t *testing.T) {
	for _, host := range []string{"../evil", "registry/example.com", "registry example.com", "registry..example.com"} {
		t.Run(host, func(t *testing.T) {
			c := DefaultCluster()
			c.Node.Runtime.Containerd.Registries.Configs = map[string]RegistryAuthSpec{host: {}}
			if res := Validate(c); res.OK() {
				t.Fatalf("registry host %q 应被拒绝", host)
			}
		})
	}

	c := DefaultCluster()
	c.Node.Runtime.Containerd.Registries.Configs = map[string]RegistryAuthSpec{"registry.example.com:5000": {}}
	if res := Validate(c); !res.OK() {
		t.Fatalf("合法 registry host 应通过: %v", res.Err())
	}
}

func TestValidateNetworkHostOverlapUsesClusterHostPath(t *testing.T) {
	c := DefaultCluster()
	c.Cluster.Hosts[0].Address = "10.244.0.5"
	c.Cluster.Hosts[0].MatchAddresses = nil
	res := Validate(c)
	if res.OK() {
		t.Fatal("host address 与 podCIDR 重叠应被拒绝")
	}
	for _, e := range res.Errors {
		if e.Field == "cluster.hosts[0].address" {
			return
		}
	}
	t.Fatalf("错误路径应定位到 cluster.hosts[0].address，实际: %v", res.Errors)
}
