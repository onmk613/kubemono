package config

import (
	"testing"

	"kubemono/internal/constants"
)

func TestFirstUsableServiceIP(t *testing.T) {
	cases := []struct {
		cidr string
		n    int
		want string
	}{
		{"10.96.0.0/12", 10, "10.96.0.10"},
		{"10.96.0.0/16", 1, "10.96.0.1"},
		{"10.96.0.0/16", 2, "10.96.0.2"},
		{"172.20.0.0/16", 10, "172.20.0.10"},
	}
	for _, tc := range cases {
		got, err := FirstUsableServiceIP(tc.cidr, tc.n)
		if err != nil {
			t.Fatalf("FirstUsableServiceIP(%q,%d): %v", tc.cidr, tc.n, err)
		}
		if got != tc.want {
			t.Errorf("FirstUsableServiceIP(%q,%d) = %s, want %s", tc.cidr, tc.n, got, tc.want)
		}
	}
	if _, err := FirstUsableServiceIP("not-a-cidr", 1); err == nil {
		t.Error("invalid CIDR must fail")
	}
}

// TestDefaultClusterIsValid 校验生成的默认配置足够完整，
// 无需手工修改即可通过校验。
func TestDefaultClusterIsValid(t *testing.T) {
	c := DefaultCluster()
	if res := Validate(c); !res.OK() {
		t.Fatalf("DefaultCluster does not validate: %v", res.Err())
	}
	host, ok := c.Cluster.Hosts.FindHost(DefaultInitNode)
	if !ok || !host.IsMaster() || !host.IsWorker() || !host.IsMixedMaster() {
		t.Fatalf("init host %q 应为 master+worker 混合角色: %+v", DefaultInitNode, host)
	}
	if host.Address != DefaultHostAddress {
		t.Fatalf("init host address = %q, want %q", host.Address, DefaultHostAddress)
	}
	if c.Network.ClusterDNS != constants.DefaultClusterDNS {
		t.Fatalf("clusterDNS = %q, want %q", c.Network.ClusterDNS, constants.DefaultClusterDNS)
	}
	if c.Addons.CoreDNS.LogFormat != "text" {
		t.Fatalf("coredns logFormat = %q, want text", c.Addons.CoreDNS.LogFormat)
	}
}

func TestValidateComponentVersions(t *testing.T) {
	c := DefaultCluster()
	c.Images.ComponentVersions = map[string]string{"coredns": "v1.14.7"}
	if res := Validate(c); !res.OK() {
		t.Fatalf("普通组件覆盖应通过: %v", res.Err())
	}

	// k8s 组件只能通过顶层的 kubernetesVersion 整组固定，写进
	// componentVersions 会被拒绝（避免两个事实来源）。
	c = DefaultCluster()
	c.Images.ComponentVersions = map[string]string{"kube-apiserver": "v1.37.2"}
	if res := Validate(c); res.OK() {
		t.Fatal("componentVersions 里的 k8s 组件应被拒绝")
	}

	// kubernetesVersion 是 k8s 组件的唯一入口，结构校验只看格式。
	c = DefaultCluster()
	c.KubernetesVersion = "v1.37.4"
	if res := Validate(c); !res.OK() {
		t.Fatalf("合法 kubernetesVersion 应通过: %v", res.Err())
	}
	for _, bad := range []string{"1.37.4", "v1.37", "v1.37.4-rc.1", "latest", "v1.37.x"} {
		c := DefaultCluster()
		c.KubernetesVersion = bad
		if res := Validate(c); res.OK() {
			t.Errorf("kubernetesVersion %q 应被拒绝", bad)
		}
	}

	bad := []map[string]string{
		{"kubemono": "v1.37.1-km.2"},
		{"": "v1.0.0"},
		{"coredns": " "},
	}
	for _, versions := range bad {
		c := DefaultCluster()
		c.Images.ComponentVersions = versions
		if res := Validate(c); res.OK() {
			t.Errorf("覆盖 %v 应被拒绝", versions)
		}
	}
}

