package identity

import (
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/util/stringsx"
)

// Resolver 按 解析本机节点身份。
// 所有外部输入（网卡、主机名、身份文件路径、集群探测）都可注入：测试必须
// 注入假实现，不得依赖运行测试的主机网卡。
type Resolver struct {
	// Lister 枚举本机网卡。为 nil 时使用 DefaultInterfaceLister。
	Lister InterfaceLister
	// Hostname 返回本机主机名。为 nil 时使用 Hostname。
	Hostname func() (string, error)
	// NodeConfigPath 覆盖节点身份文件的位置。
	// 为空表示 constants.NodeConfigFile。
	NodeConfigPath string
}

// Result 是身份识别的结果。
type Result struct {
	// Host 是集群配置中匹配到的节点条目。
	Host *clusterconfig.Host
	// Address 是标识本机的地址。
	Address string
	// KubeletNodeName 是本机实际的 NodeName（kubelet/etcd/证书 CN），由
	// Host.NodeName 策略与本机 hostname 计算得到。
	KubeletNodeName string
	// NodeConfig 是已安装的节点身份（存在且可用时）。
	NodeConfig *clusterconfig.NodeConfig
	// Warnings 列出非致命发现（地址不在本机、主机名不一致等）。
	Warnings []string
}

// NodeName 返回解析出的节点名。
func (r *Result) NodeName() string {
	if r == nil {
		return ""
	}
	if r.KubeletNodeName != "" {
		return r.KubeletNodeName
	}
	if r.Host != nil {
		return r.Host.NodeNameOrName()
	}
	if r.NodeConfig != nil {
		return r.NodeConfig.NodeName
	}
	return ""
}

// NodeConfigFile 返回生效的节点身份文件路径。
func (r *Resolver) nodeConfigFile() string {
	if r != nil && r.NodeConfigPath != "" {
		return r.NodeConfigPath
	}
	return constants.NodeConfigFile
}

func (r *Resolver) lister() InterfaceLister {
	if r != nil && r.Lister != nil {
		return r.Lister
	}
	return DefaultInterfaceLister
}

func (r *Resolver) hostname() (string, error) {
	if r != nil && r.Hostname != nil {
		return r.Hostname()
	}
	return Hostname()
}

// fillNodeName 用 Host.NodeName 策略和本机 hostname 填充实际 NodeName。
func (r *Resolver) fillNodeName(host *clusterconfig.Host, res *Result) {
	if host == nil || res == nil {
		return
	}
	hostname, err := r.hostname()
	if err != nil {
		hostname = ""
	}
	res.KubeletNodeName = host.EffectiveNodeName(hostname)
}

// Resolve 确定本机节点身份。
// override 是 --node 的取值；force 是 --allow-address-mismatch，只影响 override 的地址校验。
func (r *Resolver) Resolve(cfg *clusterconfig.Cluster, override string, force bool) (*Result, error) {
	if cfg == nil {
		return nil, fmt.Errorf("集群配置为空，无法识别本机身份")
	}
	interfaces, err := r.interfaces()
	if err != nil {
		return nil, err
	}

	// 第 1 优先：--node 手动指定。
	if override != "" {
		return r.resolveOverride(cfg, override, force, interfaces)
	}

	// 第 2 优先：/usr/local/kubemono/node.yaml。
	installed, nodeWarnings := r.loadInstalledNodeConfig()
	if installed != nil {
		return r.resolveInstalled(cfg, installed, nodeWarnings)
	}

	// 第 3 优先：网卡 IP 匹配。
	return r.resolveByInterface(cfg, nodeWarnings, interfaces)
}

