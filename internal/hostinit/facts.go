// Package hostinit 实现主机初始化：主机事实采集（facts）、
// 预检（precheck）、声明式初始化计划（plan）以及可精确还原的变更记录
// 所有会改动主机的动作都通过 Applier 接口注入，所有读取主机状态的动作都通过
// Collector / PortChecker / CommandLookup / NTPChecker 注入，因此本包在
// darwin 上可以完整构建并运行纯单元测试。
// 术语：
// - facts：主机事实，只读；
// - precheck：安装前检查，只读；
// - plan：声明式步骤列表（纯数据），由 Apply 执行；
// - journal：变更记录，Reset 依据它精确还原。
package hostinit

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
)

// ErrUnsupported 表示当前平台不支持该操作。
// 例如 darwin 上没有 /proc、/sys，也无法执行 systemctl。
// 调用方可以用 errors.Is(err, ErrUnsupported) 判断“无法确定”而不是“失败”。
var ErrUnsupported = errors.New("hostinit: 当前平台不支持该操作")

// OS family 取值（Facts.OSFamily）。
const (
	// OSFamilyRHEL 表示 RHEL 系（含 CentOS、Rocky、AlmaLinux、openEuler、麒麟等）。
	OSFamilyRHEL = "rhel"
	// OSFamilyDebian 表示 Debian 系（含 Ubuntu、UOS、Deepin 等）。
	OSFamilyDebian = "debian"
	// OSFamilyOther 表示无法归类的发行版。
	OSFamilyOther = "other"
)

// cgroup 版本取值（Facts.CgroupVersion）。
const (
	// CgroupV1 表示 cgroup v1。
	CgroupV1 = "v1"
	// CgroupV2 表示 cgroup v2（统一层级）。
	CgroupV2 = "v2"
	// CgroupUnknown 表示无法识别。
	CgroupUnknown = "unknown"
)

// 防火墙后端取值（Facts.FirewallBackend）。
const (
	FirewallBackendFirewalld = "firewalld"
	FirewallBackendUFW       = "ufw"
	FirewallBackendIptables  = "iptables"
	FirewallBackendNFTables  = "nftables"
	FirewallBackendNone      = "none"
)

// SELinux 模式取值（Facts.SELinuxMode）。
const (
	SELinuxEnforcing  = "enforcing"
	SELinuxPermissive = "permissive"
	SELinuxDisabled   = "disabled"
	SELinuxUnknown    = "unknown"
)

// hostinit 使用的固定路径。集中定义便于测试与审计。
const (
	etcOSRelease         = "/etc/os-release"
	procSysKernelRelease = "/proc/sys/kernel/osrelease"
	procCPUInfo          = "/proc/cpuinfo"
	procMeminfo          = "/proc/meminfo"
	procSwaps            = "/proc/swaps"
	procSelfCgroup       = "/proc/self/cgroup"
	procCgroups          = "/proc/cgroups"
	sysCgroupRoot        = "/sys/fs/cgroup"
	sysSELinuxDir        = "/sys/fs/selinux"
	sysSELinuxEnforce    = "/sys/fs/selinux/enforce"
	etcSELinuxConfig     = "/etc/selinux/config"
	envContainerMarker   = "/run/.containerenv"
	dockerEnvMarker      = "/.dockerenv"
)

