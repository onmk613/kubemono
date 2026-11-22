package hostinit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

func (o *PreflightOptions) checkPorts(ctx context.Context) Result {
	var busy, unknown []string
	ports := uniqueSocketPorts(o.Ports)
	for _, p := range ports {
		free, err := o.PortChecker.Free(ctx, p.Protocol, p.Port)
		switch {
		case err != nil:
			unknown = append(unknown, fmt.Sprintf("%s（%v）", describePort(p), err))
		case !free:
			busy = append(busy, describePort(p))
		}
	}
	switch {
	case len(busy) > 0:
		return result(CheckPorts, Fail, "端口已被占用："+strings.Join(busy, "、"))
	case len(unknown) > 0:
		return result(CheckPorts, Warn, "无法确认以下端口是否空闲："+strings.Join(unknown, "、"))
	default:
		return result(CheckPorts, Pass, fmt.Sprintf("%d 个端口全部空闲", len(ports)))
	}
}

// checkResources 检查 CPU、内存、数据目录空间与 inode。
func (o *PreflightOptions) checkResources() Result {
	var fails, warns, oks []string

	switch {
	case o.Facts.CPUs <= 0:
		warns = append(warns, "无法确定 CPU 核数")
	case o.Facts.CPUs < o.MinCPUs:
		fails = append(fails, fmt.Sprintf("CPU 核数 %d < %d", o.Facts.CPUs, o.MinCPUs))
	default:
		oks = append(oks, fmt.Sprintf("CPU %d 核", o.Facts.CPUs))
	}

	switch {
	case o.Facts.MemoryBytes == 0:
		warns = append(warns, "无法确定内存大小")
	case o.Facts.MemoryBytes < o.MinMemoryBytes:
		fails = append(fails, fmt.Sprintf("内存 %s < %s", FormatBytes(o.Facts.MemoryBytes), FormatBytes(o.MinMemoryBytes)))
	default:
		oks = append(oks, fmt.Sprintf("内存 %s", FormatBytes(o.Facts.MemoryBytes)))
	}

	for _, dir := range o.DataDirs {
		free, inodes, err := o.StatFS(dir)
		switch {
		case errors.Is(err, ErrUnsupported):
			warns = append(warns, fmt.Sprintf("无法确定 %s 的可用空间（当前平台不支持）", dir))
			continue
		case errors.Is(err, fs.ErrNotExist):
			warns = append(warns, fmt.Sprintf("无法确定 %s 的可用空间：目录不存在", dir))
			continue
		case err != nil:
			warns = append(warns, fmt.Sprintf("无法确定 %s 的可用空间：%v", dir, err))
			continue
		}
		if free < o.MinFreeBytes {
			fails = append(fails, fmt.Sprintf("%s 可用空间 %s < %s", dir, FormatBytes(free), FormatBytes(o.MinFreeBytes)))
			continue
		}
		if inodes > 0 && inodes < o.MinInodes {
			fails = append(fails, fmt.Sprintf("%s 可用 inode %d < %d", dir, inodes, o.MinInodes))
			continue
		}
		oks = append(oks, fmt.Sprintf("%s 可用 %s", dir, FormatBytes(free)))
	}

	switch {
	case len(fails) > 0:
		return result(CheckResources, Fail, strings.Join(fails, "；"))
	case len(warns) > 0:
		return result(CheckResources, Warn, strings.Join(warns, "；"))
	default:
		return result(CheckResources, Pass, strings.Join(oks, "，"))
	}
}

// checkKernel 检查内核版本（RHEL/Rocky 8 系列使用回移植内核，只 Warn）。
func (o *PreflightOptions) checkKernel() Result {
	major, minor, ok := parseKernelVersion(o.Facts.KernelMajorMinor)
	if !ok {
		major, minor, ok = parseKernelVersion(o.Facts.KernelVersion)
	}
	if !ok {
		return result(CheckKernel, Warn, "无法确定内核版本")
	}
	version := fmt.Sprintf("%d.%d", major, minor)
	// nftables 模式在上游有硬性内核要求（>= 5.13），不能像 RHEL 8 那样只告警。
	if o.KubeProxyMode == "nftables" && !kernelAtLeast(major, minor, MinNFTablesKernelMajor, MinNFTablesKernelMinor) {
		return result(CheckKernel, Fail,
			fmt.Sprintf("内核 %s 不支持 kube-proxy nftables 模式（需要 %d.%d+）",
				version, MinNFTablesKernelMajor, MinNFTablesKernelMinor))
	}
	if o.Facts.OSFamily == OSFamilyRHEL && o.Facts.OSMajorVersion() == 8 {
		if !kernelAtLeast(major, minor, DefaultStreamKernelMajor, DefaultStreamKernelMinor) {
			return result(CheckKernel, Warn,
				fmt.Sprintf("RHEL/Rocky 8 系列内核 %s 低于建议的 %d.%d，请确认 ipvs/nft 特性已经回移植",
					version, DefaultStreamKernelMajor, DefaultStreamKernelMinor))
		}
		return result(CheckKernel, Pass, "内核 "+version)
	}
	if !kernelAtLeast(major, minor, DefaultMinKernelMajor, DefaultMinKernelMinor) {
		return result(CheckKernel, Fail,
			fmt.Sprintf("内核 %s 低于最低要求 %d.%d", version, DefaultMinKernelMajor, DefaultMinKernelMinor))
	}
	return result(CheckKernel, Pass, "内核 "+version)
}