func TestVersionOverridesExpandsKubernetesVersion(t *testing.T) {
	c := DefaultCluster()
	c.KubernetesVersion = " v1.37.4 "
	c.Images.ComponentVersions = map[string]string{"coredns": "v1.14.7"}

	pins := c.VersionOverrides()
	if pins["coredns"] != "v1.14.7" {
		t.Fatalf("非 k8s 组件未保留: %v", pins)
	}
	for _, name := range constants.KubernetesComponentNames() {
		if pins[name] != "v1.37.4" {
			t.Errorf("%s = %q, want v1.37.4", name, pins[name])
		}
	}
	if len(pins) != len(constants.KubernetesComponentNames())+1 {
		t.Fatalf("覆盖数量 = %d: %v", len(pins), pins)
	}

	// 空值不产生任何 k8s 覆盖。
	empty := DefaultCluster()
	empty.Images.ComponentVersions = nil
	if got := empty.VersionOverrides(); got != nil {
		t.Fatalf("空配置应返回 nil: %v", got)
	}
	var nilCluster *Cluster
	if got := nilCluster.VersionOverrides(); got != nil {
		t.Fatalf("nil cluster 应返回 nil: %v", got)
	}
}

// TestKubeProxyDefaults 锁定 Service 代理默认由 kube-proxy（IPVS）承担。
func TestKubeProxyDefaults(t *testing.T) {
	c := DefaultCluster()
	if !c.KubeProxyEnabled() {
		t.Fatal("kube-proxy 默认应启用")
	}
	if c.Network.KubeProxy.Mode != KubeProxyModeIPVS {
		t.Fatalf("kubeProxy.mode = %q，期望 %q", c.Network.KubeProxy.Mode, KubeProxyModeIPVS)
	}
	if c.Network.KubeProxy.Scheduler != KubeProxyDefaultScheduler {
		t.Fatalf("kubeProxy.scheduler = %q，期望 %q", c.Network.KubeProxy.Scheduler, KubeProxyDefaultScheduler)
	}
	if got := ServiceProxyMode(c); got != "kube-proxy" {
		t.Fatalf("ServiceProxyMode = %q", got)
	}
	c.Network.KubeProxy.Enabled = BoolPtr(false)
	if got := ServiceProxyMode(c); got != "external" {
		t.Fatalf("关闭 kube-proxy 后 ServiceProxyMode = %q", got)
	}
}

// TestValidateKubeProxy 覆盖 mode/scheduler 与受管 extraArgs 的校验。
func TestValidateRegistryCredentials(t *testing.T) {
	cases := []struct {
		name string
		auth RegistryAuthSpec
		ok   bool
	}{
		{"both", RegistryAuthSpec{Username: "u", Password: "p"}, true},
		{"neither", RegistryAuthSpec{CAFile: "/usr/local/kubemono/ca.crt"}, true},
		{"username only", RegistryAuthSpec{Username: "u"}, false},
		{"password only", RegistryAuthSpec{Password: "p"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCluster()
			c.Node.Runtime.Containerd.Registries.Configs = map[string]RegistryAuthSpec{"reg.internal:5000": tc.auth}
			if got := Validate(c).OK(); got != tc.ok {
				t.Fatalf("Validate OK = %v，期望 %v：%v", got, tc.ok, Validate(c).Err())
			}
		})
	}
}

func TestValidateHostTaints(t *testing.T) {
	cases := []struct {
		name   string
		taints []string
		ok     bool
	}{
		{"empty", nil, true},
		{"no effect, defaults to NoSchedule", []string{"dedicated=infra"}, true},
		{"explicit NoSchedule", []string{"dedicated=infra:NoSchedule"}, true},
		{"PreferNoSchedule", []string{"dedicated=infra:PreferNoSchedule"}, true},
		{"NoExecute", []string{"dedicated=infra:NoExecute"}, true},
		{"bad effect", []string{"dedicated=infra:Bad"}, false},
		{"empty key", []string{"=infra:NoSchedule"}, false},
		{"empty item", []string{""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCluster()
			c.Cluster.Hosts[0].Taints = tc.taints
			if got := Validate(c).OK(); got != tc.ok {
				t.Fatalf("Validate OK = %v，期望 %v：%v", got, tc.ok, Validate(c).Err())
			}
		})
	}
}

