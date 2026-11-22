package hostinit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/hostinit"
	"kubemono/internal/phase"
	"kubemono/internal/util/output"
)

// fakeApplier 是内存中的主机。
type fakeApplier struct {
	files    map[string][]byte
	sysctls  map[string]string
	modules  []string
	commands []string
	failOn   string
}

func newFakeApplier() *fakeApplier {
	return &fakeApplier{files: map[string][]byte{}, sysctls: map[string]string{}}
}

func (f *fakeApplier) WriteFile(_ context.Context, path string, content []byte, _ os.FileMode) error {
	if f.failOn == "write" {
		return errors.New("read-only filesystem")
	}
	f.files[path] = append([]byte(nil), content...)
	return nil
}

func (f *fakeApplier) RemoveFile(_ context.Context, path string) error {
	delete(f.files, path)
	return nil
}

func (f *fakeApplier) Sysctl(_ context.Context, k, v string) error { f.sysctls[k] = v; return nil }
func (f *fakeApplier) LoadModule(_ context.Context, name string) error {
	f.modules = append(f.modules, name)
	return nil
}
func (f *fakeApplier) RunCommand(_ context.Context, name string, args ...string) error {
	f.commands = append(f.commands, name+" "+strings.Join(args, " "))
	return nil
}
func (f *fakeApplier) ReadFile(_ context.Context, path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}
func (f *fakeApplier) MakeDir(context.Context, string, os.FileMode) error { return nil }
func (f *fakeApplier) ReadSysctl(_ context.Context, k string) (string, error) {
	if v, ok := f.sysctls[k]; ok {
		return v, nil
	}
	return "", os.ErrNotExist
}

func testContext(t *testing.T) *phase.Context {
	t.Helper()
	cluster := &clusterconfig.Cluster{}
	cluster.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	cluster.Node.Host.KernelModules = []string{"overlay", "br_netfilter"}
	cluster.Node.Host.Sysctls = map[string]string{"net.ipv4.ip_forward": "1"}
	clusterconfig.SetDefaults(cluster)
	host, _ := cluster.Cluster.Hosts.FindHost("m1")
	return &phase.Context{
		Cluster: cluster, Host: host, BOM: bom.MustLoad(),
		Paths: constants.NewPaths(t.TempDir()), Release: bom.MustLoad().Release,
		Report: output.Discard(), Now: time.Now,
	}
}

// TestCorruptJournalIsPreserved 守住 reset 的可还原性：hostinit.json 损坏时
// 不能被"当成没有记录"再整份覆盖，否则 /etc/hosts、sysctl、时区等 Before 值
// 永久丢失，`kubemono reset` 之后再也回不到原状。
func TestCorruptJournalIsPreserved(t *testing.T) {
	pc := testContext(t)
	path := pc.Paths.HostInitRecord()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const corrupt = "{ this is not a valid journal"
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	got := (&Phase{}).journal(pc)
	if got == nil {
		t.Fatal("journal 不应返回 nil")
	}
	if len(got.Entries) != 0 {
		t.Fatalf("损坏记录应重建成空 journal，实际 %d 条", len(got.Entries))
	}

	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(path)+".corrupt-") {
			found = filepath.Join(dir, e.Name())
		}
	}
	if found == "" {
		t.Fatal("损坏的 hostinit.json 必须改名备份，不能被直接覆盖")
	}
	data, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != corrupt {
		t.Fatalf("备份内容被改写：%q", data)
	}
}

func TestAppliesEverywhere(t *testing.T) {
	pc := testContext(t)
	if !(&Phase{}).Applies(pc) {
		t.Fatal("每个节点都需要 host-init")
	}
}

