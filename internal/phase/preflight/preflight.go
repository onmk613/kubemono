// Package preflight 实现 `preflight` phase：在动主机之前把所有"不可能
// 成功"的原因找出来。
// 设计要点：preflight 是**闸门**而不是幂等步骤。它每次都重新执行（Check 恒为
// false），因为"上一次通过"不能证明这次主机状态没变（端口可能被占用、
// 时间可能漂移）。失败时返回错误并列出修复建议。
package preflight

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"kubemono/internal/hostinit"
	"kubemono/internal/phase"
)

// PhaseName 是 `preflight` phase 的名称。
const PhaseName = "preflight"

func init() { phase.MustRegister(&Phase{}) }

// Phase 执行预检。
type Phase struct {
	// preflight 只读主机，没有需要回滚的变更。
	// PortChecker、CommandLookup 与 StatFS 是可注入的接缝。
	PortChecker   hostinit.PortChecker
	CommandLookup hostinit.CommandLookup
	// Facts 可选地注入已采集的主机信息。
	Facts *hostinit.Facts
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 报告本节点是否应执行预检：所有节点都执行。
func (p *Phase) Applies(c *phase.Context) bool { return c.Cluster != nil }

// Check 恒为 false：主机状态随时可能变化，preflight 必须每次重跑。
func (p *Phase) Check(context.Context, *phase.Context) (bool, error) { return false, nil }

// AlwaysPending 告诉 runner：Check=false 是设计，不是“执行后未收敛”。
func (p *Phase) AlwaysPending() bool { return true }

// Run 采集主机信息并执行全部检查。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	facts, err := p.collect(ctx, c)
	if err != nil {
		return err
	}
	report := c.Reporter()

	opts := p.options(c, facts)
	if c.DryRun {
		report.Infof("将以 dry-run 方式执行 %d 项预检（只读，不修改主机）", len(hostinit.DeclaredChecks()))
	}
	results := hostinit.RunPreflight(ctx, opts)
	failed, warned := hostinit.Summarize(results)

	for _, r := range results {
		msg := r.Message
		if msg == "" {
			msg = r.Name
		}
		switch r.Status {
		case hostinit.Fail:
			report.Errorf("%s: %s%s", r.Name, msg, remediation(r))
		case hostinit.Warn:
			report.Warnf("%s: %s", r.Name, msg)
		default:
			report.Debugf("%s: %s", r.Name, msg)
		}
	}
	if failed > 0 {
		return fmt.Errorf("预检失败：%d 项不通过，%d 项警告（修复后重新执行）", failed, warned)
	}
	if warned > 0 {
		report.Warnf("预检通过，但有 %d 项警告", warned)
	} else {
		report.Result(true, "预检通过（%d 项检查）", len(results))
	}
	return nil
}

// collect 采集主机信息（或返回注入的主机信息）。
func (p *Phase) collect(ctx context.Context, c *phase.Context) (*hostinit.Facts, error) {
	if p.Facts != nil {
		return p.Facts, nil
	}
	facts, err := hostinit.Collect(ctx, hostinit.NewCollector())
	if err != nil {
		return nil, fmt.Errorf("采集主机信息失败: %w", err)
	}
	c.Reporter().Debugf("主机信息：%s %s，内核 %s，cgroup %s，%d CPU，%s 内存",
		facts.OSName, facts.OSVersion, facts.KernelVersion, facts.CgroupVersion,
		facts.CPUs, hostinit.FormatBytes(facts.MemoryBytes))
	return facts, nil
}

// options 根据集群配置构造预检参数。
func (p *Phase) options(c *phase.Context, facts *hostinit.Facts) *hostinit.PreflightOptions {
	opts := &hostinit.PreflightOptions{
		Facts:            facts,
		DataDirs:         dataDirs(c),
		Ports:            hostinit.PortsFor(c.Cluster, c.Host),
		PortChecker:      p.PortChecker,
		CommandLookup:    p.CommandLookup,
		AllowCgroupV1:    kubernetesMinor(c) < 35,
		RequiredCommands: requiredCommands(c),
	}
	if c.Cluster != nil {
		if c.Cluster.KubeProxyEnabled() {
			opts.KubeProxyMode = c.Cluster.Network.KubeProxy.Mode
		}
	}
	return opts
}

// dataDirs 返回剩余空间有影响的目录。
func dataDirs(c *phase.Context) []string {
	if c.Cluster == nil {
		return nil
	}
	dirs := []string{c.Paths.Kubelet()}
	// 外部 etcd 的数据目录不在本机受管范围内，不检查 /var/lib/etcd。
	if !c.Cluster.ControlPlane.Etcd.IsExternal() {
		dirs = append(dirs, c.Paths.EtcdData())
	}
	if root := c.Cluster.Node.Runtime.Containerd.Root; root != "" {
		dirs = append(dirs, c.Paths.Resolve(root))
	}
	sort.Strings(dirs)
	return dirs
}

// peers 列出其他节点的身份标识，用于唯一性检查。

// remediation 渲染失败检查项的修复提示。
func remediation(r hostinit.Result) string {
	if r.Remediation == "" {
		return ""
	}
	return "（" + r.Remediation + "）"
}

// kubernetesMinor 从生效 BOM 的 Kubernetes 版本解析 minor 号。
// 解析失败按 0 处理：宁可把 cgroup v1 判成不允许（fail closed），
// 也不要放过一个 kubelet 根本起不来的节点。
func kubernetesMinor(c *phase.Context) int {
	if c == nil || c.BOM == nil {
		return 0
	}
	raw := strings.TrimPrefix(strings.TrimSpace(c.BOM.Kubernetes), "v")
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return 0
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return minor
}

// requiredCommands 返回本机 host-init/网络真正依赖的命令。
// bridge/portmap CNI 插件与 kube-proxy 都会在宿主上调用 iptables；
// 极简气隙镜像经常没有它，安装会“成功”到第一次 Pod sandbox 才失败。
func requiredCommands(c *phase.Context) []string {
	out := append([]string(nil), hostinit.DefaultRequiredCommands...)
	if c == nil || c.Cluster == nil {
		return out
	}
	if host, ok := c.Self(); ok && host.IsWorker() {
		out = append(out, "iptables")
	}
	if c.Cluster.KubeProxyEnabled() && c.Cluster.Network.KubeProxy.Mode == "nftables" {
		out = append(out, "nft")
	}
	return out
}
