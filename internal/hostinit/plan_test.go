package hostinit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	clusterconfig "kubemono/internal/config"
)

// payloadOf 取出步骤的 typed payload。
func payloadOf[T any](st Step) (T, error) {
	if p, ok := st.Payload.(T); ok {
		return p, nil
	}
	var zero T
	return zero, fmt.Errorf("步骤 %s 的 Payload 类型是 %T，期望 %T", st.ID, st.Payload, zero)
}

// payloadT 取出步骤的 typed payload；类型不符直接让测试失败。
func payloadT[T any](t *testing.T, st Step) T {
	t.Helper()
	p, err := payloadOf[T](st)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return p
}

func testConfig() *clusterconfig.Cluster {
	c := clusterconfig.DefaultCluster()
	c.Node.Host.KernelModules = []string{"overlay", "br_netfilter", "zfs", "overlay"}
	c.Node.Host.Sysctls = map[string]string{
		"net.ipv4.ip_forward": "1",
		"vm.max_map_count":    "262144",
	}
	c.Cluster.Hooks.PreHostInit = []string{"echo hello", "  "}
	return c
}

func testHost(c *clusterconfig.Cluster) *clusterconfig.Host {
	return &c.Cluster.Hosts[0]
}

func findStep(t *testing.T, steps []Step, id string) Step {
	t.Helper()
	for _, st := range steps {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("计划里缺少步骤 %q", id)
	return Step{}
}

func stepIDs(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, st := range steps {
		out = append(out, st.ID)
	}
	return out
}

func TestPlanIsDeterministic(t *testing.T) {
	cfg := testConfig()
	first, err := Plan(cfg, testHost(cfg))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	second, err := Plan(cfg, testHost(cfg))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("同一输入的计划不一致:\n%+v\n%+v", first, second)
	}
	for _, st := range first {
		if st.ID == "" || st.Title == "" || st.Kind == "" || st.Payload == nil {
			t.Fatalf("步骤字段不完整: %+v", st)
		}
	}
}

func TestPlanOrder(t *testing.T) {
	cfg := testConfig()
	steps, err := Plan(cfg, testHost(cfg))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	index := map[string]int{}
	for i, st := range steps {
		index[st.ID] = i
	}
	if index["module-overlay"] >= index["modules-load"] {
		t.Fatalf("模块加载必须早于 modules-load: %v", stepIDs(steps))
	}
	if index["modules-load"] >= index["sysctl-net.ipv4.ip_forward"] {
		t.Fatalf("模块必须早于 sysctl: %v", stepIDs(steps))
	}
	if index["hook-preHostInit-0"] >= index["module-overlay"] {
		t.Fatalf("preHostInit hook 必须早于内核模块: %v", stepIDs(steps))
	}
	if index["hook-preHostInit-0"] >= index["sysctl-net.ipv4.ip_forward"] {
		t.Fatalf("preHostInit hook 必须早于 sysctl: %v", stepIDs(steps))
	}
	if index["hook-preHostInit-0"] >= index["dir-/etc/kubernetes"] {
		t.Fatalf("preHostInit hook 必须早于目录创建: %v", stepIDs(steps))
	}
}

func TestPlanContents(t *testing.T) {
	cfg := testConfig()
	steps, err := Plan(cfg, testHost(cfg))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	modulesFile := payloadT[FilePayload](t, findStep(t, steps, "modules-load"))
	if modulesFile.Path != modulesLoadPath || modulesFile.Content != "overlay\nbr_netfilter\nzfs\n" {
		t.Fatalf("modules-load = %+v", modulesFile)
	}
	sysctlFile := payloadT[FilePayload](t, findStep(t, steps, "sysctl-conf"))
	if sysctlFile.Path != sysctlConfPath {
		t.Fatalf("sysctl-conf path = %q", sysctlFile.Path)
	}
	if !strings.Contains(sysctlFile.Content, "net.ipv4.ip_forward = 1\n") ||
		!strings.Contains(sysctlFile.Content, "vm.max_map_count = 262144\n") {
		t.Fatalf("sysctl-conf content = %q", sysctlFile.Content)
	}
	hook := payloadT[HookPayload](t, findStep(t, steps, "hook-preHostInit-0"))
	if hook.Command != "echo hello" {
		t.Fatalf("hook command = %q", hook.Command)
	}
	for _, st := range steps {
		if st.ID == "hook-preHostInit-1" {
			t.Fatal("空白 hook 不应产生步骤")
		}
	}
	if !strings.Contains(stepIDs(steps)[0], "hook-preHostInit") {
		t.Fatalf("第一步应是 preHostInit hook: %v", stepIDs(steps))
	}
}

func TestPlanEmptyModulesAndSysctls(t *testing.T) {
	cfg := testConfig()
	cfg.Node.Host.KernelModules = []string{}
	cfg.Node.Host.Sysctls = map[string]string{}
	steps, err := Plan(cfg, testHost(cfg))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := payloadT[FilePayload](t, findStep(t, steps, "modules-load")).Content; got != "" {
		t.Fatalf("空模块清单应写空文件，实际 %q", got)
	}
	if got := payloadT[FilePayload](t, findStep(t, steps, "sysctl-conf")).Content; got != "" {
		t.Fatalf("空 sysctl 应写空文件，实际 %q", got)
	}
	for _, st := range steps {
		if st.Kind == KindModule || st.Kind == KindSysctl {
			t.Fatalf("空清单不应产生执行步骤: %+v", st)
		}
	}
}

