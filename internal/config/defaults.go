package config

import (
	"fmt"
	"net/netip"
	"time"

	"kubemono/internal/constants"
)

// BoolPtr 返回 v 的指针（可选字段的辅助函数）。
func BoolPtr(v bool) *bool { return &v }

// int32Ptr 返回 v 的指针。
func int32Ptr(v int32) *int32 { return &v }

// int64Ptr 返回 v 的指针。
func int64Ptr(v int64) *int64 { return &v }

// intPtr 返回 v 的指针。
func intPtr(v int) *int { return &v }

// 默认配置里的占位值。
// init 节点和主机地址无法从代码推导，DefaultCluster 用这两个值生成一份
// 可直接校验的模板；安装前必须按实际环境替换。
const (
	// DefaultInitNode 是第一台 master 的占位名称；默认模板同时给它 worker
	// 角色，让用户直观看到 master/worker 是两个独立角色。
	DefaultInitNode = "master-1"
	// DefaultHostAddress 是第一台 master 的占位地址。
	// 192.0.2.0/24 是 RFC 5737 的文档地址段，不会与真实主机冲突。
	DefaultHostAddress = "192.0.2.1"
)

// DefaultCluster 返回一份填入了全部内置默认值的完整集群配置。
// cluster.hosts 是占位值（见上面的常量），其余字段都等于 SetDefaults 的结果。
// 生成的配置可以直接通过 Validate，便于 `kubemono config default > cluster.yaml`
// 后按需修改。
func DefaultCluster() *Cluster {
	c := &Cluster{
		Cluster: ClusterSpec{
			Hosts: Hosts{{
				Name:     DefaultInitNode,
				NodeName: NodeNameFromHostname,
				Address:  DefaultHostAddress,
				Roles:    []Role{RoleMaster, RoleWorker},
			}},
		},
	}
	SetDefaults(c)
	return c
}

// SetDefaults 填充集群配置里所有未设置的字段。
// 默认值只在这里定义一次；渲染、校验、diff 都以 SetDefaults 之后的对象为准。
func SetDefaults(c *Cluster) {
	setControlPlaneDefaults(c)
	setNodeDefaults(c)
	setNetworkDefaults(c)
	setHostDefaults(c)
	setAddonsDefaults(c)
}

func setControlPlaneDefaults(c *Cluster) {
	if c.ControlPlane.PKI.KeyAlgorithm == "" {
		c.ControlPlane.PKI.KeyAlgorithm = KeyAlgorithmRSA2048
	}
	if c.ControlPlane.PKI.CAValidity.IsZero() {
		c.ControlPlane.PKI.CAValidity = Duration(constants.DefaultCATTL)
	}
	if c.ControlPlane.PKI.CertValidity.IsZero() {
		c.ControlPlane.PKI.CertValidity = Duration(constants.DefaultCertTTL)
	}
	if c.ControlPlane.Audit.Enabled == nil {
		c.ControlPlane.Audit.Enabled = BoolPtr(true)
	}
	if c.ControlPlane.Audit.LogMaxAge == nil {
		c.ControlPlane.Audit.LogMaxAge = intPtr(constants.DefaultAuditMaxAge)
	}
	if c.ControlPlane.Audit.LogMaxBackup == nil {
		c.ControlPlane.Audit.LogMaxBackup = intPtr(constants.DefaultAuditMaxBackup)
	}
	if c.ControlPlane.Audit.LogMaxSize == nil {
		c.ControlPlane.Audit.LogMaxSize = intPtr(constants.DefaultAuditMaxSize)
	}
	if !c.ControlPlane.Etcd.IsExternal() && c.ControlPlane.Etcd.Local.DataDir == "" {
		c.ControlPlane.Etcd.Local.DataDir = constants.EtcdDataDir
	}
}

func setNodeDefaults(c *Cluster) {
	if c.Node.Runtime.Containerd.Root == "" {
		c.Node.Runtime.Containerd.Root = "/var/lib/containerd"
	}
	if c.Node.Runtime.Containerd.SystemdCgroup == nil {
		c.Node.Runtime.Containerd.SystemdCgroup = BoolPtr(true)
	}
	if c.Node.Kubelet.RootDir == "" {
		c.Node.Kubelet.RootDir = constants.KubeletDir
	}
}

