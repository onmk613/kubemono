package hostinit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	clusterconfig "kubemono/internal/config"
)

type fakePortChecker struct {
	free    map[string]bool
	freeErr map[string]error
	calls   []string
}

func newFakePortChecker() *fakePortChecker {
	return &fakePortChecker{
		free:    map[string]bool{},
		freeErr: map[string]error{},
	}
}

func portKey(protocol string, port int) string { return protocol + "/" + strconv.Itoa(port) }

func (c *fakePortChecker) Free(ctx context.Context, protocol string, port int) (bool, error) {
	key := portKey(protocol, port)
	c.calls = append(c.calls, "free:"+key)
	if err, ok := c.freeErr[key]; ok {
		return false, err
	}
	if v, ok := c.free[key]; ok {
		return v, nil
	}
	return true, nil
}

type fakeCommandLookup struct {
	missing map[string]bool
}

func (l fakeCommandLookup) LookPath(name string) (string, error) {
	if l.missing[name] {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + name, nil
}

func baseOptions() *PreflightOptions {
	facts := &Facts{
		OSName:           "Rocky Linux",
		OSFamily:         OSFamilyRHEL,
		OSVersion:        "9.3",
		KernelVersion:    "5.14.0-362.el9.x86_64",
		KernelMajorMinor: "5.14",
		CPUs:             4,
		MemoryBytes:      16 << 30,
		CgroupVersion:    CgroupV2,
		SELinuxMode:      SELinuxDisabled,
		FirewallBackend:  FirewallBackendNone,
	}
	return &PreflightOptions{
		Facts:         facts,
		PortChecker:   newFakePortChecker(),
		CommandLookup: fakeCommandLookup{},
		StatFS:        func(string) (uint64, uint64, error) { return 100 << 30, 1 << 20, nil },
	}
}

func TestRunPreflightAllPass(t *testing.T) {
	results := RunPreflight(context.Background(), baseOptions())

	declared := DeclaredChecks()
	if len(results) != len(declared) {
		t.Fatalf("结果数量 = %d, want %d", len(results), len(declared))
	}
	for i, r := range results {
		if r.Name != declared[i].Name {
			t.Fatalf("第 %d 项 = %q, want %q", i, r.Name, declared[i].Name)
		}
		if r.Status != Pass {
			t.Fatalf("%s 状态 = %s: %s", r.Name, r.Status, r.Message)
		}
		if r.Message == "" {
			t.Fatalf("%s 缺少说明", r.Name)
		}
	}
	if failed, warned := Summarize(results); failed != 0 || warned != 0 {
		t.Fatalf("Summarize() = %d, %d", failed, warned)
	}
	if err := Evaluate(declared, results); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
}

func TestRunPreflightNilFacts(t *testing.T) {
	results := RunPreflight(context.Background(), nil)
	if len(results) != len(DeclaredChecks()) {
		t.Fatalf("缺少 facts 时应给出全部检查的失败结果")
	}
	for _, r := range results {
		if r.Status != Fail {
			t.Fatalf("%s 应为 Fail", r.Name)
		}
	}
	if err := Evaluate(DeclaredChecks(), results); err == nil {
		t.Fatalf("缺少 facts 时 Evaluate 必须报错")
	}
}

func TestCheckPorts(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*fakePortChecker)
		wantStatus Status
		wantSubstr string
	}{
		{"free", func(*fakePortChecker) {}, Pass, "全部空闲"},
		{"busy", func(c *fakePortChecker) { c.free["tcp/6443"] = false }, Fail, "6443/tcp"},
		{"unknown", func(c *fakePortChecker) { c.freeErr["tcp/2379"] = errors.New("权限不足") }, Warn, "2379/tcp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOptions()
			checker := newFakePortChecker()
			tc.mutate(checker)
			o.PortChecker = checker
			got := o.withDefaults().checkPorts(context.Background())
			if got.Status != tc.wantStatus || !strings.Contains(got.Message, tc.wantSubstr) {
				t.Fatalf("checkPorts() = %+v, want %s 且包含 %q", got, tc.wantStatus, tc.wantSubstr)
			}
			// 6443 在附录 A 中出现两次，只能检查一次。
			seen := map[string]bool{}
			for _, call := range checker.calls {
				if seen[call] {
					t.Fatalf("端口被重复检查: %s", call)
				}
				seen[call] = true
			}
			if len(checker.calls) != 10 {
				t.Fatalf("唯一 socket 端口数 = %d, want 10", len(checker.calls))
			}
		})
	}
}