// kernelAtLeast 报告 major.minor 是否不低于 minMajor.minMin。
// 版本比较统一在这里处理 major/minor 双条件，避免多处手写漏掉等号边界。
func kernelAtLeast(major, minor, minMajor, minMin int) bool {
	if major != minMajor {
		return major > minMajor
	}
	return minor >= minMin
}

// checkCgroup 检查 cgroup 版本，优先 v2。
func (o *PreflightOptions) checkCgroup() Result {
	switch o.Facts.CgroupVersion {
	case CgroupV2:
		return result(CheckCgroup, Pass, "使用 cgroup v2")
	case CgroupV1:
		if !o.AllowCgroupV1 {
			return result(CheckCgroup, Fail,
				"使用 cgroup v1：Kubernetes v1.35 起 kubelet 默认 failCgroupV1=true，不会在 cgroup v1 节点上启动；请切换到 cgroup v2（内核启动参数 systemd.unified_cgroup_hierarchy=1）")
		}
		return result(CheckCgroup, Warn, "使用 cgroup v1（当前 Kubernetes 版本仍允许），建议切换到 v2")
	default:
		return result(CheckCgroup, Fail, "/sys/fs/cgroup 缺失或无法识别 cgroup 版本")
	}
}

// checkContainer 检查是否运行在容器里。
func (o *PreflightOptions) checkContainer() Result {
	if o.Facts.Containerized {
		return result(CheckContainer, Warn, "检测到容器环境，主机初始化可能不适用")
	}
	return result(CheckContainer, Pass, "未检测到容器环境")
}

// checkSwap 检查 swap 状态。
func (o *PreflightOptions) checkSwap() Result {
	if o.Facts.SwapEnabled {
		// kubemono 不修改 swap：kubelet 配置里已固定 failSwapOn: false 来容忍它
		// （见 internal/profile/base/kubelet-config.yaml.tmpl）。需要关闭时由运维
		// 自行 swapoff -a 并注释 /etc/fstab，或用 cluster.hooks 完成。
		return result(CheckSwap, Warn, "swap 已启用；kubemono 不会关闭它（kubelet 已配 failSwapOn: false），如需关闭请自行处理")
	}
	return result(CheckSwap, Pass, "swap 未启用")
}

// checkSELinux 只提示：kubemono 不修改 SELinux。
func (o *PreflightOptions) checkSELinux() Result {
	switch strings.ToLower(strings.TrimSpace(o.Facts.SELinuxMode)) {
	case SELinuxEnforcing:
		return result(CheckSELinux, Warn,
			"SELinux 为 enforcing：kubemono 不修改 SELinux；Pod 卷/网络异常时请检查策略")
	case SELinuxPermissive:
		return result(CheckSELinux, Pass, "SELinux 为 permissive，不阻塞安装")
	default:
		return result(CheckSELinux, Pass, "未检测到启用的 SELinux")
	}
}

// checkFirewall 只提示：检测到活动防火墙时列出需要放行的端口。
// 不逐端口解析 firewalld/ufw 规则：nftables/rich rule/zone 场景下文本解析
// 不可靠，而且与 ports 检查的语义重叠。
func (o *PreflightOptions) checkFirewall(_ context.Context) Result {
	backend := strings.TrimSpace(o.Facts.FirewallBackend)
	switch backend {
	case "", FirewallBackendNone:
		return result(CheckFirewall, Pass, "未检测到活动防火墙")
	default:
		ports := make([]string, 0, len(o.Ports))
		for _, p := range uniqueSocketPorts(o.Ports) {
			ports = append(ports, describePort(p))
		}
		return result(CheckFirewall, Warn,
			"检测到 "+backend+" 处于活动状态；kubemono 不修改防火墙，请确认以下端口对集群节点放行："+strings.Join(ports, "、"))
	}
}

// checkCommands 检查依赖命令是否存在。
func (o *PreflightOptions) checkCommands() Result {
	var missing []string
	for _, name := range o.RequiredCommands {
		if _, err := o.CommandLookup.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return result(CheckCommands, Fail, "缺少必需命令："+strings.Join(missing, "、"))
	}
	return result(CheckCommands, Pass, fmt.Sprintf("%d 个必需命令均存在", len(o.RequiredCommands)))
}

// describePort 渲染 "6443/tcp (kube-apiserver)"。
func describePort(p PortSpec) string {
	spec := p.Protocol
	if p.Port > 0 {
		spec = strconv.Itoa(p.Port) + "/" + p.Protocol
	}
	if p.Component == "" {
		return spec
	}
	return spec + " (" + p.Component + ")"
}

// uniqueSocketPorts 返回去重后的 tcp/udp 端口。
func uniqueSocketPorts(ports []PortSpec) []PortSpec {
	seen := map[string]bool{}
	out := make([]PortSpec, 0, len(ports))
	for _, p := range ports {
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			continue
		}
		key := p.Protocol + "/" + strconv.Itoa(p.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out
}

// FormatBytes 把字节数格式化为人类可读的十进制单位。
func FormatBytes(bytes uint64) string {
	const unit = 1000
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	units := []string{"kB", "MB", "GB", "TB", "PB"}
	for _, name := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, name)
		}
	}
	return fmt.Sprintf("%.1f EB", value/unit)
}