func setNetworkDefaults(c *Cluster) {
	n := &c.Network
	if n.Plugin == "" {
		n.Plugin = PluginKubeRouter
	}
	if n.PodCIDR == "" {
		n.PodCIDR = constants.DefaultPodCIDR
	}
	if n.ServiceCIDR == "" {
		n.ServiceCIDR = constants.DefaultServiceCIDR
	}
	if n.DNSDomain == "" {
		n.DNSDomain = constants.DefaultDNSDomain
	}
	if n.NodeCIDRMaskSize == nil {
		n.NodeCIDRMaskSize = int32Ptr(constants.DefaultNodeCIDRMaskSize)
	}
	if n.ClusterDNS == "" {
		if dns, err := FirstUsableServiceIP(n.ServiceCIDR, 10); err == nil {
			n.ClusterDNS = dns
		} else {
			n.ClusterDNS = constants.DefaultClusterDNS
		}
	}
	if n.KubeRouter.RunRouter == nil {
		n.KubeRouter.RunRouter = BoolPtr(true)
	}
	if n.KubeRouter.RunFirewall == nil {
		n.KubeRouter.RunFirewall = BoolPtr(true)
	}
	if n.KubeRouter.EnableOverlay == nil {
		n.KubeRouter.EnableOverlay = BoolPtr(true)
	}
	if n.KubeRouter.OverlayType == "" {
		n.KubeRouter.OverlayType = "subnet"
	}
	if n.KubeRouter.ClusterASN == nil {
		n.KubeRouter.ClusterASN = int64Ptr(64512)
	}
	if n.KubeProxy.Enabled == nil {
		n.KubeProxy.Enabled = BoolPtr(true)
	}
	if n.KubeProxy.Mode == "" {
		n.KubeProxy.Mode = KubeProxyModeIPVS
	}
	// scheduler 只对 ipvs 有意义：默认配置里的 rr 在改成 iptables/nftables 后
	// 自动清掉，避免用户只改 mode 就被校验拒绝。
	if n.KubeProxy.Mode != KubeProxyModeIPVS && n.KubeProxy.Scheduler == KubeProxyDefaultScheduler {
		n.KubeProxy.Scheduler = ""
	}
	if n.KubeProxy.Scheduler == "" && n.KubeProxy.Mode == KubeProxyModeIPVS {
		n.KubeProxy.Scheduler = KubeProxyDefaultScheduler
	}
}

// setHostDefaults 把 K8s 必需的内核模块与 sysctl 写成显式配置值。
// 用户可以在 cluster.yaml 里直接看到并调整整份清单；没有隐藏的默认值 +
// extra 追加模式。显式写空列表/空 map 表示一项都不做。
func setHostDefaults(c *Cluster) {
	if c.Node.Host.KernelModules == nil {
		c.Node.Host.KernelModules = defaultKernelModules(c)
	}
	if c.Node.Host.Sysctls == nil {
		c.Node.Host.Sysctls = defaultSysctls()
	}
}

func defaultKernelModules(c *Cluster) []string {
	out := []string{"overlay", "br_netfilter", "nf_conntrack"}
	if c.KubeProxyEnabled() {
		if c.Network.KubeProxy.Mode == KubeProxyModeNFTables {
			out = append(out, "nf_tables")
		} else {
			out = append(out, "ip_vs", "ip_vs_rr", "ip_vs_wrr", "ip_vs_sh")
		}
	}
	if c.Network.Plugin == PluginKubeRouter &&
		(c.Network.KubeRouter.EnableOverlay == nil || *c.Network.KubeRouter.EnableOverlay) {
		out = append(out, "ipip")
	}
	return out
}

func defaultSysctls() map[string]string {
	return map[string]string{
		"net.ipv4.ip_forward":                "1",
		"net.bridge.bridge-nf-call-iptables": "1",
		"fs.inotify.max_user_instances":      "8192",
		"fs.inotify.max_user_watches":        "524288",
	}
}

func setAddonsDefaults(c *Cluster) {
	if c.Addons.CoreDNS.Enabled == nil {
		c.Addons.CoreDNS.Enabled = BoolPtr(true)
	}
	if c.Addons.CoreDNS.Replicas == nil {
		c.Addons.CoreDNS.Replicas = int32Ptr(2)
	}
	if c.Addons.CoreDNS.Enabled != nil && *c.Addons.CoreDNS.Enabled && len(c.Addons.CoreDNS.Upstream) == 0 {
		c.Addons.CoreDNS.Upstream = []string{"/etc/resolv.conf"}
	}
	if c.Addons.CoreDNS.LogFormat == "" {
		c.Addons.CoreDNS.LogFormat = "text"
	}
}

// FirstUsableServiceIP 返回 CIDR 中第 n 个可用地址
// （n=1 是网络地址之后的第一个地址）。
// 默认 clusterDNS 是 Service 网段第 10 个地址：10.96.0.0/12 → 10.96.0.10。
// 网络地址本身不用，所以从 network+1 开始数。
func FirstUsableServiceIP(cidr string, n int) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", err
	}
	if n < 1 {
		n = 1
	}
	addr := prefix.Masked().Addr()
	for i := 0; i < n; i++ {
		addr = addr.Next()
		if !prefix.Contains(addr) {
			return "", fmt.Errorf("service CIDR %s is too small for the default cluster DNS address", cidr)
		}
	}
	return addr.String(), nil
}

// CertValidity 返回叶子证书的有效期。
func (c *Cluster) CertValidity() time.Duration { return c.ControlPlane.PKI.CertValidity.D() }

// CAValidity 返回 CA 的有效期。
func (c *Cluster) CAValidity() time.Duration { return c.ControlPlane.PKI.CAValidity.D() }