func TestCheckResources(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*PreflightOptions)
		wantStatus Status
		wantSubstr string
	}{
		{"ok", func(*PreflightOptions) {}, Pass, ""},
		{"cpu low", func(o *PreflightOptions) { o.Facts.CPUs = 1 }, Fail, "CPU"},
		{"memory low", func(o *PreflightOptions) { o.Facts.MemoryBytes = 1 << 30 }, Fail, "内存"},
		{"free low", func(o *PreflightOptions) {
			o.StatFS = func(string) (uint64, uint64, error) { return 5 << 30, 1 << 20, nil }
		}, Fail, "可用空间"},
		{"inode low", func(o *PreflightOptions) {
			o.StatFS = func(string) (uint64, uint64, error) { return 100 << 30, 10, nil }
		}, Fail, "inode"},
		{"unsupported statfs", func(o *PreflightOptions) {
			o.StatFS = func(string) (uint64, uint64, error) { return 0, 0, ErrUnsupported }
		}, Warn, "无法确定"},
		{"unknown cpu and memory", func(o *PreflightOptions) {
			o.Facts.CPUs = 0
			o.Facts.MemoryBytes = 0
		}, Warn, "无法确定 CPU"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOptions()
			tc.mutate(o)
			got := o.withDefaults().checkResources()
			if got.Status != tc.wantStatus || !strings.Contains(got.Message, tc.wantSubstr) {
				t.Fatalf("checkResources() = %+v, want %s 且包含 %q", got, tc.wantStatus, tc.wantSubstr)
			}
		})
	}
}

func TestCheckKernel(t *testing.T) {
	tests := []struct {
		name       string
		family     string
		osVersion  string
		kernel     string
		wantStatus Status
	}{
		{"modern", OSFamilyRHEL, "9.3", "5.14", Pass},
		{"minimum", OSFamilyDebian, "12", "5.4", Pass},
		{"too old", OSFamilyDebian, "11", "5.3", Fail},
		{"rhel8 backport", OSFamilyRHEL, "8.9", "4.18", Warn},
		{"rhel8 below 5.8", OSFamilyRHEL, "8.9", "5.4", Warn},
		{"rhel8 modern kernel", OSFamilyRHEL, "8.9", "6.1", Pass},
		{"unknown", OSFamilyOther, "", "", Warn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOptions()
			o.Facts.OSFamily = tc.family
			o.Facts.OSVersion = tc.osVersion
			o.Facts.KernelMajorMinor = tc.kernel
			o.Facts.KernelVersion = tc.kernel
			got := o.withDefaults().checkKernel()
			if got.Status != tc.wantStatus {
				t.Fatalf("checkKernel() = %+v, want %s", got, tc.wantStatus)
			}
		})
	}
}