func TestValidateKubeProxy(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Cluster)
		ok     bool
	}{
		{"ipvs default", func(*Cluster) {}, true},
		{"iptables", func(c *Cluster) { c.Network.KubeProxy.Mode = KubeProxyModeIPTables; SetDefaults(c) }, true},
		{"nftables", func(c *Cluster) { c.Network.KubeProxy.Mode = KubeProxyModeNFTables; SetDefaults(c) }, true},
		{"bad mode", func(c *Cluster) { c.Network.KubeProxy.Mode = "userspace" }, false},
		{"scheduler on iptables", func(c *Cluster) {
			c.Network.KubeProxy.Mode = KubeProxyModeIPTables
			c.Network.KubeProxy.Scheduler = "rr"
		}, false},
		{"kube-router service proxy", func(c *Cluster) {
			c.Network.KubeRouter.ExtraArgs = map[string]string{"run-service-proxy": "true"}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCluster()
			tc.mutate(c)
			if got := Validate(c).OK(); got != tc.ok {
				t.Fatalf("Validate OK = %v，期望 %v：%v", got, tc.ok, Validate(c).Err())
			}
		})
	}
}

// TestValidateCoreDNSLogFormat 固定 coredns 日志格式只接受 text/json。
func TestValidateCoreDNSLogFormat(t *testing.T) {
	cases := []struct {
		format string
		ok     bool
	}{
		{"", true},
		{"text", true},
		{"json", true},
		{"yaml", false},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			c := DefaultCluster()
			c.Addons.CoreDNS.LogFormat = tc.format
			if got := Validate(c).OK(); got != tc.ok {
				t.Fatalf("Validate OK = %v，期望 %v：%v", got, tc.ok, Validate(c).Err())
			}
		})
	}
}

// TestSetDefaultsDerivesClusterDNS 校验文档中描述的默认值推导。
func TestSetDefaultsDerivesClusterDNS(t *testing.T) {
	c := &Cluster{}
	c.Cluster.Hosts = []Host{{Name: "m1", Address: "10.0.0.1", Roles: []Role{RoleMaster}}}
	c.Network.ServiceCIDR = "172.20.0.0/16"
	SetDefaults(c)
	if c.Network.ClusterDNS != "172.20.0.10" {
		t.Fatalf("clusterDNS = %s, want 172.20.0.10", c.Network.ClusterDNS)
	}
	// 显式给出的 clusterDNS 不能被默认值覆盖。
	c2 := &Cluster{}
	c2.Cluster.Hosts = []Host{{Name: "m1", Address: "10.0.0.1", Roles: []Role{RoleMaster}}}
	c2.Network.ClusterDNS = "10.96.0.99"
	SetDefaults(c2)
	if c2.Network.ClusterDNS != "10.96.0.99" {
		t.Fatalf("explicit clusterDNS was overwritten: %s", c2.Network.ClusterDNS)
	}
}

func TestHostDefaultsAreExplicit(t *testing.T) {
	c := DefaultCluster()
	wantModules := []string{"overlay", "br_netfilter", "nf_conntrack", "ip_vs", "ip_vs_rr", "ip_vs_wrr", "ip_vs_sh", "ipip"}
	if len(c.Node.Host.KernelModules) != len(wantModules) {
		t.Fatalf("kernelModules = %v", c.Node.Host.KernelModules)
	}
	for i, want := range wantModules {
		if c.Node.Host.KernelModules[i] != want {
			t.Fatalf("kernelModules[%d] = %q, want %q", i, c.Node.Host.KernelModules[i], want)
		}
	}
	if got := c.Node.Host.Sysctls["net.ipv4.ip_forward"]; got != "1" {
		t.Fatalf("net.ipv4.ip_forward = %q", got)
	}

	nft := &Cluster{}
	nft.Network.KubeProxy.Mode = KubeProxyModeNFTables
	SetDefaults(nft)
	foundNFT, foundIPVS := false, false
	for _, m := range nft.Node.Host.KernelModules {
		foundNFT = foundNFT || m == "nf_tables"
		foundIPVS = foundIPVS || m == "ip_vs"
	}
	if !foundNFT || foundIPVS {
		t.Fatalf("nftables 默认模块 = %v", nft.Node.Host.KernelModules)
	}

	empty := DefaultCluster()
	empty.Node.Host.KernelModules = []string{}
	empty.Node.Host.Sysctls = map[string]string{}
	SetDefaults(empty)
	if len(empty.Node.Host.KernelModules) != 0 || len(empty.Node.Host.Sysctls) != 0 {
		t.Fatalf("显式空清单应保留: %v / %v", empty.Node.Host.KernelModules, empty.Node.Host.Sysctls)
	}
}
