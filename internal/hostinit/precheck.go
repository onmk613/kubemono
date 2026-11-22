package hostinit

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

// Status 是单项预检的结论。
type Status string

// 预检结论。
const (
	// Pass 表示检查通过。
	Pass Status = "pass"
	// Warn 表示不阻塞安装，但需要用户确认。
	Warn Status = "warn"
	// Fail 表示必须先修复。
	Fail Status = "fail"
)

// Check 是一项预检的声明（名称、说明、失败时的修复建议）。
type Check struct {
	Name        string
	Description string
	// Failure 是失败时的修复建议（remediation）。
	Failure string
}

// Result 是单项预检的执行结果。
type Result struct {
	Name        string
	Status      Status
	Message     string
	Remediation string
}

// 预检项名称。
const (
	CheckPorts     = "ports"
	CheckResources = "resources"
	CheckKernel    = "kernel"
	CheckCgroup    = "cgroup"
	CheckContainer = "container"
	CheckSwap      = "swap"
	CheckSELinux   = "selinux"
	CheckFirewall  = "firewall"
	CheckCommands  = "commands"
)

// 预检阈值默认值。
const (
	// DefaultMinCPUs 是最小 CPU 核数。
	DefaultMinCPUs = 2
	// DefaultMinMemoryBytes 是最小内存（2 GiB）。
	DefaultMinMemoryBytes uint64 = 2 << 30
	// DefaultMinFreeBytes 是数据目录最小可用空间（10 GiB）。
	DefaultMinFreeBytes uint64 = 10 << 30
	// DefaultMinInodes 是数据目录最小可用 inode 数。
	DefaultMinInodes uint64 = 10000
	// DefaultMinKernelMajor / DefaultMinKernelMinor 是最低内核版本。
	DefaultMinKernelMajor = 5
	DefaultMinKernelMinor = 4
	// DefaultStreamKernelMajor / DefaultStreamKernelMinor 是 RHEL/Rocky 8
	// 系列建议达到的内核版本（低于它只 Warn，因为 8 系列使用回移植内核）。
	DefaultStreamKernelMajor = 5
	DefaultStreamKernelMinor = 8
	// MinNFTablesKernelMajor / MinNFTablesKernelMinor 是 kube-proxy nftables
	// 模式的最小内核版本（上游要求 Linux 5.13+）。
	MinNFTablesKernelMajor = 5
	MinNFTablesKernelMinor = 13
)

// DefaultRequiredCommands 是 host init 依赖的命令。
var DefaultRequiredCommands = []string{"systemctl", "modprobe", "sysctl"}

// DefaultDataDirs 是需要检查可用空间的数据目录。
var DefaultDataDirs = []string{constants.KubeletDir, "/var/lib/containerd"}

// PortSpec 描述一个需要保持空闲或放行的端口。
type PortSpec struct {
	Port      int
	Protocol  string
	Component string
}

// portCatalog 是端口清单的唯一目录：每条声明"什么角色/什么形态需要它"。
// DefaultPorts 与 PortsFor 都从这张表派生，避免两份字面量清单漂移
// （例如 etcd metrics 端口改一处漏一处）。
type portEntry struct {
	spec  PortSpec
	roles func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool
}