func TestPlanArgumentValidation(t *testing.T) {
	if _, err := Plan(nil, &clusterconfig.Host{}); err == nil {
		t.Fatal("nil 配置应报错")
	}
	if _, err := Plan(testConfig(), nil); err == nil {
		t.Fatal("nil 主机应报错")
	}
}

func TestNormalizeModules(t *testing.T) {
	got := normalizeModules([]string{" overlay ", "", "br_netfilter", "overlay", "zfs"})
	want := []string{"overlay", "br_netfilter", "zfs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeModules = %v, want %v", got, want)
	}
}

func TestApplyFullPlan(t *testing.T) {
	cfg := testConfig()
	steps, err := Plan(cfg, testHost(cfg))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	ap := NewFakeApplier()
	j := NewMemory()
	if err := ApplyWithOptions(context.Background(), steps, ap, j, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	joined := strings.Join(ap.ops, "\n")
	for _, want := range []string{
		"module:overlay",
		"write:" + modulesLoadPath,
		"sysctl:net.ipv4.ip_forward=1",
		"write:" + sysctlConfPath,
		"mkdir:/etc/kubernetes",
		"cmd:sh -c echo hello",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("操作序列缺少 %q:\n%s", want, joined)
		}
	}
	if len(j.Entries) == 0 {
		t.Fatal("journal 应有记录")
	}
}

func TestApplyFileStepRestoresBefore(t *testing.T) {
	const path = "/tmp/kubemono-hostinit-test"
	ap := NewFakeApplier()
	ap.files[path] = "old"
	j := NewMemory()
	steps := []Step{{
		ID: "file-1", Kind: KindFile,
		Payload: FilePayload{Path: path, Mode: 0o644, Content: "new"},
	}}
	if err := ApplyWithOptions(context.Background(), steps, ap, j, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ap.files[path] != "new" {
		t.Fatalf("文件内容 = %q", ap.files[path])
	}
	if err := Reset(context.Background(), j, ap); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if ap.files[path] != "old" {
		t.Fatalf("还原后文件内容 = %q", ap.files[path])
	}
}

func TestApplySysctlAndModule(t *testing.T) {
	ap := NewFakeApplier()
	ap.sysctlValues["net.ipv4.ip_forward"] = "0"
	j := NewMemory()
	steps := []Step{
		{ID: "module-1", Kind: KindModule, Payload: ModulePayload{Name: "overlay"}},
		{ID: "sysctl-1", Kind: KindSysctl, Payload: SysctlPayload{Key: "net.ipv4.ip_forward", Value: "1"}},
	}
	if err := ApplyWithOptions(context.Background(), steps, ap, j, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// fake applier 的 Sysctl 只记录，不写值；用 journal 的 Before 验证。
	if len(j.Entries) != 2 || j.Entries[1].Before != "0" {
		t.Fatalf("journal = %+v", j.Entries)
	}
}

func TestApplyDirAndHook(t *testing.T) {
	ap := NewFakeApplier()
	j := NewMemory()
	steps := []Step{
		{ID: "dir-1", Kind: KindDir, Payload: DirPayload{Path: "/tmp/kubemono-dir", Mode: 0o755}},
		{ID: "hook-1", Kind: KindHook, Payload: HookPayload{Command: "true"}},
	}
	if err := ApplyWithOptions(context.Background(), steps, ap, j, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(ap.ops) != 2 || !strings.HasPrefix(ap.ops[0], "mkdir:") || ap.ops[1] != "cmd:sh -c true" {
		t.Fatalf("ops = %v", ap.ops)
	}
}

func TestApplyErrors(t *testing.T) {
	ap := NewFakeApplier()
	ap.failOn["module:"] = errors.New("boom")
	j := NewMemory()
	err := ApplyWithOptions(context.Background(), []Step{{
		ID: "module-1", Kind: KindModule, Payload: ModulePayload{Name: "overlay"},
	}}, ap, j, ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "module-1") {
		t.Fatalf("错误应包含步骤 ID: %v", err)
	}
}

func TestVerifyDetectsManagedFileDrift(t *testing.T) {
	const path = "/tmp/kubemono-hostinit-verify"
	ap := NewFakeApplier()
	step := Step{ID: "file-1", Kind: KindFile, Payload: FilePayload{Path: path, Mode: 0o644, Content: "want"}}
	steps := []Step{step}
	if ok, err := Verify(context.Background(), steps, ap, true); err != nil || ok {
		t.Fatalf("文件不存在时 Verify = %v, %v", ok, err)
	}
	ap.files[path] = "other"
	if ok, err := Verify(context.Background(), steps, ap, true); err != nil || ok {
		t.Fatalf("内容不一致时 Verify = %v, %v", ok, err)
	}
	ap.files[path] = "want"
	if ok, err := Verify(context.Background(), steps, ap, true); err != nil || !ok {
		t.Fatalf("内容一致时 Verify = %v, %v", ok, err)
	}
}

func TestApplyOptionalCapabilityMissing(t *testing.T) {
	ap := NewCoreApplier()
	err := ApplyWithOptions(context.Background(), []Step{{
		ID: "dir-1", Kind: KindDir, Payload: DirPayload{Path: "/tmp/x", Mode: fs.FileMode(0o755)},
	}}, ap, NewMemory(), ApplyOptions{})
	if err == nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("缺少 DirMaker 应返回 ErrUnsupported: %v", err)
	}
}