// TestCheckKernelNFTables 锁定 kube-proxy nftables 模式的内核下限（5.13）。
func TestCheckKernelNFTables(t *testing.T) {
	tests := []struct {
		kernel     string
		wantStatus Status
	}{
		{"5.4", Fail},
		{"5.12", Fail},
		{"5.13", Pass},
		{"6.1", Pass},
	}
	for _, tc := range tests {
		t.Run(tc.kernel, func(t *testing.T) {
			o := baseOptions()
			o.KubeProxyMode = clusterconfig.KubeProxyModeNFTables
			o.Facts.OSFamily = OSFamilyDebian
			o.Facts.OSVersion = "12"
			o.Facts.KernelMajorMinor = tc.kernel
			o.Facts.KernelVersion = tc.kernel
			got := o.withDefaults().checkKernel()
			if got.Status != tc.wantStatus {
				t.Fatalf("checkKernel(nftables, %s) = %+v, want %s", tc.kernel, got, tc.wantStatus)
			}
		})
	}
}

func TestCheckCgroupContainerSwap(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*PreflightOptions)
		check      func(*PreflightOptions) Result
		wantStatus Status
	}{
		{"cgroup v2", func(*PreflightOptions) {}, func(o *PreflightOptions) Result { return o.checkCgroup() }, Pass},
		{"cgroup v1 rejected", func(o *PreflightOptions) { o.Facts.CgroupVersion = CgroupV1 }, func(o *PreflightOptions) Result { return o.checkCgroup() }, Fail},
		{"cgroup v1 allowed for old k8s", func(o *PreflightOptions) {
			o.Facts.CgroupVersion = CgroupV1
			o.AllowCgroupV1 = true
		}, func(o *PreflightOptions) Result { return o.checkCgroup() }, Warn},
		{"cgroup unknown", func(o *PreflightOptions) { o.Facts.CgroupVersion = CgroupUnknown }, func(o *PreflightOptions) Result { return o.checkCgroup() }, Fail},
		{"container", func(o *PreflightOptions) { o.Facts.Containerized = true }, func(o *PreflightOptions) Result { return o.checkContainer() }, Warn},
		{"not container", func(*PreflightOptions) {}, func(o *PreflightOptions) Result { return o.checkContainer() }, Pass},
		{"swap on", func(o *PreflightOptions) { o.Facts.SwapEnabled = true }, func(o *PreflightOptions) Result { return o.checkSwap() }, Warn},
		{"swap off", func(*PreflightOptions) {}, func(o *PreflightOptions) Result { return o.checkSwap() }, Pass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOptions()
			tc.mutate(o)
			if got := tc.check(o.withDefaults()); got.Status != tc.wantStatus {
				t.Fatalf("状态 = %s, want %s (%s)", got.Status, tc.wantStatus, got.Message)
			}
		})
	}
}

func TestCheckSELinux(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want Status
	}{
		{SELinuxEnforcing, Warn},
		{SELinuxPermissive, Pass},
		{SELinuxDisabled, Pass},
		{SELinuxUnknown, Pass},
	} {
		o := baseOptions()
		o.Facts.SELinuxMode = tc.mode
		if got := o.withDefaults().checkSELinux(); got.Status != tc.want {
			t.Fatalf("SELinux=%s checkSELinux() = %+v, want %s", tc.mode, got, tc.want)
		}
	}
}

func TestCheckFirewall(t *testing.T) {
	o := baseOptions()
	if got := o.withDefaults().checkFirewall(context.Background()); got.Status != Pass {
		t.Fatalf("未检测到防火墙应 Pass: %+v", got)
	}
	for _, backend := range []string{FirewallBackendFirewalld, FirewallBackendUFW, FirewallBackendIptables, FirewallBackendNFTables} {
		o = baseOptions()
		o.Facts.FirewallBackend = backend
		got := o.withDefaults().checkFirewall(context.Background())
		if got.Status != Warn || !strings.Contains(got.Message, backend) || !strings.Contains(got.Message, "6443/tcp") {
			t.Fatalf("backend=%s 应 Warn 并列出端口: %+v", backend, got)
		}
	}
}