// Collector 抽象主机事实的来源：文件读取与命令执行。
// 真实主机实现由 NewCollector 返回；测试使用内存实现，
// 因此 facts 的采集逻辑本身可以完全离线测试。
type Collector interface {
	// ReadFile 读取文件；文件不存在时返回包装了 fs.ErrNotExist 的错误。
	ReadFile(path string) ([]byte, error)
	// Glob 返回匹配的文件路径，结果已排序。
	Glob(pattern string) ([]string, error)
	// Run 执行命令并返回合并后的输出；平台不支持时返回 ErrUnsupported。
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Facts 是主机事实的快照。
// 只保留 precheck / plan 真正读取的字段：采集本身有成本，不要收集没人消费的数据。
type Facts struct {
	OSName           string `json:"osName,omitempty"`
	OSFamily         string `json:"osFamily"` // rhel|debian|other 之一
	OSVersion        string `json:"osVersion,omitempty"`
	KernelVersion    string `json:"kernelVersion,omitempty"`
	KernelMajorMinor string `json:"kernelMajorMinor,omitempty"`
	CPUs             int    `json:"cpus"`
	MemoryBytes      uint64 `json:"memoryBytes"`
	CgroupVersion    string `json:"cgroupVersion"` // v1|v2|unknown 之一
	SwapEnabled      bool   `json:"swapEnabled"`
	SELinuxMode      string `json:"selinuxMode"`
	// FirewallBackend 取 firewalld|ufw|iptables|nftables|none。
	FirewallBackend string `json:"firewallBackend"`
	Containerized   bool   `json:"containerized"`
}

// OSMajorVersion 返回 OS 版本号的主版本（"8.9" → 8），无法解析时返回 0。
func (f *Facts) OSMajorVersion() int {
	if f == nil {
		return 0
	}
	v := digitsPrefix(strings.TrimSpace(f.OSVersion))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// Collect 采集主机事实。
// 单项读取失败不会让整体失败：无法获得的字段留空或为 0，由 precheck 判定。
// 只有 Collector 为 nil 或 ctx 已取消时才返回错误。
func Collect(ctx context.Context, c Collector) (*Facts, error) {
	if c == nil {
		return nil, errors.New("hostinit: Collector 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f := &Facts{
		CPUs:            runtime.NumCPU(),
		OSFamily:        OSFamilyOther,
		CgroupVersion:   CgroupUnknown,
		SELinuxMode:     SELinuxUnknown,
		FirewallBackend: FirewallBackendNone,
	}

	// 发行版：解析 /etc/os-release。
	if data, err := c.ReadFile(etcOSRelease); err == nil {
		osr := parseOSRelease(data)
		f.OSName = firstNonEmpty(osr["NAME"], osr["PRETTY_NAME"], osr["ID"])
		f.OSFamily = osFamily(osr)
		f.OSVersion = firstNonEmpty(osr["VERSION_ID"], osr["VERSION"])
	}

	// 内核版本。
	f.KernelVersion = firstNonEmpty(readTrimmed(c, procSysKernelRelease), runTrimmed(ctx, c, "uname", "-r"))
	if major, minor, ok := parseKernelVersion(f.KernelVersion); ok {
		f.KernelMajorMinor = strconv.Itoa(major) + "." + strconv.Itoa(minor)
	}

	// CPU 与内存。
	if data, err := c.ReadFile(procCPUInfo); err == nil {
		if n := countProcessors(string(data)); n > 0 {
			f.CPUs = n
		}
	}
	if data, err := c.ReadFile(procMeminfo); err == nil {
		meminfo := string(data)
		if kb, ok := meminfoKB(meminfo, "MemTotal"); ok {
			f.MemoryBytes = kb * 1024
		}
		if kb, ok := meminfoKB(meminfo, "SwapTotal"); ok && kb > 0 {
			f.SwapEnabled = true
		}
	}

	f.CgroupVersion = detectCgroupVersion(c)

	// swap：/proc/meminfo 没结论时看 /proc/swaps 和 swapon 命令。
	if !f.SwapEnabled {
		if data, err := c.ReadFile(procSwaps); err == nil {
			f.SwapEnabled = hasSwapEntries(string(data))
		}
	}
	if !f.SwapEnabled {
		f.SwapEnabled = runTrimmed(ctx, c, "swapon", "--noheadings", "--show") != ""
	}

	f.SELinuxMode = detectSELinuxMode(ctx, c)
	f.FirewallBackend = detectFirewallBackend(ctx, c)
	f.Containerized = detectContainerized(ctx, c)

	return f, nil
}

// detectCgroupVersion 判断 cgroup 版本。
func detectCgroupVersion(c Collector) string {
	if m, _ := c.Glob(sysCgroupRoot + "/cgroup.controllers"); len(m) > 0 {
		return CgroupV2
	}
	for _, p := range []string{sysCgroupRoot + "/memory/cgroup.procs", sysCgroupRoot + "/cpu/cgroup.procs"} {
		if m, _ := c.Glob(p); len(m) > 0 {
			return CgroupV1
		}
	}
	if self := readTrimmed(c, procSelfCgroup); self != "" {
		for line := range strings.SplitSeq(self, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "0::") {
				return CgroupV2
			}
		}
		if strings.Count(self, ":") >= 2 {
			return CgroupV1
		}
	}
	if m, _ := c.Glob(procCgroups); len(m) > 0 {
		return CgroupV1
	}
	return CgroupUnknown
}

// detectSELinuxMode 判断 SELinux 当前模式。
func detectSELinuxMode(ctx context.Context, c Collector) string {
	// getenforce 每次调用都是一个子进程，取值后复用。
	getenforce := func() string {
		return strings.ToLower(strings.TrimSpace(runTrimmed(ctx, c, "getenforce")))
	}
	// /sys/fs/selinux 未挂载时 SELinux 运行时没有启用；/etc/selinux/config
	// 只描述下次启动，不能据此调用 setenforce（Debian 默认没有 SELinux）。
	if _, err := c.ReadFile(sysSELinuxEnforce); err != nil {
		if mode := getenforce(); isSELinuxMode(mode) {
			return mode
		}
		if _, err := c.ReadFile(sysSELinuxDir); err == nil {
			return SELinuxUnknown
		}
		return SELinuxDisabled
	}
	switch strings.TrimSpace(readTrimmed(c, sysSELinuxEnforce)) {
	case "1":
		return SELinuxEnforcing
	case "0":
		return SELinuxPermissive
	}
	if cfg := parseOSRelease(mustRead(c, etcSELinuxConfig)); len(cfg) > 0 {
		if mode := strings.ToLower(strings.TrimSpace(cfg["SELINUX"])); isSELinuxMode(mode) {
			return mode
		}
	}
	if mode := getenforce(); isSELinuxMode(mode) {
		return mode
	}
	// 没有任何 SELinux 痕迹：既可能真的没装，也可能读不到。
	if len(globAny(c, sysSELinuxDir, etcSELinuxConfig)) > 0 {
		return SELinuxUnknown
	}
	return SELinuxDisabled
}

// isSELinuxMode 报告 s 是否是 getenforce / /etc/selinux/config 的合法取值。
func isSELinuxMode(s string) bool {
	switch s {
	case SELinuxEnforcing, SELinuxPermissive, SELinuxDisabled:
		return true
	}
	return false
}

// detectFirewallBackend 判断当前生效的防火墙后端。
// 顺序与 附录 A 一致：firewalld → ufw → iptables → nftables → none。
func detectFirewallBackend(ctx context.Context, c Collector) string {
	if runTrimmed(ctx, c, "systemctl", "is-active", "firewalld") == "active" {
		return FirewallBackendFirewalld
	}
	if strings.Contains(runOutput(ctx, c, "ufw", "status"), "Status: active") {
		return FirewallBackendUFW
	}
	if hasIptablesRules(runOutput(ctx, c, "iptables", "-S")) {
		return FirewallBackendIptables
	}
	if strings.TrimSpace(runOutput(ctx, c, "nft", "list", "ruleset")) != "" {
		return FirewallBackendNFTables
	}
	return FirewallBackendNone
}

// detectContainerized 判断本机是否运行在容器里。
func detectContainerized(ctx context.Context, c Collector) bool {
	if len(globAny(c, dockerEnvMarker, envContainerMarker)) > 0 {
		return true
	}
	cgroup := readTrimmed(c, "/proc/1/cgroup")
	for _, marker := range []string{"docker", "kubepods", "containerd", "lxc", "podman", "garden"} {
		if strings.Contains(cgroup, marker) {
			return true
		}
	}
	if strings.Contains(readTrimmed(c, "/proc/1/environ"), "container=") {
		return true
	}
	if v := runTrimmed(ctx, c, "systemd-detect-virt", "--container"); v != "" && v != "none" {
		return true
	}
	return false
}

// hasIptablesRules 判断 `iptables -S` 的输出里是否有真正的规则（不只是默认策略）。
func hasIptablesRules(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-P ") || line == "-P" {
			continue
		}
		return true
	}
	return false
}