// resolveOverride 实现优先级规则 1。
func (r *Resolver) resolveOverride(cfg *clusterconfig.Cluster, override string, force bool, interfaces []Interface) (*Result, error) {
	host, ok := cfg.Cluster.Hosts.FindHost(override)
	if !ok {
		return nil, fmt.Errorf("配置中没有名为 %q 的节点；可用节点: %s",
			override, strings.Join(cfg.Cluster.Hosts.SortedNames(), ", "))
	}
	res := &Result{Host: host, Address: normalizeAddress(host.Address)}

	local := allInterfaceAddresses(interfaces)
	if !containsAddress(local, host.AllAddresses()) {
		if !force {
			return nil, fmt.Errorf(
				"节点 %q 的地址(%s)不在本机网卡上；本机地址: %s。\n"+
					"若这是 NAT 场景（节点自报地址与网卡地址不同），请加 --allow-address-mismatch 继续；"+
					"若节点名写错了，请核对配置中的节点列表",
				override, strings.Join(hostAddressStrings(host), ", "),
				joinOrNone(stringsx.DedupSorted(local)))
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"节点 %q 的地址(%s)不在本机网卡上，已因 --allow-address-mismatch 继续（NAT 场景）",
			override, strings.Join(hostAddressStrings(host), ", ")))
	}

	installed, warnings := r.loadInstalledNodeConfig()
	res.NodeConfig = installed
	res.Warnings = append(res.Warnings, warnings...)
	if installed != nil {
		installedConfig := installed.HostName
		if installedConfig == "" {
			installedConfig = installed.NodeName
		}
		if installedConfig != host.Name {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"本机已安装为节点 %q，但 --node 指定了 %q；安装时会覆盖本机身份文件",
				installedConfig, host.Name))
		}
	}
	r.fillNodeName(host, res)
	return res, nil
}

// resolveInstalled 实现优先级规则 2。
// node.yaml 的优先级高于网卡匹配，所以当它声明的节点不在配置中时直接报错：
// 这只有两种可能——用了别的集群的 cluster.yaml，或该节点已被移出集群。
func (r *Resolver) resolveInstalled(cfg *clusterconfig.Cluster, installed *clusterconfig.NodeConfig, warnings []string) (*Result, error) {
	path := r.nodeConfigFile()
	configName := installed.HostName
	host, ok := cfg.Cluster.Hosts.FindHost(configName)
	if !ok {
		return nil, fmt.Errorf(
			"本机身份文件 %s 声明的节点 %q 不在当前配置中；本机不是该集群的成员，"+
				"或使用了错误的 cluster.yaml（确认后执行 node reset 清除本机身份，或用 --node <节点名> 指定）",
			path, configName)
	}

	res := &Result{
		Host:       host,
		Address:    normalizeAddress(host.Address),
		NodeConfig: installed,
		Warnings:   append([]string(nil), warnings...),
	}
	if installed.Address != "" && normalizeAddress(installed.Address) != normalizeAddress(host.Address) {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"节点 %q 的地址已从 %s 变为 %s，安装时将按配置更新本机身份",
			host.Name, installed.Address, host.Address))
	}
	r.fillNodeName(host, res)
	return res, nil
}

// loadInstalledNodeConfig 读取 /usr/local/kubemono/node.yaml。
// 文件不存在时返回 (nil, nil)：没有安装过的节点靠这一点继续做网卡匹配。
// 文件损坏时只记录警告并继续，绝不因为一个坏文件中断 whoami。
func (r *Resolver) loadInstalledNodeConfig() (*clusterconfig.NodeConfig, []string) {
	path := r.nodeConfigFile()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	n, err := clusterconfig.LoadNodeConfig(data)
	if err != nil {
		return nil, []string{fmt.Sprintf("读取本机身份文件 %s 失败，忽略该文件: %v", path, err)}
	}
	return n, nil
}