func TestCheckCommands(t *testing.T) {
	o := baseOptions()
	o.CommandLookup = fakeCommandLookup{}
	if got := o.withDefaults().checkCommands(); got.Status != Pass {
		t.Fatalf("checkCommands() = %+v, want Pass", got)
	}

	o = baseOptions()
	o.CommandLookup = fakeCommandLookup{missing: map[string]bool{"systemctl": true, "modprobe": true}}
	got := o.withDefaults().checkCommands()
	if got.Status != Fail || !strings.Contains(got.Message, "systemctl") || !strings.Contains(got.Message, "modprobe") {
		t.Fatalf("checkCommands() = %+v", got)
	}
}

func TestDefaultPortsCoverAppendixA(t *testing.T) {
	got := map[string]bool{}
	for _, p := range DefaultPorts() {
		got[portKey(p.Protocol, p.Port)] = true
	}
	want := []string{
		"tcp/6443", "tcp/2379", "tcp/2380", "tcp/2381", "tcp/10250", "tcp/10248",
		"tcp/10257", "tcp/10259", "tcp/179", "tcp/20244", "ipip/0",
	}
	for _, key := range want {
		if !got[key] {
			t.Errorf("附录 A 端口 %s 缺失", key)
		}
	}
	if len(got) != len(want) {
		t.Errorf("端口数量 = %d, want %d", len(got), len(want))
	}
}

func TestSummarizeAndEvaluate(t *testing.T) {
	results := []Result{
		{Name: CheckPorts, Status: Pass},
		{Name: CheckKernel, Status: Warn, Message: "内核偏旧"},
		{Name: CheckCgroup, Status: Fail, Message: "缺少 cgroup v2"},
	}
	if failed, warned := Summarize(results); failed != 1 || warned != 1 {
		t.Fatalf("Summarize() = %d, %d", failed, warned)
	}

	checks := []Check{
		{Name: CheckPorts, Description: "端口", Failure: "释放端口"},
		{Name: CheckKernel, Description: "内核", Failure: "升级内核"},
		{Name: CheckCgroup, Description: "cgroup", Failure: "切换到 v2"},
	}
	err := Evaluate(checks, results)
	if err == nil {
		t.Fatalf("存在 Fail 时 Evaluate 必须报错")
	}
	for _, want := range []string{"缺少 cgroup v2", "切换到 v2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误 %q 缺少 %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "内核偏旧") {
		t.Fatalf("Warn 不应出现在错误里: %v", err)
	}
	// 缺少某个声明的检查项时必须报错。
	extra := append(append([]Check{}, checks...), Check{Name: CheckSwap, Description: "swap", Failure: "关闭 swap"})
	if err := Evaluate(extra, results); err == nil || !strings.Contains(err.Error(), "缺少预检结果") {
		t.Fatalf("缺少的检查项应当被报告: %v", err)
	}

	if err := Evaluate(checks, []Result{
		{Name: CheckPorts, Status: Pass},
		{Name: CheckKernel, Status: Pass},
		{Name: CheckCgroup, Status: Pass},
	}); err != nil {
		t.Fatalf("全部通过时 Evaluate() = %v", err)
	}
	if err := Evaluate(checks, []Result{
		{Name: CheckPorts, Status: Pass},
		{Name: CheckKernel, Status: Pass},
		{Name: CheckCgroup, Status: Pass},
		{Name: "surprise", Status: Pass},
	}); err == nil || !strings.Contains(err.Error(), "未声明") {
		t.Fatalf("未声明的结果应当报错，得到 %v", err)
	}
}