func TestRunAppliesPlanAndWritesJournal(t *testing.T) {
	pc := testContext(t)
	ap := newFakeApplier()
	p := &Phase{Applier: ap}
	ctx := context.Background()

	done, err := p.Check(ctx, pc)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("空 journal 时不应报告完成")
	}
	if err := p.Run(ctx, pc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// journal 落盘。
	journalPath := pc.Paths.HostInitRecord()
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("hostinit.json 未写入: %v", err)
	}
	loaded, err := hostinit.Load(journalPath)
	if err != nil || loaded == nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) == 0 {
		t.Fatal("journal 为空")
	}
	// 关键动作确实被执行。
	if len(ap.modules) == 0 {
		t.Error("未加载任何内核模块")
	}
	if _, ok := ap.sysctls["net.ipv4.ip_forward"]; !ok {
		t.Errorf("未设置 ip_forward（已设置：%v）", ap.sysctls)
	}
	if _, ok := ap.files[pc.Paths.Resolve("/etc/modules-load.d/kubemono.conf")]; !ok {
		t.Error("未写入 modules-load.d")
	}
	if _, ok := ap.files[pc.Paths.Resolve("/etc/sysctl.d/99-kubemono.conf")]; !ok {
		t.Error("未写入 sysctl.d")
	}
	// 再次 Check 应通过（journal 覆盖全部步骤且 swap 已关闭）。
	done, err = p.Check(ctx, pc)
	if err != nil || !done {
		t.Fatalf("Run 后 Check(done=%v err=%v)", done, err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	pc := testContext(t)
	pc.DryRun = true
	ap := newFakeApplier()
	p := &Phase{Applier: ap}
	if err := p.Run(context.Background(), pc); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(ap.files) != 0 || len(ap.modules) != 0 {
		t.Fatal("dry-run 不得修改主机")
	}
	if _, err := os.Stat(pc.Paths.HostInitRecord()); !os.IsNotExist(err) {
		t.Fatal("dry-run 不得写 journal")
	}
}

func TestApplyFailureIsReported(t *testing.T) {
	pc := testContext(t)
	ap := newFakeApplier()
	ap.failOn = "write"
	p := &Phase{Applier: ap}
	err := p.Run(context.Background(), pc)
	if err == nil {
		t.Fatal("写入失败必须报错")
	}
	if !strings.Contains(err.Error(), "主机变更") && !strings.Contains(err.Error(), "应用步骤") {
		t.Fatalf("错误信息应指出失败的步骤: %v", err)
	}
}

func TestStepsInjectionAndSummary(t *testing.T) {
	pc := testContext(t)
	steps := []hostinit.Step{
		{ID: "sysctl-ip-forward", Title: "开启转发", Kind: hostinit.KindSysctl},
		{ID: "module-overlay", Title: "加载 overlay", Kind: hostinit.KindModule},
	}
	p := &Phase{Steps: steps, Applier: newFakeApplier()}
	got, err := p.steps(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("注入的步骤未生效: %+v", got)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	pc := testContext(t)
	p := &Phase{}
	first, err := p.steps(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.steps(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("两次 Plan 的步骤数不同: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("Plan 顺序不稳定: %s vs %s", first[i].ID, second[i].ID)
		}
	}
}

func TestApplyRealApplierWritesFiles(t *testing.T) {
	// 真实 Applier 的文件部分在 darwin 上也可用：确认 WriteFile/ReadFile 往返。
	root := t.TempDir()
	a := NewApplier()
	path := filepath.Join(root, "nested", "file.conf")
	if err := a.WriteFile(context.Background(), path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := a.ReadFile(context.Background(), path)
	if err != nil || string(data) != "hello" {
		t.Fatalf("ReadFile = %q, err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("权限不正确: %v %v", info.Mode(), err)
	}
	if err := a.RemoveFile(context.Background(), path); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("文件未删除")
	}
}

// TestEveryPathPayloadIsRelocatedUnderRoot 保证测试根演练绝不碰真实主机。
func TestEveryPathPayloadIsRelocatedUnderRoot(t *testing.T) {
	pc := testContext(t)
	p := &Phase{}
	steps, err := p.steps(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("计划为空")
	}
	for _, st := range steps {
		for _, path := range stepPaths(st) {
			if path == "" {
				continue
			}
			if !strings.HasPrefix(path, pc.Paths.Root) {
				t.Errorf("步骤 %s 的路径 %q 未重定位到 %s", st.ID, path, pc.Paths.Root)
			}
		}
	}
	// 生产环境（无 root）必须保持真实路径。
	prod := testContext(t)
	prod.Paths = constants.Paths{}
	steps, err = p.steps(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range steps {
		if p, ok := st.Payload.(hostinit.FilePayload); ok && p.Path == "/etc/modules-load.d/kubemono.conf" {
			found = true
		}
	}
	if !found {
		t.Error("生产环境应使用真实 /etc/hosts 路径")
	}
}

// stepPaths 返回步骤中所有需要重定位的绝对路径字段。
func stepPaths(st hostinit.Step) []string {
	switch p := st.Payload.(type) {
	case hostinit.FilePayload:
		return []string{p.Path}
	case hostinit.DirPayload:
		return []string{p.Path}
	default:
		return nil
	}
}

func TestRunRefusesTestRootWithRealApplier(t *testing.T) {
	pc := testContext(t)
	err := (&Phase{}).Run(context.Background(), pc)
	if err == nil || !strings.Contains(err.Error(), "测试根") {
		t.Fatalf("真实 applier + 测试根必须拒绝执行: %v", err)
	}
}

// TestVerifyDetectsManagedFileDrift 保护受管文件的内容漂移：
// Verify 必须复用 Apply 的读/算逻辑，不能因为只看 FilePayload.Content
// 就漏掉漂移。
func TestVerifyDetectsManagedFileDrift(t *testing.T) {
	pc := testContext(t)
	ap := newFakeApplier()
	p := &Phase{Applier: ap}
	steps, err := p.steps(context.Background(), pc)
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	if err := hostinit.ApplyWithOptions(context.Background(), steps, ap, &hostinit.Journal{}, hostinit.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 内存 fake 没有真实目录，目录步骤由生产路径的 os.Stat 校验；这里只验证
	// 文件/受管区块/sysctl 等可比较项。
	var comparable []hostinit.Step
	for _, st := range steps {
		if st.Kind != hostinit.KindDir {
			comparable = append(comparable, st)
		}
	}
	ok, err := hostinit.Verify(context.Background(), comparable, ap, true)
	if err != nil || !ok {
		t.Fatalf("Apply 后 Verify(ok=%v err=%v)", ok, err)
	}
	modulesPath := pc.Paths.Resolve("/etc/modules-load.d/kubemono.conf")
	if _, ok := ap.files[modulesPath]; !ok {
		t.Fatal("计划里没有 modules-load.d 步骤")
	}
	ap.files[modulesPath] = []byte("# operator removed the managed modules\n")
	ok, err = hostinit.Verify(context.Background(), comparable, ap, true)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Fatal("modules-load.d 漂移后 Verify 必须为 false")
	}
}

// TestRelocateKubemonoDirsToCustomDataDir 固定 --data-dir 下 host-init 不会去创建默认根目录。
func TestRelocateKubemonoDirsToCustomDataDir(t *testing.T) {
	c := &phase.Context{Paths: constants.NewPaths("").WithKubemonoDir("/srv/kubemono")}
	steps := []hostinit.Step{{
		ID: "dir-artifacts", Kind: hostinit.KindDir,
		Payload: hostinit.DirPayload{Path: constants.ArtifactsDir, Mode: 0o755},
	}}
	got := relocate(c, steps)
	p, ok := got[0].Payload.(hostinit.DirPayload)
	if !ok {
		t.Fatalf("payload 类型 = %T", got[0].Payload)
	}
	if p.Path != "/srv/kubemono/artifacts" {
		t.Fatalf("自定义 data-dir 下目录 = %q", p.Path)
	}
}

type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRunCommandUsesHookTimeout 固定 hook 的独立超时，避免被普通主机命令的
// 60s 超时截断。
func TestRunCommandUsesHookTimeout(t *testing.T) {
	a := NewApplier()
	a.Runner = blockingRunner{}
	a.HookTimeout = 20 * time.Millisecond

	start := time.Now()
	err := a.RunCommand(context.Background(), "sh", "-c", "sleep 10")
	if err == nil {
		t.Fatal("超时后 RunCommand 必须返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("RunCommand 没有使用 HookTimeout，耗时 %s", elapsed)
	}
}