// resolveByInterface 实现优先级规则 3。
func (r *Resolver) resolveByInterface(cfg *clusterconfig.Cluster, warnings []string, interfaces []Interface) (*Result, error) {
	hosts := cfg.Cluster.Hosts
	excluded := excludedAddressSets(cfg)
	owners := addressOwners(hosts)

	type match struct {
		host    *clusterconfig.Host
		address string
	}
	// pickMatch 在同一个 host 的多个匹配地址中优先选择声明的 Address，
	// 否则选第一个（候选地址已排序，结果稳定）。
	pickMatch := func(ms []match) match {
		for _, m := range ms {
			if normalizeAddress(m.host.Address) == m.address {
				return m
			}
		}
		return ms[0]
	}
	var (
		matches   []match
		conflicts []string
	)
	for _, addr := range candidateAddresses(interfaces, excluded) {
		names, declared := owners[addr]
		if !declared {
			continue
		}
		if len(names) > 1 {
			conflicts = append(conflicts, fmt.Sprintf("%s 同时被节点 %s 声明", addr, strings.Join(names, "、")))
			continue
		}
		host, _ := hosts.FindHost(names[0])
		matches = append(matches, match{host: host, address: addr})
	}

	// 一个 host 的 Address 与 MatchAddresses 可能同时在本机网卡上（多网卡/
	// NAT 是受支持的场景）。按地址粒度的 matches 不能直接当成“多个节点”；
	// 先按 host 去重，再在同一个 host 内选 Address 优先。
	byHost := make(map[string][]match, len(matches))
	for _, m := range matches {
		if m.host == nil {
			continue
		}
		byHost[m.host.Name] = append(byHost[m.host.Name], m)
	}

	switch {
	case len(byHost) == 1:
		var chosen []match
		for _, ms := range byHost {
			chosen = ms
		}
		m := pickMatch(chosen)
		res := &Result{
			Host:     m.host,
			Address:  m.address,
			Warnings: append([]string(nil), warnings...),
		}
		if len(chosen) > 1 {
			addrs := make([]string, 0, len(chosen))
			for _, c := range chosen {
				addrs = append(addrs, c.address)
			}
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("本机在节点 %s 上匹配到多个声明地址（%s），已选择 %s（address 优先）",
					m.host.Name, strings.Join(addrs, "、"), m.address))
		}
		r.fillNodeName(m.host, res)
		return res, nil
	case len(byHost) > 1:
		names := make([]string, 0, len(byHost))
		for name, ms := range byHost {
			addrs := make([]string, 0, len(ms))
			for _, m := range ms {
				addrs = append(addrs, m.address)
			}
			names = append(names, fmt.Sprintf("%s(%s)", name, strings.Join(addrs, "、")))
		}
		sort.Strings(names)
		return nil, fmt.Errorf(
			"本机地址同时匹配到配置中的多个节点: %s。\n"+
				"配置存在冲突（同一地址被多个节点声明），或本机网卡上存在多余的集群地址；"+
				"请修正 cluster.yaml 中的 hosts 地址，或用 --node <节点名> 明确指定本机身份",
			strings.Join(names, "、"))
	case len(conflicts) > 0:
		return nil, fmt.Errorf(
			"无法确定本机身份: 本机地址与配置冲突（%s）。\n"+
				"请修正 cluster.yaml 中的 hosts 地址冲突，或用 --node <节点名> 明确指定本机身份",
			strings.Join(conflicts, "；"))
	default:
		return nil, fmt.Errorf(
			"无法根据网卡确定本机身份: 本机候选地址: %s；配置中的地址: %s。\n"+
				"请确认本机地址已写入 cluster.yaml 的 hosts（NAT 场景写 matchAddresses），"+
				"或用 --node <节点名> 明确指定本机身份",
			joinOrNone(candidateAddresses(interfaces, excluded)),
			joinOrNone(stringsx.DedupSorted(configuredAddresses(cfg))))
	}
}

func (r *Resolver) interfaces() ([]Interface, error) {
	ifaces, err := r.lister()()
	if err != nil {
		return nil, fmt.Errorf("枚举本机网卡失败: %w", err)
	}
	return ifaces, nil
}