func TestRunPreflightFromCollectedFacts(t *testing.T) {
	fc := NewFakeCollector()
	fc.Files[etcOSRelease] = rockyOSRelease
	fc.Files[procSysKernelRelease] = "5.14.0-362.el9.x86_64\n"
	fc.Files[procMeminfo] = "MemTotal:       16316428 kB\nSwapTotal:       2097148 kB\n"
	fc.Files[sysCgroupRoot+"/cgroup.controllers"] = "cpu"
	fc.Files[sysSELinuxEnforce] = "1\n"
	fc.Commands["systemctl is-active firewalld"] = "active\n"

	facts, err := Collect(context.Background(), fc)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	o := baseOptions()
	o.Facts = facts

	results := RunPreflight(context.Background(), o)
	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if got := byName[CheckSwap]; got.Status != Warn {
		t.Fatalf("swap 检查 = %+v, want Warn", got)
	}
	if got := byName[CheckSELinux]; got.Status != Warn || !strings.Contains(got.Message, "enforcing") {
		t.Fatalf("SELinux 检查 = %+v, want Warn 且提到 enforcing", got)
	}
	if got := byName[CheckFirewall]; got.Status != Warn {
		t.Fatalf("检测到 firewalld 应 Warn: %+v", got)
	}
	if got := byName[CheckResources]; got.Status != Pass {
		t.Fatalf("资源检查应 Pass: %+v", got)
	}
	if failed, _ := Summarize(results); failed != 0 {
		t.Fatalf("不应有 Fail：%+v", results)
	}
}

func TestNewCollectorSmoke(t *testing.T) {
	c := NewCollector()
	if c == nil {
		t.Fatalf("NewCollector() 返回 nil")
	}
	if _, err := c.Glob("/kubemono-does-not-exist/*"); err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if _, err := c.ReadFile("/kubemono-does-not-exist"); err == nil {
		t.Fatalf("读取不存在的文件应当报错")
	}
	if _, _, err := (osCollector{}).StatFS("/"); err != nil && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("StatFS(/) error = %v", err)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes uint64
		want  string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1.0 kB"},
		{2 << 30, "2.1 GB"},
	}
	for _, tc := range tests {
		if got := FormatBytes(tc.bytes); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestPreflightDefaultsDoNotMutateInput(t *testing.T) {
	o := &PreflightOptions{Facts: &Facts{}}
	got := o.withDefaults()
	if o.PortChecker != nil || o.MinCPUs != 0 || len(o.Ports) != 0 {
		t.Fatalf("withDefaults 修改了调用方对象")
	}
	if got.PortChecker == nil || got.MinCPUs != DefaultMinCPUs || len(got.Ports) != len(DefaultPorts()) {
		t.Fatalf("withDefaults 未补齐默认值: %+v", got)
	}
	if !reflect.DeepEqual(got.DataDirs, DefaultDataDirs) {
		t.Fatalf("DataDirs = %v", got.DataDirs)
	}
}

// Evaluate 把结果与声明比对：缺少检查、未声明的结果、任何 Fail 都是错误。
func Evaluate(checks []Check, results []Result) error {
	declared := make(map[string]Check, len(checks))
	order := make([]string, 0, len(checks))
	for _, c := range checks {
		declared[c.Name] = c
		order = append(order, c.Name)
	}
	seen := make(map[string]bool, len(results))
	var failed []string
	var unknown []string
	for _, r := range results {
		c, ok := declared[r.Name]
		if !ok {
			unknown = append(unknown, r.Name)
			continue
		}
		seen[r.Name] = true
		if r.Status == Fail {
			message := r.Message
			if message == "" {
				message = c.Description + " 未通过"
			}
			remediation := r.Remediation
			if remediation == "" {
				remediation = c.Failure
			}
			failed = append(failed, fmt.Sprintf("%s：%s（修复：%s）", r.Name, message, remediation))
		}
	}
	var missing []string
	for _, name := range order {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	var parts []string
	if len(failed) > 0 {
		parts = append(parts, "预检失败："+strings.Join(failed, "；"))
	}
	if len(missing) > 0 {
		parts = append(parts, "缺少预检结果："+strings.Join(missing, "、"))
	}
	if len(unknown) > 0 {
		parts = append(parts, "出现未声明的预检结果："+strings.Join(unknown, "、"))
	}
	if len(parts) > 0 {
		return errors.New("hostinit: " + strings.Join(parts, "；"))
	}
	return nil
}