// portCatalog 的顺序即附录 A 的展示顺序。
func portCatalog() []portEntry {
	return []portEntry{
		{PortSpec{constants.APIServerPort, "tcp", "kube-apiserver"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool { return host.IsMaster() }},
		{PortSpec{constants.APIServerPort, "tcp", "本地代理"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool {
				return host.IsWorker() && cfg.ControlPlane.LocalProxyEnabled()
			}},
		{PortSpec{constants.EtcdClientPort, "tcp", "etcd client"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool {
				return host.IsMaster() && !cfg.ControlPlane.Etcd.IsExternal()
			}},
		{PortSpec{constants.EtcdPeerPort, "tcp", "etcd peer"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool {
				return host.IsMaster() && !cfg.ControlPlane.Etcd.IsExternal()
			}},
		{PortSpec{constants.EtcdMetricsPort, "tcp", "etcd metrics/health"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool {
				return host.IsMaster() && !cfg.ControlPlane.Etcd.IsExternal()
			}},
		{PortSpec{constants.KubeletPort, "tcp", "kubelet"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool { return host.IsWorker() }},
		{PortSpec{constants.KubeletHealthzPort, "tcp", "kubelet healthz"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool { return host.IsWorker() }},
		{PortSpec{constants.ControllerManagerPort, "tcp", "kube-controller-manager"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool { return host.IsMaster() }},
		{PortSpec{constants.SchedulerPort, "tcp", "kube-scheduler"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool { return host.IsMaster() }},
		{PortSpec{constants.BGPPort, "tcp", "kube-router BGP"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool {
				return host.IsWorker() && runRouter(cfg)
			}},
		{PortSpec{constants.KubeRouterHealthzPort, "tcp", "kube-router healthz"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool {
				return host.IsWorker() && cfg.Network.Plugin == clusterconfig.PluginKubeRouter
			}},
		// IP 协议 4（IPIP）不是 socket 端口，用 Port=0、Protocol="ipip" 表示，
		// 端口检查会跳过它。
		{PortSpec{0, "ipip", "kube-router overlay（IP 协议 4）"},
			func(cfg *clusterconfig.Cluster, host *clusterconfig.Host) bool {
				return host.IsWorker() && overlayEnabled(cfg)
			}},
	}
}

// runRouter 报告 kube-router 是否运行路由转发。
func runRouter(cfg *clusterconfig.Cluster) bool {
	kubeRouter := cfg.Network.Plugin == clusterconfig.PluginKubeRouter
	return kubeRouter && (cfg.Network.KubeRouter.RunRouter == nil || *cfg.Network.KubeRouter.RunRouter)
}

// overlayEnabled 报告是否启用 IPIP overlay。
func overlayEnabled(cfg *clusterconfig.Cluster) bool {
	kubeRouter := cfg.Network.Plugin == clusterconfig.PluginKubeRouter
	return kubeRouter && (cfg.Network.KubeRouter.EnableOverlay == nil || *cfg.Network.KubeRouter.EnableOverlay)
}

// DefaultPorts 返回附录 A 的完整端口清单（全部角色并集）。
func DefaultPorts() []PortSpec {
	out := make([]PortSpec, 0, len(portCatalog()))
	for _, e := range portCatalog() {
		out = append(out, e.spec)
	}
	return out
}

// PortsFor 返回本节点角色实际会用到的端口。
// 不做全集群并集检查：worker 上的 etcd/BGP 端口是否有别的进程占用，
// 与它能否加入集群无关；过度检查只会制造误报。
func PortsFor(cfg *clusterconfig.Cluster, host *clusterconfig.Host) []PortSpec {
	if cfg == nil || host == nil {
		return DefaultPorts()
	}
	out := make([]PortSpec, 0, 8)
	for _, e := range portCatalog() {
		if e.roles(cfg, host) {
			out = append(out, e.spec)
		}
	}
	return out
}

// PortChecker 检查端口是否被占用，必须可注入以便测试。
type PortChecker interface {
	// Free 报告本机是否没有进程监听该端口。
	Free(ctx context.Context, protocol string, port int) (bool, error)
}

// hostPortChecker 是 PortChecker 的真实主机实现。
type hostPortChecker struct{}

// NewPortChecker 返回检查真实主机的 PortChecker。
func NewPortChecker() PortChecker { return hostPortChecker{} }

// Free 通过尝试 bind 判断端口是否空闲。
func (hostPortChecker) Free(ctx context.Context, protocol string, port int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	switch strings.ToLower(protocol) {
	case "tcp", "":
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				return false, nil
			}
			return false, err
		}
		return true, ln.Close()
	case "udp":
		pc, err := net.ListenPacket("udp", net.JoinHostPort("", strconv.Itoa(port)))
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				return false, nil
			}
			return false, err
		}
		return true, pc.Close()
	default:
		// 非 socket 协议（如 IPIP）没有端口占用概念。
		return true, nil
	}
}

// CommandLookup 查询命令是否存在，必须可注入以便测试。
type CommandLookup interface {
	LookPath(name string) (string, error)
}

// hostCommandLookup 是基于 PATH 的真实实现。
type hostCommandLookup struct{}

// NewCommandLookup 返回基于 PATH 的 CommandLookup。
func NewCommandLookup() CommandLookup { return hostCommandLookup{} }

// LookPath 在 PATH 中查找命令。
func (hostCommandLookup) LookPath(name string) (string, error) { return exec.LookPath(name) }

// StatFSFunc 读取路径所在文件系统的可用空间。
type StatFSFunc func(path string) (freeBytes uint64, freeInodes uint64, err error)

// PreflightOptions 是预检的输入。零值字段会使用上面定义的默认阈值与真实实现。
type PreflightOptions struct {
	// Facts 是主机事实（必填）。
	Facts *Facts
	// DataDirs 是需要检查空间的数据目录。
	DataDirs []string
	// Ports 是需要检查的端口（默认附录 A）。
	Ports []PortSpec
	// RequiredCommands 是必须存在的命令。
	RequiredCommands []string
	// KubeProxyMode 是 network.kubeProxy.mode；nftables 模式会额外校验内核。
	KubeProxyMode string
	// AllowCgroupV1 允许 cgroup v1 继续（仅 Kubernetes < 1.35）。
	// 1.35 起 kubelet 默认 failCgroupV1=true，v1 节点上 kubelet 不会启动，
	// 因此默认必须 Fail；只有明确使用旧版本时才允许降级为 Warn。
	AllowCgroupV1 bool

	// 注入点。
	PortChecker   PortChecker
	CommandLookup CommandLookup
	StatFS        StatFSFunc

	// 阈值，零值表示使用默认值。
	MinCPUs        int
	MinMemoryBytes uint64
	MinFreeBytes   uint64
	MinInodes      uint64
}

// checkSpec 是预检项的唯一定义：名称、说明、修复建议与执行函数。
type checkSpec struct {
	name        string
	description string
	failure     string
	run         func(*PreflightOptions, context.Context) Result
}

// remediations 是每项检查的修复建议；表与检查函数分开，避免 check 函数
// 经 result 反向依赖这张表。
var remediations = map[string]string{
	CheckPorts:     "停止占用端口的进程，或调整端口后再安装",
	CheckResources: "扩容 CPU/内存，或清理数据目录所在磁盘后重试",
	CheckKernel:    "升级内核（最低 5.4；kube-proxy nftables 模式需要 5.13+；RHEL/Rocky 8 系列请确认回移植特性）",
	CheckCgroup:    "在 /etc/default/grub 增加 systemd.unified_cgroup_hierarchy=1 并重启",
	CheckContainer: "在物理机或虚拟机上安装，不要在容器里执行 host init",
	CheckSwap:      "如需关闭，自行 swapoff -a 并注释 /etc/fstab，或用 cluster.hooks；kubemono 不代改",
	CheckSELinux:   "kubemono 不修改 SELinux；Pod 卷/网络异常时检查策略",
	CheckFirewall:  "按清单放行端口，或自行关闭防火墙；kubemono 不代改",
	CheckCommands:  "安装缺失命令所在的软件包",
}

// preflightChecks 是声明顺序即执行顺序的唯一表；DeclaredChecks 与
// RunPreflight 都从它派生，不会再出现“表里有一项、调用里漏一项”的情况。
var preflightChecks = []checkSpec{
	{CheckPorts, "附录 A 的端口未被占用", remediations[CheckPorts], (*PreflightOptions).checkPorts},
	{CheckResources, "CPU、内存、数据目录可用空间与 inode 满足下限", remediations[CheckResources], func(o *PreflightOptions, _ context.Context) Result { return o.checkResources() }},
	{CheckKernel, "内核版本满足要求", remediations[CheckKernel], func(o *PreflightOptions, _ context.Context) Result { return o.checkKernel() }},
	{CheckCgroup, "优先使用 cgroup v2", remediations[CheckCgroup], func(o *PreflightOptions, _ context.Context) Result { return o.checkCgroup() }},
	{CheckContainer, "本机不是容器环境", remediations[CheckContainer], func(o *PreflightOptions, _ context.Context) Result { return o.checkContainer() }},
	{CheckSwap, "swap 状态提示（kubelet 已容忍 swap）", remediations[CheckSwap], func(o *PreflightOptions, _ context.Context) Result { return o.checkSwap() }},
	{CheckSELinux, "SELinux 状态提示（kubemono 不修改）", remediations[CheckSELinux], func(o *PreflightOptions, _ context.Context) Result { return o.checkSELinux() }},
	{CheckFirewall, "活动防火墙的端口放行提示", remediations[CheckFirewall], (*PreflightOptions).checkFirewall},
	{CheckCommands, "host init 依赖的命令存在", remediations[CheckCommands], func(o *PreflightOptions, _ context.Context) Result { return o.checkCommands() }},
}

// DeclaredChecks 返回按执行顺序声明的全部检查项。
func DeclaredChecks() []Check {
	out := make([]Check, 0, len(preflightChecks))
	for _, c := range preflightChecks {
		out = append(out, Check{Name: c.name, Description: c.description, Failure: c.failure})
	}
	return out
}

// RunPreflight 执行全部预检，返回与 DeclaredChecks 顺序一致的结果。
// Facts 为空时所有检查都返回 Fail（宁可阻塞，也不要带着未知状态继续安装）。
func RunPreflight(ctx context.Context, opts *PreflightOptions) []Result {
	if opts == nil || opts.Facts == nil {
		results := make([]Result, 0, len(preflightChecks))
		for _, c := range preflightChecks {
			results = append(results, Result{
				Name: c.name, Status: Fail, Message: "缺少主机事实（facts）", Remediation: c.failure,
			})
		}
		return results
	}
	o := opts.withDefaults()
	results := make([]Result, 0, len(preflightChecks))
	for _, c := range preflightChecks {
		results = append(results, c.run(o, ctx))
	}
	return results
}

// Summarize 汇总失败与警告数量。
func Summarize(results []Result) (failed int, warned int) {
	for _, r := range results {
		switch r.Status {
		case Fail:
			failed++
		case Warn:
			warned++
		}
	}
	return failed, warned
}

// withDefaults 返回补齐默认值的副本（不修改调用方对象）。
func (o *PreflightOptions) withDefaults() *PreflightOptions {
	out := *o
	if len(out.Ports) == 0 {
		out.Ports = DefaultPorts()
	}
	if len(out.DataDirs) == 0 {
		out.DataDirs = append([]string{}, DefaultDataDirs...)
	}
	if len(out.RequiredCommands) == 0 {
		out.RequiredCommands = append([]string{}, DefaultRequiredCommands...)
	}
	if out.PortChecker == nil {
		out.PortChecker = NewPortChecker()
	}
	if out.CommandLookup == nil {
		out.CommandLookup = NewCommandLookup()
	}
	if out.StatFS == nil {
		out.StatFS = osCollector{}.StatFS
	}
	if out.MinCPUs == 0 {
		out.MinCPUs = DefaultMinCPUs
	}
	if out.MinMemoryBytes == 0 {
		out.MinMemoryBytes = DefaultMinMemoryBytes
	}
	if out.MinFreeBytes == 0 {
		out.MinFreeBytes = DefaultMinFreeBytes
	}
	if out.MinInodes == 0 {
		out.MinInodes = DefaultMinInodes
	}
	return &out
}

// result 构造结果，并在非 Pass 时带上声明的修复建议。
func result(name string, status Status, message string) Result {
	r := Result{Name: name, Status: status, Message: message}
	if status != Pass {
		r.Remediation = remediations[name]
	}
	return r
}

// checkPorts 检查附录 A 的端口是否空闲。
