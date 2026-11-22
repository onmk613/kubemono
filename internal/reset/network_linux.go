//go:build linux

package reset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/moby/ipvs"
	"github.com/vishvananda/netlink"

	"kubemono/internal/execx"
	"kubemono/internal/util/output"
)

// ipsetBinary 是 ipset 内核工具。
// 依赖集里没有纯 Go 的 ipset 库（只有 netlink/iptables/ipvs），因此
// kube-router-* ipset 通过 ipset(8) 清理。它不是任何 K8s 组件二进制，
// 走的仍是内核 ipset 接口，符合 "不依赖组件二进制"的要求。
const ipsetBinary = "ipset"

// nativeNetworkCleaner 用 Go 库原生实现 第 4 步。
type nativeNetworkCleaner struct {
	// Runner 执行 ipset(8)；nil 时跳过 ipset 清理并报告。
	Runner execx.Runner
	// Report 接收进度消息。
	Report output.Reporter
}

// NewNetworkCleaner 返回当前平台的原生网络清理实现。
func NewNetworkCleaner() NetworkCleaner {
	return &nativeNetworkCleaner{Runner: execx.Default()}
}

func (c *nativeNetworkCleaner) report(format string, args ...any) {
	if c.Report != nil {
		c.Report.Infof(format, args...)
	}
}

// CleanNetwork 实现 NetworkCleaner。
// 每一项独立失败：收集全部错误后在最后返回，保证"能清的都清掉"。
func (c *nativeNetworkCleaner) CleanNetwork(ctx context.Context, targets NetworkTargets) error {
	var errs []error

	// 1. KUBE-* iptables / ip6tables 链。
	for _, target := range targets.IPTables {
		if err := c.cleanIPTables(target); err != nil {
			errs = append(errs, err)
		}
	}

	// 2. kube-proxy 的 nftables 表。
	if err := c.cleanNFTables(ctx, targets.NFTables); err != nil {
		errs = append(errs, err)
	}

	// 3. 清理 kube-router-* ipset。
	if err := c.cleanIPSets(ctx, targets); err != nil {
		errs = append(errs, err)
	}

	// 3. ipvs 服务与规则。
	if targets.IPVS {
		if err := cleanIPVS(); err != nil {
			errs = append(errs, err)
		}
	}

	// 4. 网络接口与经由它们路由（kube-ipvs0、kube-bridge、kube-dummy-if、tun-*）。
	if err := c.cleanLinks(ctx, targets); err != nil {
		errs = append(errs, err)
	}

	// 5. CNI 配置与状态目录。
	for _, path := range targets.Paths {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("删除 %s 失败: %w", path, err))
			continue
		}
		c.report("已删除 %s", path)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("原生网络清理未完全成功: %w", err)
	}
	return nil
}

// cleanNFTables 删除 kube-proxy 在 nftables 模式下创建的表。
// nft 是内核工具（不是 K8s 组件二进制）；缺失或表不存在时跳过，不影响 reset。
func (c *nativeNetworkCleaner) cleanNFTables(ctx context.Context, targets []NFTablesTarget) error {
	if len(targets) == 0 {
		return nil
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		c.report("未找到 nft，跳过 kube-proxy nftables 表清理")
		return nil
	}
	var errs []error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, nft, "delete", "table", target.Family, target.Name).CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			// 表不存在是正常情况（例如集群从未使用 nftables 模式）。
			if strings.Contains(msg, "No such file or directory") || strings.Contains(msg, "does not exist") {
				continue
			}
			errs = append(errs, fmt.Errorf("删除 nftables %s %s 表失败: %w（%s）", target.Family, target.Name, err, msg))
			continue
		}
		c.report("已删除 nftables %s %s 表", target.Family, target.Name)
	}
	return errors.Join(errs...)
}

