// Package identity 解析集群配置中本机是哪一台节点，并读写本机节点身份存储。
// 身份识别遵循 的优先级：
// 1. --node <name> 手动指定（地址不在本机时警告，--allow-address-mismatch 用于 NAT 场景）；
// 2. /usr/local/kubemono/node.yaml（已安装节点直接读取自己的身份）；
// 3. 本机网卡地址与 hosts[].address / hosts[].matchAddresses 求交集。
// 本包不做任何变更，也不引入集群网络依赖：身份解析在完全离线时也能完成。
package identity

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
)

// 命中这些名字的接口，其地址永远不会标识集群节点。这类接口由容器运行时、
// CNI 或隧道创建，地址与 cluster.yaml 中的主机地址无关。
var excludedInterfacePrefixes = []string{
	"docker",
	"cni",
	"kube-",
	"cali",
	"flannel",
	"cilium",
	"vxlan",
	"tun",
	"tap",
	"br-",
	"veth",
	"virbr",
	"lxc",
	"tailscale",
}

// excludedInterfaceNames 按精确匹配排除（kubemono-ipvs0 是完整名字）。
var excludedInterfaceNames = []string{
	"kube-ipvs0",
}

// linkLocalV6 是 fe80::/10。
var linkLocalV6 = netip.MustParsePrefix("fe80::/10")

// linkLocalV4 是 169.254.0.0/16。
var linkLocalV4 = netip.MustParsePrefix("169.254.0.0/16")

// Interface 是一个本机网卡及其地址。
type Interface struct {
	// Name 是网卡名，例如 "eth0"。
	Name string
	// Flags 以文本形式保存关注的标志位：
	Flags []string
	// Addresses 保存网卡的全部可用 IP 地址。
	Addresses []netip.Prefix
}

// HasFlag 报告网卡是否带有给定标志。
func (i Interface) HasFlag(flag string) bool {
	return slices.Contains(i.Flags, flag)
}

// IsLoopback 报告网卡是否为回环设备。
func (i Interface) IsLoopback() bool { return i.HasFlag("loopback") }

// IsUp 报告网卡是否处于管理上的 up 状态。
func (i Interface) IsUp() bool { return i.HasFlag("up") }

// InterfaceLister 枚举本机网卡。测试通过注入假实现，
// 避免依赖运行测试的主机网卡。
type InterfaceLister func() ([]Interface, error)

// FlagUp、FlagLoopback 等是 Interface.Flags 中使用的稳定取值。
const (
	FlagUp           = "up"
	FlagLoopback     = "loopback"
	FlagPointToPoint = "pointtopoint"
	FlagMulticast    = "multicast"
)

// DefaultInterfaceLister 列出本机网卡。
// 只返回 up 的接口；地址按接口声明顺序保留，便于错误信息稳定可读。
func DefaultInterfaceLister() ([]Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("枚举本机网卡失败: %w", err)
	}
	out := make([]Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		item := Interface{Name: iface.Name, Flags: interfaceFlags(iface.Flags)}
		addrs, err := iface.Addrs()
		if err != nil {
			// 单个网卡读不到地址不应让整体失败：它无法参与匹配。
			continue
		}
		for _, a := range addrs {
			if p, ok := parseInterfaceAddr(a); ok {
				item.Addresses = append(item.Addresses, p)
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func interfaceFlags(flags net.Flags) []string {
	out := make([]string, 0, 4)
	if flags&net.FlagUp != 0 {
		out = append(out, FlagUp)
	}
	if flags&net.FlagLoopback != 0 {
		out = append(out, FlagLoopback)
	}
	if flags&net.FlagPointToPoint != 0 {
		out = append(out, FlagPointToPoint)
	}
	if flags&net.FlagMulticast != 0 {
		out = append(out, FlagMulticast)
	}
	return out
}

// parseInterfaceAddr 把网卡的 net.Addr 转换为前缀。
// 地址统一去掉 IPv6 的 zone（%eth0），否则无法与配置中的字面量比较。
func parseInterfaceAddr(a net.Addr) (netip.Prefix, bool) {
	s := a.String()
	if i := strings.IndexByte(s, '%'); i >= 0 {
		// zone 只出现在 "fe80::1%eth0/64" 形式中。
		if j := strings.IndexByte(s, '/'); j > i {
			s = s[:i] + s[j:]
		} else {
			s = s[:i]
		}
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, true
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(addr, addr.BitLen()), true
	}
	return netip.Prefix{}, false
}

// Hostname 返回本机主机名（os.Hostname）。
func Hostname() (string, error) { return os.Hostname() }

// isExcludedInterfaceName 报告网卡名是否命中排除规则。
func isExcludedInterfaceName(name string) bool {
	if slices.Contains(excludedInterfaceNames, name) {
		return true
	}
	for _, prefix := range excludedInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// isExcludedAddress 报告地址是否永远无法标识节点：
// 回环地址、IPv4/IPv6 链路本地地址。
func isExcludedAddress(p netip.Prefix) bool {
	if p.Addr().IsLoopback() {
		return true
	}
	if p.Addr().Is4() || p.Addr().Is4In6() {
		return linkLocalV4.Contains(p.Addr())
	}
	return linkLocalV6.Contains(p.Addr())
}

// parseAddress 解析单个地址字面量，容忍 "addr/prefix" 与
// IPv6 zone 后缀。配置里偶尔会写成 CIDR，这里一并接受。
func parseAddress(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	if strings.ContainsRune(s, '/') {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Addr{}, false
		}
		return p.Addr().Unmap(), true
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// normalizeAddress 把配置中的地址渲染为规范形式。无法解析的
// 字面量原样返回（校验阶段已经拒绝，这里是兜底）。
func normalizeAddress(s string) string {
	if a, ok := parseAddress(s); ok {
		return a.String()
	}
	return strings.TrimSpace(s)
}
