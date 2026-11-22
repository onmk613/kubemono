package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
)

// TestContainerdStatusUsesCtr 用 ctr 确认 containerd API，并计算受管配置摘要。
func TestContainerdStatusUsesCtr(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	spec := managedSpecFor(t, paths, "version = 2\n", nil)
	cfgPath := render.ContainerdConfigPath(&render.Context{Paths: paths})
	writeTestFile(t, cfgPath, "version = 2\n", 0o644)
	// probe 会在 root 下检查 socket 文件。
	socketPath := paths.Resolve("/run/containerd/containerd.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socketPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	svc := newFakeServices(DefaultServiceName)
	cmd := &fakeRunner{}
	prov := newTestContainerdProvider(t, containerdOptions(root, svc, cmd))

	st, err := prov.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running {
		t.Error("Running 应为 true")
	}
	if st.CRISocket != DefaultCRISocket {
		t.Errorf("CRISocket = %q，期望 %q", st.CRISocket, DefaultCRISocket)
	}
	if st.ConfigHash != DesiredConfigHash(spec) {
		t.Errorf("ConfigHash = %s，期望 %s", st.ConfigHash, DesiredConfigHash(spec))
	}
	if !strings.Contains(st.Message, "运行中") {
		t.Errorf("Message = %q，应包含运行状态", st.Message)
	}
	last := cmd.lastCall()
	if len(last) != 4 || !strings.HasSuffix(last[0], "/ctr") || last[1] != "--address" || last[2] != socketPath || last[3] != "version" {
		t.Errorf("ctr 调用参数不正确: %v", last)
	}
}

// TestContainerdStatusCtrMissingFallsBack ctr 缺失时仍以 systemd + socket 判断。
func TestContainerdStatusCtrMissingFallsBack(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	socketPath := paths.Resolve("/run/containerd/containerd.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socketPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newFakeServices(DefaultServiceName)
	cmd := &fakeRunner{fn: func(name string, _ []string) (CommandResult, error) {
		return CommandResult{ExitCode: -1}, fmt.Errorf("%w: %s", ErrCommandNotFound, name)
	}}
	prov := newTestContainerdProvider(t, containerdOptions(root, svc, cmd))

	st, err := prov.Status(context.Background())
	if err != nil {
		t.Fatalf("ctr 缺失不应报错: %v", err)
	}
	if !st.Running {
		t.Error("Running 应为 true")
	}
	if !strings.Contains(st.Message, "未找到 ctr") {
		t.Errorf("Message = %q", st.Message)
	}
}

// TestContainerdStatusServiceQueryFails systemd 查询失败时冒泡。
func TestContainerdStatusServiceQueryFails(t *testing.T) {
	root := t.TempDir()
	svc := newFakeServices()
	svc.fail["is-active:"+DefaultServiceName] = errors.New("systemd 不可用")
	prov := newTestContainerdProvider(t, containerdOptions(root, svc, &fakeRunner{}))
	if _, err := prov.Status(context.Background()); err == nil {
		t.Fatal("unit 状态查询失败应返回错误")
	}
}

// TestContainerdRestart 重启走服务管理器。
func TestContainerdRestart(t *testing.T) {
	root := t.TempDir()
	svc := newFakeServices(DefaultServiceName)
	prov := newTestContainerdProvider(t, containerdOptions(root, svc, &fakeRunner{}))
	if err := prov.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := svc.recorded(); len(got) != 1 || got[0] != "restart:"+DefaultServiceName {
		t.Errorf("服务动作 = %v，期望 [restart:containerd]", got)
	}
}

// TestCustomProviderNeverWritesConfig custom 运行时绝不写任何配置（含目录）。
func TestCustomProviderNeverWritesConfig(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	spec := managedSpecFor(t, paths, "version = 2\n", map[string]string{"reg.example.com": "server = \"x\"\n"})

	opts := Options{
		Paths:    paths,
		Report:   nil,
		Commands: &fakeRunner{},
		Services: newFakeServices("crio"),
	}
	prov := newTestCustomProvider(t, v1CustomSpec(), opts)

	plan, err := prov.ApplyConfig(context.Background(), spec)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if plan.Decision != DecisionNone || len(plan.Changes) != 0 {
		t.Errorf("custom 的决策必须是 none/空计划，实际 %s/%d", plan.Decision, len(plan.Changes))
	}
	if files := listFiles(t, root); len(files) != 0 {
		t.Errorf("custom 运行时不应写任何文件，实际写了 %v", files)
	}
	if _, err := os.Stat(render.ContainerdConfigPath(&render.Context{Paths: paths})); !os.IsNotExist(err) {
		t.Errorf("containerd 配置不应存在（err=%v）", err)
	}
}

// TestCustomStatusUsesConfiguredSocketAndService 只检查配置的 socket 与服务名。
func TestCustomStatusUsesConfiguredSocketAndService(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	// 即便磁盘上存在 containerd 配置，custom 的 Status 也绝不读取它。
	writeTestFile(t, render.ContainerdConfigPath(&render.Context{Paths: paths}), "version = 2\n", 0o644)

	socketPath := paths.Resolve("/run/crio/crio.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socketPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newFakeServices("crio")
	cmd := &fakeRunner{}
	opts := Options{Paths: paths, Commands: cmd, Services: svc}
	prov := newTestCustomProvider(t, v1CustomSpec(), opts)

	st, err := prov.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running {
		t.Error("Running 应为 true")
	}
	if st.CRISocket != "unix:///run/crio/crio.sock" {
		t.Errorf("CRISocket = %q", st.CRISocket)
	}
	if st.ConfigHash != "" {
		t.Errorf("custom 的 ConfigHash 必须为空，实际 %q", st.ConfigHash)
	}
	if cmd.count() != 0 {
		t.Errorf("custom Status 不应调用 ctr: %v", cmd.calls)
	}
	if got := svc.recorded(); len(got) != 0 {
		t.Errorf("Status 不应产生服务动作: %v", got)
	}
}

// TestCustomRestart 重启使用配置的 serviceName。
func TestCustomRestart(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	svc := newFakeServices("crio")
	prov := newTestCustomProvider(t, v1CustomSpec(), Options{Paths: paths, Services: svc})

	if err := prov.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := svc.recorded(); len(got) != 1 || got[0] != "restart:crio" {
		t.Errorf("服务动作 = %v，期望 [restart:crio]", got)
	}
}

// TestDryRunWritesNothingAtProviderLevel 受管 provider 的 dry-run 不落任何文件。
func TestDryRunWritesNothingAtProviderLevel(t *testing.T) {
	root := t.TempDir()
	stageRuntimeArtifacts(t, root)
	paths := constants.NewPaths(root)
	spec := managedSpecFor(t, paths, "version = 2\n", nil)

	before := listFiles(t, root)
	opts := containerdOptions(root, newFakeServices(DefaultServiceName), &fakeRunner{})
	opts.DryRun = true
	prov := newTestContainerdProvider(t, opts)

	if err := prov.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := prov.ApplyConfig(context.Background(), spec); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	after := listFiles(t, root)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("dry-run 不应写任何文件\n之前: %v\n之后: %v", before, after)
	}
}

// v1CustomSpec 返回一份完整的自定义运行时配置。
func v1CustomSpec() clusterconfig.CustomRuntimeSpec {
	return clusterconfig.CustomRuntimeSpec{
		CRISocket:    "unix:///run/crio/crio.sock",
		CgroupDriver: "systemd",
		ServiceName:  "crio",
	}
}
