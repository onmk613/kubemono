package network

import (
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/render"
)

// profileOf 加载当前发行版的 profile。
// profile 内容嵌入在二进制里、不依赖 BOM；nil 检查只是把"上下文不完整"
// 提前成更明确的错误。
// networkSpec 返回网络配置，不可用时返回零值。
func networkSpec(c *render.Context) clusterconfig.NetworkSpec {
	if c == nil || c.Cluster == nil {
		return clusterconfig.NetworkSpec{}
	}
	return c.Cluster.Network
}

// NodeCIDR 返回单节点的 Pod 子网。
// 优先使用 kube-controller-manager 已经分配到 Node.spec.podCIDR 的值（调用方通过
// assigned 传入），否则按 podCIDR + nodeCIDRMaskSize 推导出第一个子网。
// 推导值只是"节点还没被分配 CIDR"时的兜底：CNI 的 host-local IPAM 只在第一次
// ADD 时读取它，之后 kube-controller-manager 写入的 podCIDR 才是权威值。
func NodeCIDR(podCIDR string, maskSize int32, assigned string) (string, error) {
	if assigned != "" {
		if _, err := netip.ParsePrefix(assigned); err != nil {
			return "", fmt.Errorf("节点已分配的 podCIDR %q 不是合法网段: %w", assigned, err)
		}
		return assigned, nil
	}
	prefix, err := netip.ParsePrefix(podCIDR)
	if err != nil {
		return "", fmt.Errorf("network.podCIDR %q 不是合法网段: %w", podCIDR, err)
	}
	// 防御性 mask：即使调用方传入带 host bits 的前缀，也不把错误扩散到
	// host-local IPAM；配置校验会在更早的阶段拒绝这种输入。
	prefix = prefix.Masked()
	if maskSize <= 0 {
		maskSize = 24
	}
	if int(maskSize) < prefix.Bits() || maskSize > 32 {
		return "", fmt.Errorf("network.nodeCIDRMaskSize %d 必须落在 %d..32 之间", maskSize, prefix.Bits())
	}
	// 本节点在集群网段中的序号：kubemono 用确定性推导（第 1 个），因为同一时刻只有
	// 一台首次加入的节点在读这份兜底配置，真正的分配由 controller-manager 完成。
	index := 1
	bits := int(maskSize) - prefix.Bits()
	if bits < 0 || bits > 24 {
		return "", fmt.Errorf("network.nodeCIDRMaskSize %d 与 podCIDR %s 的差值过大", maskSize, podCIDR)
	}
	shift := 32 - int(maskSize)
	base := uint64(addrToUint32(prefix.Addr()))
	offset := uint64(index) << uint(shift)
	// 上界必须把 base 一起算进来：只检查 offset 会漏掉 base+offset 回绕
	// （例如 255.255.255.255/32 + nodeCIDRMaskSize=32 会静默变成 0.0.0.0/32）。
	if base+offset > uint64(^uint32(0)) {
		return "", fmt.Errorf("network.podCIDR %s 太小，无法容纳 nodeCIDRMaskSize=%d", podCIDR, maskSize)
	}
	node := uint32(base + offset)
	out := netip.PrefixFrom(uint32ToAddr(node), int(maskSize))
	return out.String(), nil
}

func addrToUint32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uint32ToAddr(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// DetectHostMTU 返回本机网卡 MTU。优先匹配 addresses 中的地址，否则取第一个
// 非 loopback 且 up 的网卡；探测失败或非 Linux 时返回 DefaultHostMTU。
func DetectHostMTU(addresses ...string) int {
	if runtime.GOOS != "linux" {
		return DefaultHostMTU
	}
	want := map[string]bool{}
	for _, a := range addresses {
		if ip := net.ParseIP(strings.TrimSpace(a)); ip != nil {
			want[ip.String()] = true
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return DefaultHostMTU
	}
	fallback := 0
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 || iface.MTU <= 0 {
			continue
		}
		if fallback == 0 {
			fallback = iface.MTU
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			host, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			if want[host.String()] {
				return iface.MTU
			}
		}
	}
	if fallback > 0 {
		return fallback
	}
	return DefaultHostMTU
}

// SuggestedMTU 返回 CNI 应使用的 MTU。
// kube-router 的 IPIP overlay 会在原始报文外再包一层（IPv4 20 字节 + 外层 IP 头），
// 因此必须从宿主网卡 MTU 里扣掉开销，否则跨子网的大包会被静默丢弃。
func SuggestedMTU(c *render.Context, hostMTU int) int {
	if hostMTU <= 0 {
		hostMTU = DefaultHostMTU
	}
	n := networkSpec(c)
	if n.KubeRouter.MTU > 0 {
		return n.KubeRouter.MTU
	}
	if n.Plugin != clusterconfig.PluginKubeRouter {
		// 第三方 CNI 自己决定封装方式，kubemono 只给出宿主 MTU。
		return hostMTU
	}
	if !boolValue(n.KubeRouter.EnableOverlay, true) {
		return hostMTU
	}
	switch n.KubeRouter.OverlayType {
	case "", "subnet", "full":
		// 默认封装是 IPIP（--overlay-encap=ipip），预留 20 字节。
		return hostMTU - IPIPOverhead
	default:
		return hostMTU - IPIPOverhead
	}
}

// DefaultHostMTU 在无法测量宿主 MTU 时使用。
// 1500 是绝大多数以太网的 MTU；需要时由调用方通过 network.kubeRouter.mtu 覆盖。
const DefaultHostMTU = 1500

// Overlay 封装开销（字节）。
const (
	// IPIPOverhead 是 IPIP 隧道的 IPv4 外层头。
	IPIPOverhead = 20
)

// boolValue 解引用可选 bool 并给出默认值。
func boolValue(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// kubeRouterDeployed 报告是否必须存在 kube-router DaemonSet。
func kubeRouterDeployed(c *render.Context) bool {
	return networkSpec(c).Plugin == clusterconfig.PluginKubeRouter
}