// osFamily 依据 os-release 的 ID / ID_LIKE 归类发行版。
func osFamily(osr map[string]string) string {
	if fam, ok := osFamilyByToken(strings.ToLower(strings.TrimSpace(osr["ID"]))); ok {
		return fam
	}
	for tok := range strings.FieldsSeq(strings.ToLower(osr["ID_LIKE"])) {
		if fam, ok := osFamilyByToken(tok); ok {
			return fam
		}
	}
	return OSFamilyOther
}

// osFamilyByToken 把单个发行版标识映射到 OS family。
// 麒麟（kylin/neokylin）服务器版使用 RPM 包管理，归入 rhel。
func osFamilyByToken(token string) (string, bool) {
	switch token {
	case "rhel", "centos", "centos-stream", "rocky", "almalinux", "alma", "fedora",
		"ol", "oracle", "openeuler", "anolis", "kylin", "neokylin", "bclinux":
		return OSFamilyRHEL, true
	case "debian", "ubuntu", "kali", "linuxmint", "mint", "deepin", "uos", "raspbian":
		return OSFamilyDebian, true
	}
	return "", false
}

// parseOSRelease 解析 os-release 风格的 KEY=VALUE 文本（含引号与转义）。
func parseOSRelease(data []byte) map[string]string {
	out := map[string]string{}
	for raw := range strings.SplitSeq(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if quote := value[0]; (quote == '"' || quote == '\'') && value[len(value)-1] == quote {
				value = value[1 : len(value)-1]
				if quote == '"' {
					value = strings.ReplaceAll(value, `\"`, `"`)
					value = strings.ReplaceAll(value, `\\`, `\`)
				}
			}
		}
		out[key] = value
	}
	return out
}

// parseKernelVersion 解析 "5.10.0-153.el8.x86_64" 形式的内核版本，返回主次版本。
func parseKernelVersion(v string) (major, minor int, ok bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return 0, 0, false
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(digitsPrefix(parts[0]))
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(digitsPrefix(parts[1]))
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// digitsPrefix 返回字符串开头连续的数字。
func digitsPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// meminfoKB 从 /proc/meminfo 中取出以 kB 为单位的字段值。
func meminfoKB(content, key string) (uint64, bool) {
	for line := range strings.SplitSeq(content, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// countProcessors 统计 /proc/cpuinfo 中的 processor 行数。
func countProcessors(content string) int {
	n := 0
	for line := range strings.SplitSeq(content, "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	return n
}

// hasSwapEntries 判断 /proc/swaps 中除表头外是否还有条目。
func hasSwapEntries(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	return len(strings.Split(trimmed, "\n")) > 1
}

// readTrimmed 读取文件并去掉首尾空白；失败返回空串。
func readTrimmed(c Collector, p string) string {
	data, err := c.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// mustRead 读取文件内容；失败返回 nil。
func mustRead(c Collector, p string) []byte {
	data, err := c.ReadFile(p)
	if err != nil {
		return nil
	}
	return data
}

// runOutput 执行命令并返回原始输出；失败返回空串。
func runOutput(ctx context.Context, c Collector, name string, args ...string) string {
	out, err := c.Run(ctx, name, args...)
	if err != nil {
		return ""
	}
	return out
}

// runTrimmed 执行命令并返回去掉空白的输出；失败返回空串。
func runTrimmed(ctx context.Context, c Collector, name string, args ...string) string {
	return strings.TrimSpace(runOutput(ctx, c, name, args...))
}

// globAny 返回所有存在的路径（不存在的忽略）。
func globAny(c Collector, patterns ...string) []string {
	var out []string
	for _, p := range patterns {
		if m, err := c.Glob(p); err == nil {
			out = append(out, m...)
		}
	}
	return out
}

// firstNonEmpty 返回第一个非空值。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