// deleteOneForwardingRule 删除一条跳转到目标前缀链的规则，返回是否删掉了东西。
// 每次都重新 List：iptables 的规则序号只在当次列举内有效。
func deleteOneForwardingRule(ipt *iptables.IPTables, target IPTablesTarget) (bool, error) {
	chains, err := ipt.ListChains(target.Table)
	if err != nil {
		return false, fmt.Errorf("列出 %s %s 表链失败: %w", target.Family, target.Table, err)
	}
	for _, chain := range chains {
		rules, err := ipt.List(target.Table, chain)
		if err != nil {
			return false, fmt.Errorf("列出 %s %s/%s 规则失败: %w", target.Family, target.Table, chain, err)
		}
		// `iptables -S <chain>` 的第一行是 `-N <chain>`（用户链）或
		// `-P <chain> <policy>`（内建链），它们不是规则，不计入 -D 的序号。
		if pos := firstForwardingRule(rules, target.Prefix); pos > 0 {
			if err := ipt.DeleteById(target.Table, chain, pos); err != nil {
				// 序号又失效了（有人并发改规则）：交给下一轮重新列举。
				continue
			}
			return true, nil
		}
	}
	return false, nil
}

// firstForwardingRule 返回“第一条跳转到 prefix 链的规则”的 1-based 规则号。
// 只统计 -A 行：-N/-P 是链定义，不是 iptables -D 的规则序号。
func firstForwardingRule(rules []string, prefix string) int {
	n := 0
	for _, rule := range rules {
		if !strings.HasPrefix(rule, "-A ") {
			continue
		}
		n++
		if referencesChain(rule, prefix) {
			return n
		}
	}
	return 0
}

// cleanIPTables 删除一个 (族, 表) 中所有前缀匹配的链。
// iptables 不允许删除仍被引用的链，因此先把所有指向 KUBE-* 链的规则删掉，
// 再反复尝试删除链本身（链之间可能互相引用，顺序不确定）。
func (c *nativeNetworkCleaner) cleanIPTables(target IPTablesTarget) error {
	proto := iptables.ProtocolIPv4
	if target.Family == FamilyIPv6 {
		proto = iptables.ProtocolIPv6
	}
	ipt, err := iptables.NewWithProtocol(proto)
	if err != nil {
		return fmt.Errorf("初始化 %s iptables 失败: %w", target.Family, err)
	}
	chains, err := ipt.ListChains(target.Table)
	if err != nil {
		return fmt.Errorf("列出 %s %s 表链失败: %w", target.Family, target.Table, err)
	}
	kube := map[string]bool{}
	for _, chain := range chains {
		if strings.HasPrefix(chain, target.Prefix) {
			kube[chain] = true
		}
	}
	if len(kube) == 0 {
		return nil
	}

	// 先删除所有"跳转到 KUBE-* 链"的规则。
	// 规则序号在两次调用之间可能失效：reset 时 kube-router 可能
	// 还在跑并自己增删规则，或者同一轮里前一条规则的删除让后面的序号整体前移。
	// 真机实测就会报 `iptables: Index of deletion too big.`。
	// 因此这里**每删一条都重新列举**，只按"当前快照里的第一条匹配"删，序号永远新鲜。
	const maxPasses = 4096 // 防御性上限：避免规则被反复重建时死循环 / Defensive cap: avoid an infinite loop when rules are repeatedly recreated
	for range maxPasses {
		deleted, err := deleteOneForwardingRule(ipt, target)
		if err != nil {
			return err
		}
		if !deleted {
			break
		}
	}

	// 再删链本身：反复扫描，直到没有进展（链之间可能存在引用关系）。
	remaining := make([]string, 0, len(kube))
	for _, chain := range chains {
		if kube[chain] {
			remaining = append(remaining, chain)
		}
	}
	var errs []error
	for pass := 0; pass <= len(remaining) && len(remaining) > 0; pass++ {
		var next []string
		for _, chain := range remaining {
			if err := ipt.ClearAndDeleteChain(target.Table, chain); err != nil {
				next = append(next, chain)
				continue
			}
			c.report("已删除 iptables 链 %s %s/%s", target.Family, target.Table, chain)
		}
		if len(next) == len(remaining) {
			break
		}
		remaining = next
	}
	for _, chain := range remaining {
		errs = append(errs, fmt.Errorf("删除 %s %s/%s 链失败：仍被引用或权限不足", target.Family, target.Table, chain))
	}
	return errors.Join(errs...)
}

