package config

import "testing"

// TestValidateRejectsIPv6 固定 P0-6 的边界：IPv6 目前没有完整的
// bracket/dual-stack/CNI 支持，必须在 validation 阶段明确拒绝，
// 而不是等到 etcd URL 或 As4 panic。
func TestValidateRejectsIPv6(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Cluster)
	}{
		{"host address", func(c *Cluster) { c.Cluster.Hosts[0].Address = "fd00::1" }},
		{"matchAddresses", func(c *Cluster) {
			c.Cluster.Hosts[0].Address = "10.0.0.1"
			c.Cluster.Hosts[0].MatchAddresses = []string{"fd00::1"}
		}},
		{"podCIDR", func(c *Cluster) { c.Network.PodCIDR = "fd10::/48" }},
		{"serviceCIDR", func(c *Cluster) { c.Network.ServiceCIDR = "fd01::/108" }},
		{"clusterDNS", func(c *Cluster) { c.Network.ClusterDNS = "fd01::a" }},
		{"externalEndpoints", func(c *Cluster) { c.ControlPlane.ExternalEndpoints = []string{"[fd00::1]:6443"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCluster()
			tc.mutate(c)
			if res := Validate(c); res.OK() {
				t.Fatalf("%s 的 IPv6 配置必须被拒绝", tc.name)
			}
		})
	}
}
