package hostinit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// FakeCollector 是内存版 Collector，用于离线测试 facts 的采集逻辑。
type FakeCollector struct {
	Files      map[string]string
	Commands   map[string]string
	CommandErr map[string]error
}

// NewFakeCollector 返回带有空映射的 FakeCollector。
func NewFakeCollector() *FakeCollector {
	return &FakeCollector{
		Files:      map[string]string{},
		Commands:   map[string]string{},
		CommandErr: map[string]error{},
	}
}

// ReadFile 从内存映射读取文件。
func (f *FakeCollector) ReadFile(p string) ([]byte, error) {
	if data, ok := f.Files[p]; ok {
		return []byte(data), nil
	}
	return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
}

// Glob 用 path.Match 匹配内存中的文件。
func (f *FakeCollector) Glob(pattern string) ([]string, error) {
	var out []string
	for p := range f.Files {
		if ok, err := path.Match(pattern, p); err == nil && ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Run 返回预设的命令输出。
func (f *FakeCollector) Run(ctx context.Context, name string, args ...string) (string, error) {
	key := name
	if len(args) > 0 {
		key = name + " " + strings.Join(args, " ")
	}
	if err, ok := f.CommandErr[key]; ok {
		return "", err
	}
	if out, ok := f.Commands[key]; ok {
		return out, nil
	}
	return "", fmt.Errorf("%s: %w", key, ErrUnsupported)
}

const rockyOSRelease = `NAME="Rocky Linux"
VERSION="8.9 (Green Obsidian)"
ID="rocky"
ID_LIKE="rhel centos fedora"
VERSION_ID="8.9"
PLATFORM_ID="platform:el8"
PRETTY_NAME="Rocky Linux 8.9 (Green Obsidian)"
`

const ubuntuOSRelease = `PRETTY_NAME="Ubuntu 22.04.4 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
ID=ubuntu
ID_LIKE=debian
`

const kylinOSRelease = `NAME="Kylin Linux Advanced Server"
VERSION="V10 (Sword)"
ID="kylin"
ID_LIKE="rhel fedora"
VERSION_ID="V10"
PRETTY_NAME="Kylin Linux Advanced Server V10 (Sword)"
`

func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			name:  "rocky",
			input: rockyOSRelease,
			want: map[string]string{
				"NAME": "Rocky Linux", "ID": "rocky", "ID_LIKE": "rhel centos fedora",
				"VERSION_ID": "8.9", "PRETTY_NAME": "Rocky Linux 8.9 (Green Obsidian)",
			},
		},
		{
			name:  "unquoted",
			input: ubuntuOSRelease,
			want:  map[string]string{"ID": "ubuntu", "ID_LIKE": "debian", "VERSION_ID": "22.04"},
		},
		{
			name:  "comments and quotes",
			input: "# comment\n\nNAME='Single Quoted'\nKEY=\"escaped \\\"quote\\\"\"\nEMPTY=\n",
			want:  map[string]string{"NAME": "Single Quoted", "KEY": `escaped "quote"`, "EMPTY": ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOSRelease([]byte(tc.input))
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("parseOSRelease()[%q] = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestOSFamily(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"rocky", rockyOSRelease, OSFamilyRHEL},
		{"ubuntu", ubuntuOSRelease, OSFamilyDebian},
		{"kylin", kylinOSRelease, OSFamilyRHEL},
		{"id by id_like only", "ID=myos\nID_LIKE=\"debian ubuntu\"\n", OSFamilyDebian},
		{"unknown", "ID=frobnix\n", OSFamilyOther},
		{"empty", "", OSFamilyOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := osFamily(parseOSRelease([]byte(tc.input))); got != tc.want {
				t.Fatalf("osFamily() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseKernelVersion(t *testing.T) {
	tests := []struct {
		input string
		major int
		minor int
		ok    bool
	}{
		{"5.14.0-362.el9.x86_64", 5, 14, true},
		{"4.18.0-513.el8.x86_64", 4, 18, true},
		{"v6.1", 6, 1, true},
		{"5.10.0-153.12.0.2.uel20.x86_64", 5, 10, true},
		{"5", 0, 0, false},
		{"", 0, 0, false},
		{"abc.def", 0, 0, false},
	}
	for _, tc := range tests {
		major, minor, ok := parseKernelVersion(tc.input)
		if ok != tc.ok || major != tc.major || minor != tc.minor {
			t.Errorf("parseKernelVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.input, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

func TestFactsHelpers(t *testing.T) {
	f := &Facts{OSVersion: "8.9", KernelMajorMinor: "5.14"}
	if got := f.OSMajorVersion(); got != 8 {
		t.Fatalf("OSMajorVersion() = %d, want 8", got)
	}
	if got := (*Facts)(nil).OSMajorVersion(); got != 0 {
		t.Fatalf("nil Facts OSMajorVersion() = %d", got)
	}
}

func TestMeminfoHelpers(t *testing.T) {
	if kb, ok := meminfoKB("MemTotal:       16316428 kB\n", "MemTotal"); !ok || kb != 16316428 {
		t.Fatalf("meminfoKB() = %d, %v", kb, ok)
	}
	if _, ok := meminfoKB("MemFree: 1 kB\n", "MemTotal"); ok {
		t.Fatalf("meminfoKB 不应命中缺失字段")
	}
	if !hasSwapEntries("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n/dev/sda2   partition\t2097148\t0\t-2\n") {
		t.Fatalf("hasSwapEntries 应识别 swap 条目")
	}
	if hasSwapEntries("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n") {
		t.Fatalf("只有表头时不应认为启用了 swap")
	}
	if got := countProcessors("processor\t: 0\nmodel name\t: x\nprocessor\t: 1\n"); got != 2 {
		t.Fatalf("countProcessors() = %d, want 2", got)
	}
}

func TestCollectFullHost(t *testing.T) {
	fc := NewFakeCollector()
	fc.Files[etcOSRelease] = rockyOSRelease
	fc.Files[procSysKernelRelease] = "5.14.0-362.el9.x86_64\n"
	fc.Files[procCPUInfo] = "processor\t: 0\nprocessor\t: 1\nprocessor\t: 2\nprocessor\t: 3\n"
	fc.Files[procMeminfo] = "MemTotal:       16316428 kB\nSwapTotal:             0 kB\n"
	fc.Files[sysSELinuxEnforce] = "0\n"
	fc.Files[sysCgroupRoot+"/cgroup.controllers"] = "cpuset cpu io memory\n"
	fc.Commands["systemctl is-active firewalld"] = "active\n"

	facts, err := Collect(context.Background(), fc)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if facts.OSName != "Rocky Linux" || facts.OSFamily != OSFamilyRHEL || facts.OSVersion != "8.9" {
		t.Errorf("OS = %q/%q/%q", facts.OSName, facts.OSFamily, facts.OSVersion)
	}
	if facts.KernelVersion != "5.14.0-362.el9.x86_64" || facts.KernelMajorMinor != "5.14" {
		t.Errorf("Kernel = %q/%q", facts.KernelVersion, facts.KernelMajorMinor)
	}
	if facts.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4", facts.CPUs)
	}
	if facts.MemoryBytes != 16316428*1024 {
		t.Errorf("MemoryBytes = %d", facts.MemoryBytes)
	}
	if facts.CgroupVersion != CgroupV2 {
		t.Errorf("CgroupVersion = %q", facts.CgroupVersion)
	}
	if facts.SwapEnabled {
		t.Errorf("SwapEnabled = true, want false")
	}
	if facts.SELinuxMode != SELinuxPermissive {
		t.Errorf("SELinuxMode = %q", facts.SELinuxMode)
	}
	if facts.FirewallBackend != FirewallBackendFirewalld {
		t.Errorf("FirewallBackend = %q", facts.FirewallBackend)
	}
	if facts.Containerized {
		t.Errorf("Containerized = true")
	}
}

func TestCollectDegradesWithoutLinuxSupport(t *testing.T) {
	fc := NewFakeCollector()
	fc.Files[etcOSRelease] = ubuntuOSRelease

	facts, err := Collect(context.Background(), fc)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if facts.OSFamily != OSFamilyDebian {
		t.Errorf("OSFamily = %q", facts.OSFamily)
	}
	if facts.CgroupVersion != CgroupUnknown {
		t.Errorf("CgroupVersion = %q", facts.CgroupVersion)
	}
	if facts.OSFamily == OSFamilyOther && facts.OSName == "" {
		t.Errorf("os-release 应当被解析")
	}
	if facts.CPUs != runtime.NumCPU() {
		t.Errorf("CPUs = %d, want runtime.NumCPU()", facts.CPUs)
	}
}

func TestCollectCgroupDetection(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "v2 controllers",
			files: map[string]string{sysCgroupRoot + "/cgroup.controllers": "cpu"},
			want:  CgroupV2,
		},
		{
			name:  "v2 self cgroup",
			files: map[string]string{procSelfCgroup: "0::/system.slice/containerd.service\n"},
			want:  CgroupV2,
		},
		{
			name:  "v1 memory hierarchy",
			files: map[string]string{sysCgroupRoot + "/memory/cgroup.procs": "1\n"},
			want:  CgroupV1,
		},
		{
			name:  "v1 self cgroup",
			files: map[string]string{procSelfCgroup: "12:devices:/\n11:cpu,cpuacct:/\n"},
			want:  CgroupV1,
		},
		{
			name:  "v1 proc cgroups",
			files: map[string]string{procCgroups: "#subsys_name\thierarchy\n"},
			want:  CgroupV1,
		},
		{
			name:  "unknown",
			files: map[string]string{},
			want:  CgroupUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := NewFakeCollector()
			maps.Copy(fc.Files, tc.files)
			if got := detectCgroupVersion(fc); got != tc.want {
				t.Fatalf("detectCgroupVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCollectSwapDetection(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		command string
		want    bool
	}{
		{"meminfo", map[string]string{procMeminfo: "SwapTotal:       2097148 kB\n"}, "", true},
		{"proc swaps", map[string]string{procSwaps: "Filename\t\t\t\tType\t\tSize\n/dev/sda2 partition 100 0 -2\n"}, "", true},
		{"swapon", map[string]string{}, "/dev/sda2 partition 2G 0B -2\n", true},
		{"none", map[string]string{procMeminfo: "SwapTotal:             0 kB\n"}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := NewFakeCollector()
			maps.Copy(fc.Files, tc.files)
			if tc.command != "" {
				fc.Commands["swapon --noheadings --show"] = tc.command
			}
			facts, err := Collect(context.Background(), fc)
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if facts.SwapEnabled != tc.want {
				t.Fatalf("SwapEnabled = %v, want %v", facts.SwapEnabled, tc.want)
			}
		})
	}
}

func TestCollectFirewallBackend(t *testing.T) {
	tests := []struct {
		name     string
		commands map[string]string
		want     string
	}{
		{"firewalld", map[string]string{"systemctl is-active firewalld": "active\n"}, FirewallBackendFirewalld},
		{"ufw", map[string]string{"ufw status": "Status: active\n"}, FirewallBackendUFW},
		{"iptables", map[string]string{"iptables -S": "-P INPUT ACCEPT\n-A INPUT -j KUBE-FIREWALL\n"}, FirewallBackendIptables},
		{"iptables default only is none", map[string]string{"iptables -S": "-P INPUT ACCEPT\n-P FORWARD ACCEPT\n"}, FirewallBackendNone},
		{"nftables", map[string]string{"nft list ruleset": "table inet filter {\n}\n"}, FirewallBackendNFTables},
		{"none", map[string]string{}, FirewallBackendNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := NewFakeCollector()
			maps.Copy(fc.Commands, tc.commands)
			if got := detectFirewallBackend(context.Background(), fc); got != tc.want {
				t.Fatalf("detectFirewallBackend() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCollectContainerized(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		commands map[string]string
		want     bool
	}{
		{"dockerenv", map[string]string{dockerEnvMarker: ""}, nil, true},
		{"containerenv", map[string]string{envContainerMarker: ""}, nil, true},
		{"proc cgroup", map[string]string{"/proc/1/cgroup": "1:name=systemd:/kubepods/besteffort/pod1\n"}, nil, true},
		{"proc environ", map[string]string{"/proc/1/environ": "PATH=/usr/bin\x00container=podman\x00"}, nil, true},
		{"systemd-detect-virt", nil, map[string]string{"systemd-detect-virt --container": "docker\n"}, true},
		{"bare metal", map[string]string{"/proc/1/cgroup": "0::/init.scope\n"}, map[string]string{"systemd-detect-virt --container": "none\n"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := NewFakeCollector()
			maps.Copy(fc.Files, tc.files)
			maps.Copy(fc.Commands, tc.commands)
			if got := detectContainerized(context.Background(), fc); got != tc.want {
				t.Fatalf("detectContainerized() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCollectSELinuxDetection(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		commands map[string]string
		want     string
	}{
		{"enforce file", map[string]string{sysSELinuxEnforce: "1\n"}, nil, SELinuxEnforcing},
		{"config file", map[string]string{etcSELinuxConfig: "SELINUX=disabled\nSELINUXTYPE=targeted\n"}, nil, SELinuxDisabled},
		{"getenforce", nil, map[string]string{"getenforce": "Permissive\n"}, SELinuxPermissive},
		{"absent", map[string]string{}, nil, SELinuxDisabled},
		{"unknown with selinux dir", map[string]string{sysSELinuxDir: ""}, nil, SELinuxUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := NewFakeCollector()
			maps.Copy(fc.Files, tc.files)
			maps.Copy(fc.Commands, tc.commands)
			if got := detectSELinuxMode(context.Background(), fc); got != tc.want {
				t.Fatalf("detectSELinuxMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCollectErrors(t *testing.T) {
	if _, err := Collect(context.Background(), nil); err == nil {
		t.Fatalf("nil Collector 应当报错")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Collect(ctx, NewFakeCollector()); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应当返回 context.Canceled，得到 %v", err)
	}
}
