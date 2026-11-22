package config

import (
	"fmt"
	"net/netip"
	"strings"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func validateNetwork(res *ValidationResult, c *Cluster, spec *field.Path) {
	n := c.Network
	p := spec
	validateEnum(res, p.Child("plugin"), n.Plugin, []string{PluginKubeRouter, PluginNone})

	podPrefix, podOK := validateIPv4Prefix(res, p.Child("podCIDR"), n.PodCIDR)
	svcPrefix, svcOK := validateIPv4Prefix(res, p.Child("serviceCIDR"), n.ServiceCIDR)
	if podOK && svcOK && prefixesOverlap(podPrefix, svcPrefix) {
		res.Errors = append(res.Errors, field.Invalid(p.Child("serviceCIDR"), n.ServiceCIDR, "must not overlap with network.podCIDR"))
	}
	validateClusterDNS(res, p, n, svcPrefix, svcOK)
	validateDNSDomain(res, p, n.DNSDomain)
	validateHostAddressOverlap(res, c.Cluster.Hosts, podPrefix, svcPrefix, podOK, svcOK)
	validateNodeCIDRMask(res, p, n, podPrefix, podOK)
	validateKubeRouter(res, p, n)
	validateKubeProxy(res, c, p)
}

// validateClusterDNS 校验 clusterDNS 是 IPv4 且落在 serviceCIDR 内。
func validateClusterDNS(res *ValidationResult, p *field.Path, n NetworkSpec, svcPrefix netip.Prefix, svcOK bool) {
	if !svcOK {
		return
	}
	dns, err := netip.ParseAddr(n.ClusterDNS)
	switch {
	case err != nil:
		res.Errors = append(res.Errors, field.Invalid(p.Child("clusterDNS"), n.ClusterDNS, "not a valid IP address"))
	case dns.Is6():
		res.Errors = append(res.Errors, field.Invalid(p.Child("clusterDNS"), n.ClusterDNS,
			"IPv6 is not supported yet: clusterDNS must be IPv4"))
	case !svcPrefix.Contains(dns):
		res.Errors = append(res.Errors, field.Invalid(p.Child("clusterDNS"), n.ClusterDNS, "must be inside network.serviceCIDR"))
	}
}

// validateDNSDomain 校验集群域名是合法的 DNS-1123 子域。
func validateDNSDomain(res *ValidationResult, p *field.Path, domain string) {
	if domain == "" {
		res.Errors = append(res.Errors, field.Required(p.Child("dnsDomain"), "cluster domain is required"))
		return
	}
	if errs := utilvalidation.IsDNS1123Subdomain(domain); len(errs) > 0 {
		res.Errors = append(res.Errors, field.Invalid(p.Child("dnsDomain"), domain,
			"must be a DNS-1123 subdomain (e.g. cluster.local): "+strings.Join(errs, "; ")))
	}
}

// validateHostAddressOverlap 保证节点地址不落在 Pod/Service CIDR 内。
func validateHostAddressOverlap(res *ValidationResult, hosts Hosts, podPrefix, svcPrefix netip.Prefix, podOK, svcOK bool) {
	if !podOK {
		return
	}
	for i, h := range hosts {
		for _, addr := range h.AllAddresses() {
			a, err := netip.ParseAddr(addr)
			if err != nil {
				continue
			}
			if podPrefix.Contains(a) {
				res.Errors = append(res.Errors, field.Invalid(field.NewPath("cluster", "hosts").Index(i).Child("address"), addr, "host address must not overlap with network.podCIDR"))
			}
			if svcOK && svcPrefix.Contains(a) {
				res.Errors = append(res.Errors, field.Invalid(field.NewPath("cluster", "hosts").Index(i).Child("address"), addr, "host address must not overlap with network.serviceCIDR"))
			}
		}
	}
}

// validateNodeCIDRMask 校验节点子网前缀长度在 (pod 前缀, 32] 内。
func validateNodeCIDRMask(res *ValidationResult, p *field.Path, n NetworkSpec, podPrefix netip.Prefix, podOK bool) {
	if n.NodeCIDRMaskSize == nil || !podOK {
		return
	}
	bits := podPrefix.Bits()
	mask := int(*n.NodeCIDRMaskSize)
	switch {
	case mask <= bits:
		res.Errors = append(res.Errors, field.Invalid(p.Child("nodeCIDRMaskSize"), *n.NodeCIDRMaskSize,
			fmt.Sprintf("must be greater than the pod CIDR prefix length (%d)", bits)))
	case mask > 32:
		// IPv4 只有 32 位；>32 会被 kube-controller-manager 拒绝，
		// 在 validate 阶段拦住比安装到一半再失败便宜得多。
		res.Errors = append(res.Errors, field.Invalid(p.Child("nodeCIDRMaskSize"), *n.NodeCIDRMaskSize,
			"must be at most 32 for IPv4"))
	}
}

// validateKubeRouter 校验 kube-router 自己的参数。
func validateKubeRouter(res *ValidationResult, p *field.Path, n NetworkSpec) {
	if n.KubeRouter.MTU < 0 {
		res.Errors = append(res.Errors, field.Invalid(p.Child("kubeRouter", "mtu"), n.KubeRouter.MTU, "must not be negative"))
	}
	if n.KubeRouter.ClusterASN != nil && (*n.KubeRouter.ClusterASN < 0 || *n.KubeRouter.ClusterASN > 4294967295) {
		res.Errors = append(res.Errors, field.Invalid(p.Child("kubeRouter", "clusterAsn"), *n.KubeRouter.ClusterASN, "must be a valid 32-bit ASN"))
	}
	switch n.KubeRouter.OverlayType {
	case "", "subnet", "full":
	default:
		// 上游 kube-router v2.12 的 --overlay-type 只接受 subnet/full；
		// 封装类型由 --overlay-encap 决定（ipip/fou），没有 vxlan。
		res.Errors = append(res.Errors, field.NotSupported(p.Child("kubeRouter", "overlayType"),
			n.KubeRouter.OverlayType, []string{"subnet", "full"}))
	}
}

// validateIPv4Prefix 解析一个必须为 IPv4 的 CIDR，并把错误写进 res。
// podCIDR 与 serviceCIDR 共用这一份解析、IPv6 拒绝与错误定位逻辑。
func validateIPv4Prefix(res *ValidationResult, p *field.Path, raw string) (netip.Prefix, bool) {
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		res.Errors = append(res.Errors, field.Invalid(p, raw, "not a valid CIDR"))
		return netip.Prefix{}, false
	}
	if prefix.Addr().Is6() {
		res.Errors = append(res.Errors, field.Invalid(p, raw, "IPv6 is not supported yet: only IPv4 CIDRs are accepted"))
		return netip.Prefix{}, false
	}
	if prefix != prefix.Masked() {
		// 10.244.5.6/16 这类带 host bits 的前缀会让 NodeCIDR 派生出
		// 10.244.6.6/24，CNI 子网与 controller-manager 的分配不一致。
		res.Errors = append(res.Errors, field.Invalid(p, raw,
			"CIDR must be in canonical form (host bits must be zero), e.g. 10.244.0.0/16"))
		return netip.Prefix{}, false
	}
	return prefix, true
}

// validateKubeProxy 校验 kube-proxy 的代理模式与 IPVS 调度器。
func validateKubeProxy(res *ValidationResult, c *Cluster, p *field.Path) {
	n := c.Network
	kp := p.Child("kubeProxy")
	validateEnum(res, kp.Child("mode"), n.KubeProxy.Mode,
		[]string{KubeProxyModeIPVS, KubeProxyModeIPTables, KubeProxyModeNFTables})
	if n.KubeProxy.Scheduler != "" && n.KubeProxy.Mode != KubeProxyModeIPVS {
		res.Errors = append(res.Errors, field.Forbidden(kp.Child("scheduler"),
			"only valid when network.kubeProxy.mode=ipvs"))
	}
	if c.KubeProxyEnabled() && n.KubeProxy.Mode == KubeProxyModeNFTables {
		// 配置文件是集群级的，无法在这里知道每台节点的内核；preflight 会按
		// 本机内核严格校验，这里只给出静态提示。
		res.warnf("network.kubeProxy.mode=nftables 需要每个节点内核 >= 5.13；preflight 会按本机内核校验")
	}
}

func prefixesOverlap(a, b netip.Prefix) bool {
	return a.Contains(b.Masked().Addr()) || b.Contains(a.Masked().Addr())
}