// referencesChain 判断一条 iptables -S 规则是否跳转到前缀匹配的链。
func referencesChain(rule, prefix string) bool {
	for _, marker := range []string{"-j " + prefix, "--jump " + prefix, "-g " + prefix, "--goto " + prefix} {
		if strings.Contains(rule, marker) {
			return true
		}
	}
	return false
}

// cleanIPSets 删除 kube-router-* ipset。
func (c *nativeNetworkCleaner) cleanIPSets(ctx context.Context, targets NetworkTargets) error {
	if len(targets.IPSets) == 0 && len(targets.IPSetPrefixes) == 0 {
		return nil
	}
	if c.Runner == nil {
		return fmt.Errorf("没有配置命令执行器，无法清理 ipset %s", strings.Join(targets.IPSetPrefixes, "、"))
	}
	out, err := c.Runner.Run(ctx, ipsetBinary, "list", "-name")
	if err != nil {
		// 最小化安装的系统通常没有 ipset(8)。没有这个工具就说明本机没有
		// 可管理的 ipset（kube-router 的时代也已过去），不应让 reset 失败。
		if errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found") {
			c.report("ipset 不可用（未安装），跳过 ipset 清理")
			return nil
		}
		return fmt.Errorf("列出 ipset 失败: %w", err)
	}
	exact := map[string]bool{}
	for _, name := range targets.IPSets {
		exact[name] = true
	}
	var errs []error
	for name := range strings.FieldsSeq(string(out)) {
		if !exact[name] && !hasAnyPrefix(name, targets.IPSetPrefixes) {
			continue
		}
		if _, err := c.Runner.Run(ctx, ipsetBinary, "destroy", name); err != nil {
			errs = append(errs, fmt.Errorf("删除 ipset %s 失败: %w", name, err))
			continue
		}
		c.report("已删除 ipset %s", name)
	}
	return errors.Join(errs...)
}

// cleanIPVS 删除全部 ipvs 服务（连同其 destination）。
func cleanIPVS() error {
	handle, err := ipvs.New("")
	if err != nil {
		// 内核没有加载 ip_vs 时没有可清理的 ipvs 状态。
		return nil
	}
	defer handle.Close()
	services, err := handle.GetServices()
	if err != nil {
		return fmt.Errorf("列出 ipvs 服务失败: %w", err)
	}
	var errs []error
	for _, service := range services {
		if err := handle.DelService(service); err != nil {
			errs = append(errs, fmt.Errorf("删除 ipvs 服务 %s:%d 失败: %w",
				service.Address, service.Port, err))
		}
	}
	return errors.Join(errs...)
}

// cleanLinks 删除 kubemono 创建的网络接口，以及经由它们的路由。
func (c *nativeNetworkCleaner) cleanLinks(ctx context.Context, targets NetworkTargets) error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("列出网络接口失败: %w", err)
	}
	var errs []error
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := link.Attrs().Name
		if !matchesName(name, targets.Links, targets.LinkPrefixes) {
			continue
		}
		if targets.RoutesViaLinks {
			if err := deleteRoutesVia(link.Attrs().Index); err != nil {
				errs = append(errs, err)
			}
		}
		if err := netlink.LinkDel(link); err != nil {
			errs = append(errs, fmt.Errorf("删除网络接口 %s 失败: %w", name, err))
			continue
		}
		c.report("已删除网络接口 %s", name)
	}
	return errors.Join(errs...)
}

// deleteRoutesVia 删除经由指定接口的全部 IPv4/IPv6 路由。
func deleteRoutesVia(index int) error {
	var errs []error
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{LinkIndex: index}, netlink.RT_FILTER_OIF)
		if err != nil {
			// 该族没有路由表时忽略。
			continue
		}
		for i := range routes {
			if err := netlink.RouteDel(&routes[i]); err != nil {
				errs = append(errs, fmt.Errorf("删除路由 %s 失败: %w", routes[i].Dst, err))
			}
		}
	}
	return errors.Join(errs...)
}

// matchesName 判断名字是否命中精确清单或前缀清单。
func matchesName(name string, exact, prefixes []string) bool {
	if slices.Contains(exact, name) {
		return true
	}
	return hasAnyPrefix(name, prefixes)
}

// hasAnyPrefix 判断名字是否命中任一前缀。
func hasAnyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
