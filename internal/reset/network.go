package reset

import (
	"context"
	"fmt"

	"kubemono/internal/constants"
)

// ChainFamily 是 iptables 规则族。
type ChainFamily string

// 规则族取值。
const (
	FamilyIPv4 ChainFamily = "ipv4"
	FamilyIPv6 ChainFamily = "ipv6"
)

// NFTablesTarget 描述"要删掉哪个 nftables 表"。
type NFTablesTarget struct {
	// Family 是 ip / ip6 / inet 等 nftables family。
	Family string `json:"family"`
	// Name 是表名。
	Name string `json:"name"`
}

// IPTablesTarget 描述"要删掉哪些 iptables 链"。
// 用前缀而不是固定清单：Service 代理会按 Service 生成任意多的
// KUBE-SVC-xxx / KUBE-SEP-xxx 链，只有前缀是稳定的。
type IPTablesTarget struct {
	// Family 是 ipv4 或 ipv6。
	Family ChainFamily `json:"family"`
	// Table 是 nat / filter / mangle。
	Table string `json:"table"`
	// Prefix 是链名前缀（kubemono 只管 KUBE-*）。
	Prefix string `json:"prefix"`
}

// NetworkTargets 是本机网络清理的完整清单。
// 它是"要删什么"的唯一来源：真实实现按它执行，fake 实现记录它，
// DryRun 直接打印它——三者看到的是同一份数据。
type NetworkTargets struct {
	// IPTables 是要清理的 iptables/ip6tables 链前缀。
	IPTables []IPTablesTarget `json:"iptables,omitempty"`
	// NFTables 是要删除的 nftables 表（kube-proxy nftables 模式）。
	NFTables []NFTablesTarget `json:"nftables,omitempty"`
	// IPSets 是要删除的 ipset 名字（精确匹配）。
	IPSets []string `json:"ipsets,omitempty"`
	// IPSetPrefixes 是要删除的 ipset 名字前缀（kube-router-*）。
	IPSetPrefixes []string `json:"ipsetPrefixes,omitempty"`
	// Links 是要删除的网络接口名（精确匹配）。
	Links []string `json:"links,omitempty"`
	// LinkPrefixes 是要删除的网络接口名前缀（tun-*）。
	LinkPrefixes []string `json:"linkPrefixes,omitempty"`
	// RoutesViaLinks 表示同时删除经由上述接口的路由。
	RoutesViaLinks bool `json:"routesViaLinks,omitempty"`
	// IPVS 表示清空 ipvs 服务与规则。
	IPVS bool `json:"ipvs,omitempty"`
	// Paths 是要递归删除的 CNI 配置/状态目录。
	Paths []string `json:"paths,omitempty"`
}

// Describe 渲染成可读的清单（dry-run 与日志共用，输出顺序稳定）。
func (t NetworkTargets) Describe() []string {
	var out []string
	for _, target := range t.IPTables {
		out = append(out, fmt.Sprintf("%s %s 表中所有 %s* 链", target.Family, target.Table, target.Prefix))
	}
	for _, target := range t.NFTables {
		out = append(out, fmt.Sprintf("nftables %s %s 表", target.Family, target.Name))
	}
	for _, name := range t.IPSets {
		out = append(out, "ipset "+name)
	}
	for _, prefix := range t.IPSetPrefixes {
		out = append(out, "ipset "+prefix+"*")
	}
	for _, name := range t.Links {
		out = append(out, "网络接口 "+name)
	}
	for _, prefix := range t.LinkPrefixes {
		out = append(out, "网络接口 "+prefix+"*")
	}
	if t.RoutesViaLinks {
		out = append(out, "上述接口上的路由")
	}
	if t.IPVS {
		out = append(out, "全部 ipvs 服务与规则")
	}
	out = append(out, t.Paths...)
	return out
}

// NetworkCleaner 以原生方式清理网络状态。
// 实现不得调用 kube-router 等组件二进制：
// iptables、ipvs、netlink 全部用 Go 库，ipset 用内核工具 ipset(8)。
type NetworkCleaner interface {
	// CleanNetwork 清理清单中的全部网络状态，返回未清理成功的项。
	CleanNetwork(ctx context.Context, targets NetworkTargets) error
}

// DefaultNetworkTargets 返回 kubemono 会在本机创建的网络状态清单。
func DefaultNetworkTargets(paths constants.Paths) NetworkTargets {
	return NetworkTargets{
		IPTables: []IPTablesTarget{
			{Family: FamilyIPv4, Table: "nat", Prefix: "KUBE-"},
			{Family: FamilyIPv4, Table: "filter", Prefix: "KUBE-"},
			{Family: FamilyIPv4, Table: "mangle", Prefix: "KUBE-"},
			{Family: FamilyIPv6, Table: "nat", Prefix: "KUBE-"},
			{Family: FamilyIPv6, Table: "filter", Prefix: "KUBE-"},
			{Family: FamilyIPv6, Table: "mangle", Prefix: "KUBE-"},
		},
		// kube-proxy 的 nftables 模式只使用自己的 ip/ip6 kube-proxy 表。
		NFTables: []NFTablesTarget{
			{Family: "ip", Name: "kube-proxy"},
			{Family: "ip6", Name: "kube-proxy"},
		},
		IPSetPrefixes: []string{"kube-router-"},
		// kubemono0 是 kube-router 的 CNI 网桥（kubemono 自己写的 CNI 配置里的 bridge）。
		// 不删它，重装到新集群时 bridge 插件会拒绝 "already has an IP address
		// different from …"，Pod 永远起不来（真机实测，四轮验收都要手工删）。
		Links:          []string{"kube-ipvs0", "kube-bridge", "kube-dummy-if", "kubemono0"},
		LinkPrefixes:   []string{"tun-"},
		RoutesViaLinks: true,
		IPVS:           true,
		Paths:          []string{paths.CNIConfig(), paths.CNIData()},
	}
}