// excludedAddressSets 收集永远不能参与网卡匹配的地址：VIP、
// externalEndpoints 的主机地址。
func excludedAddressSets(cfg *clusterconfig.Cluster) map[string]struct{} {
	out := map[string]struct{}{}
	add := func(s string) {
		if a, ok := parseAddress(s); ok {
			out[a.String()] = struct{}{}
		}
	}
	if cfg == nil {
		return out
	}
	for _, ep := range cfg.ControlPlane.ExternalEndpoints {
		if h := clusterconfig.EndpointHost(ep); h != "" {
			add(h)
		}
	}
	return out
}

// allInterfaceAddresses 返回所有已启用网卡的全部地址，
// 用于 --node 的地址校验（回环地址也算本机地址）。
func allInterfaceAddresses(ifaces []Interface) []string {
	out := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		if !iface.IsUp() {
			continue
		}
		for _, p := range iface.Addresses {
			out = append(out, p.Addr().Unmap().String())
		}
	}
	return out
}

// candidateAddresses 返回可以参与网卡匹配的本机地址（已应用全部排除规则），
// 排序去重。
func candidateAddresses(ifaces []Interface, excluded map[string]struct{}) []string {
	out := make([]string, 0, len(ifaces))
	for _, addr := range candidateInterfaceAddresses(ifaces, excluded) {
		out = append(out, addr.String())
	}
	return stringsx.DedupSorted(out)
}

func candidateInterfaceAddresses(ifaces []Interface, excluded map[string]struct{}) []netip.Addr {
	out := make([]netip.Addr, 0, len(ifaces))
	for _, iface := range ifaces {
		if !iface.IsUp() || iface.IsLoopback() || iface.HasFlag(FlagPointToPoint) {
			continue
		}
		if isExcludedInterfaceName(iface.Name) {
			continue
		}
		for _, p := range iface.Addresses {
			if isExcludedAddress(p) {
				continue
			}
			addr := p.Addr().Unmap()
			if _, skip := excluded[addr.String()]; skip {
				continue
			}
			out = append(out, addr)
		}
	}
	return out
}

// addressOwners 把每个配置地址映射到声明它的节点。
// 一个地址被多个节点声明就是配置冲突。
func addressOwners(hosts []clusterconfig.Host) map[string][]string {
	out := map[string][]string{}
	for i := range hosts {
		seen := map[string]struct{}{}
		for _, raw := range hosts[i].AllAddresses() {
			a, ok := parseAddress(raw)
			if !ok {
				continue
			}
			key := a.String()
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out[key] = append(out[key], hosts[i].Name)
		}
	}
	return out
}

// configuredAddresses 列出所有声明过的节点地址，用于诊断。
// 无法解析的字面量原样保留：错误信息必须完整呈现配置，否则运维无法定位问题。
func configuredAddresses(cfg *clusterconfig.Cluster) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Cluster.Hosts))
	for i := range cfg.Cluster.Hosts {
		for _, raw := range cfg.Cluster.Hosts[i].AllAddresses() {
			if v, ok := parseAddress(raw); ok {
				out = append(out, v.String())
				continue
			}
			out = append(out, strings.TrimSpace(raw))
		}
	}
	return out
}

// containsAddress 报告 want 中是否有地址出现在 local 中。
func containsAddress(local, want []string) bool {
	set := map[string]struct{}{}
	for _, a := range local {
		if v, ok := parseAddress(a); ok {
			set[v.String()] = struct{}{}
		}
	}
	for _, a := range want {
		if v, ok := parseAddress(a); ok {
			if _, ok := set[v.String()]; ok {
				return true
			}
		}
	}
	return false
}

// hostAddressStrings 以规范形式渲染节点的全部声明地址。
func hostAddressStrings(h *clusterconfig.Host) []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.AllAddresses()))
	for _, a := range h.AllAddresses() {
		out = append(out, normalizeAddress(a))
	}
	return stringsx.DedupSorted(out)
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "（无）"
	}
	return strings.Join(items, ", ")
}
