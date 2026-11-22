package reset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/util/output"
)

// fakeHost 是内存文件系统/挂载点。
type fakeHost struct {
	exists      map[string]bool
	dirs        map[string][]string
	mounts      []string
	copies      [][2]string
	removed     []string
	moved       [][2]string
	unmounted   []string
	copyErr     error
	removeErrOn map[string]error
}

func newFakeHost() *fakeHost {
	return &fakeHost{exists: map[string]bool{}, dirs: map[string][]string{}, removeErrOn: map[string]error{}}
}

func (f *fakeHost) Exists(path string) bool { return f.exists[path] }

func (f *fakeHost) CopyTree(_ context.Context, src, dst string) error {
	if f.copyErr != nil {
		return f.copyErr
	}
	f.copies = append(f.copies, [2]string{src, dst})
	return nil
}

func (f *fakeHost) RemoveAll(_ context.Context, path string) error {
	if err := f.removeErrOn[path]; err != nil {
		return err
	}
	delete(f.exists, path)
	f.removed = append(f.removed, path)
	return nil
}

func (f *fakeHost) ListMounts(_ context.Context, _ string) ([]string, error) { return f.mounts, nil }

func (f *fakeHost) Unmount(_ context.Context, target string) error {
	f.unmounted = append(f.unmounted, target)
	return nil
}

func (f *fakeHost) hasRemoved(path string) bool {
	return slices.Contains(f.removed, path)
}

// fakeNetwork 记录网络清理清单。
type fakeNetwork struct {
	calls   int
	targets []NetworkTargets
	err     error
}

func (f *fakeNetwork) CleanNetwork(_ context.Context, targets NetworkTargets) error {
	f.calls++
	f.targets = append(f.targets, targets)
	return f.err
}

// fakeRuntime / fakeHostInit 记录"是否被调用"。

type fakeHostInit struct {
	calls   int
	paths   []constants.Paths
	revertE error
}

func (f *fakeHostInit) Revert(_ context.Context, paths constants.Paths) error {
	f.calls++
	f.paths = append(f.paths, paths)
	return f.revertE
}

// fakeRunner 记录 systemctl 调用。
type fakeRunner struct {
	calls  []string
	active map[string]bool
}

func newFakeRunner() *fakeRunner { return &fakeRunner{active: map[string]bool{}} }

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case name == "systemctl" && len(args) > 1 && args[0] == "is-active":
		if f.active[strings.TrimSuffix(args[1], ".service")] {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), nil
	case name == "ctr" && len(args) >= 4 && args[0] == "-n" && args[2] == "tasks" && args[3] == "list":
		return []byte("ctr-1\n"), nil
	}
	return nil, nil
}

func (f *fakeRunner) hasPrefix(prefix string) bool {
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

// harness 组装一次 reset 所需的全部 fake。
type harness struct {
	root    string
	paths   constants.Paths
	host    *fakeHost
	network *fakeNetwork
	hostin  *fakeHostInit
	runner  *fakeRunner
	opts    *Options
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	paths := constants.NewPaths(t.TempDir())
	h := &harness{
		root:    paths.Root,
		paths:   paths,
		host:    newFakeHost(),
		network: &fakeNetwork{},
		hostin:  &fakeHostInit{},
		runner:  newFakeRunner(),
	}
	h.runner.active["kubelet"] = true
	for _, unit := range ControlPlaneUnits {
		h.runner.active[strings.TrimSuffix(unit, ".service")] = true
	}

	h.host.exists[paths.PKI()] = true
	h.host.exists[paths.Kubernetes()] = true
	h.host.exists[paths.Kubemono()] = true
	h.host.exists[paths.NodeConfig()] = true
	h.host.exists[paths.ClusterConfig()] = true
	h.host.exists[paths.Desired()] = true
	h.host.exists[paths.Kubelet()] = true
	h.host.exists[paths.EtcdData()] = true
	h.host.exists[paths.ManifestPath("kube-apiserver")] = true
	h.host.dirs[paths.Manifests()] = []string{"kube-apiserver.yaml", "etcd.yaml"}
	h.host.mounts = []string{
		filepath.Join(paths.Kubelet(), "pods", "1", "volumes"),
		filepath.Join(paths.Kubelet(), "pods", "2", "volumes"),
	}

	h.opts = &Options{
		Paths:  paths,
		Now:    func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) },
		Report: output.Discard(),
		Deps: Deps{
			Runner:   h.runner,
			Host:     h.host,
			Network:  h.network,
			HostInit: h.hostin,
		},
	}
	return h
}

func TestRunExecutesFiveStepsInOrder(t *testing.T) {
	h := newHarness(t)
	res, err := Run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got []string
	for _, step := range res.Steps {
		got = append(got, string(step.Step))
		if !step.Done {
			t.Fatalf("步骤 %s 未完成：%s", step.Step, step.Error)
		}
	}
	want := []string{"control-plane", "kubelet", "network", "data", "runtime"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("步骤顺序不符：%v", got)
	}

	// 1. 控制面 unit 停止并禁用。
	for _, unit := range ControlPlaneUnits {
		if !h.runner.hasPrefix("systemctl stop " + unit) {
			t.Fatalf("未停止 %s：%v", unit, h.runner.calls)
		}
		if !h.runner.hasPrefix("systemctl disable " + unit) {
			t.Fatalf("未禁用 %s：%v", unit, h.runner.calls)
		}
	}
	// 3. kubelet 停止 + Pod 停止 + 卸载挂载点。
	if !h.runner.hasPrefix("systemctl stop kubelet.service") {
		t.Fatalf("未停止 kubelet：%v", h.runner.calls)
	}
	if !h.runner.hasPrefix("ctr -n k8s.io tasks kill -s SIGKILL ctr-1") {
		t.Fatalf("未用 ctr 停止容器：%v", h.runner.calls)
	}
	if len(h.host.unmounted) != 2 {
		t.Fatalf("应卸载 2 个挂载点：%v", h.host.unmounted)
	}
	// 4. 网络清理。
	if h.network.calls != 1 {
		t.Fatalf("网络清理调用次数 = %d", h.network.calls)
	}
	// 5. PKI 备份 + 删除。
	if len(h.host.copies) != 1 || h.host.copies[0][1] != filepath.Join(h.paths.Backup(), "20240501T120000Z", "pki") {
		t.Fatalf("PKI 备份目录不符：%v", h.host.copies)
	}
	if res.BackupDir == "" {
		t.Fatal("Result.BackupDir 未设置")
	}
	for _, path := range []string{h.paths.Kubernetes(), h.paths.NodeConfig(), h.paths.ClusterConfig(), h.paths.Desired(), h.paths.Kubelet(), h.paths.EtcdData()} {
		if !h.host.hasRemoved(path) {
			t.Fatalf("应删除 %s，实际删除 %v", path, h.host.removed)
		}
	}
	if h.hostin.calls != 1 {
		t.Fatal("默认应还原主机初始化")
	}
}

func TestDryRunDeletesNothing(t *testing.T) {
	h := newHarness(t)
	h.opts.DryRun = true

	res, err := Run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.hostin.calls != 0 {
		t.Fatal("dry-run 不应还原主机初始化")
	}
	if h.network.calls != 0 {
		t.Fatal("dry-run 不应执行网络清理")
	}
	if len(h.host.removed) != 0 || len(h.host.unmounted) != 0 || len(h.host.moved) != 0 {
		t.Fatalf("dry-run 不应改主机：removed=%v unmounted=%v moved=%v", h.host.removed, h.host.unmounted, h.host.moved)
	}
	if len(res.WouldDelete) == 0 {
		t.Fatal("dry-run 必须输出将删除的清单")
	}
	joined := strings.Join(res.WouldDelete, "\n")
	for _, want := range []string{h.paths.Kubernetes(), h.paths.NodeConfig(), h.paths.ClusterConfig(), h.paths.Desired(), h.paths.Kubelet(), h.paths.EtcdData()} {
		if !strings.Contains(joined, want) {
			t.Fatalf("将删除清单缺少 %s：\n%s", want, joined)
		}
	}
	if len(h.network.targets) != 0 {
		t.Fatal("dry-run 不应把清单交给清理器执行")
	}
}

func TestKeepDataAndKeepHostInitMatrix(t *testing.T) {
	cases := []struct {
		name         string
		keepData     bool
		keepHostInit bool
		wantDeleted  []string
		denyDeleted  []string
		wantHostInit int
	}{
		{
			name:         "默认：删数据、还原主机、不卸载运行时",
			wantDeleted:  []string{"kubelet", "etcd"},
			wantHostInit: 1,
		},
		{
			name:         "keep-data 保留数据目录",
			keepData:     true,
			denyDeleted:  []string{"kubelet", "etcd"},
			wantHostInit: 1,
		},
		{
			name:         "keep-hostinit 保留主机改动",
			keepHostInit: true,
			wantDeleted:  []string{"kubelet", "etcd"},
			wantHostInit: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.opts.KeepData = tc.keepData
			h.opts.KeepHostInit = tc.keepHostInit

			res, err := Run(context.Background(), h.opts)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, want := range tc.wantDeleted {
				var path string
				switch want {
				case "kubelet":
					path = h.paths.Kubelet()
				case "etcd":
					path = h.paths.EtcdData()
				}
				if !h.host.hasRemoved(path) {
					t.Fatalf("应删除 %s，实际 %v", path, h.host.removed)
				}
			}
			for _, deny := range tc.denyDeleted {
				var path string
				switch deny {
				case "kubelet":
					path = h.paths.Kubelet()
				case "etcd":
					path = h.paths.EtcdData()
				}
				if h.host.hasRemoved(path) {
					t.Fatalf("--keep-data 不应删除 %s", path)
				}
				if !strings.Contains(strings.Join(res.Skipped, "\n"), path) {
					t.Fatalf("Result.Skipped 应说明保留了 %s：%v", path, res.Skipped)
				}
			}
			if h.hostin.calls != tc.wantHostInit {
				t.Fatalf("主机初始化还原调用 = %d，期望 %d", h.hostin.calls, tc.wantHostInit)
			}
		})
	}
}

func TestPKIBackupFailureAbortsDeletion(t *testing.T) {
	h := newHarness(t)
	h.host.copyErr = errors.New("磁盘已满")

	_, err := Run(context.Background(), h.opts)
	if err == nil {
		t.Fatal("PKI 备份失败时 Run 必须返回错误")
	}
	if !strings.Contains(err.Error(), "中止删除") {
		t.Fatalf("错误应说明已中止删除：%v", err)
	}
	if len(h.host.removed) != 0 {
		t.Fatalf("备份失败后不得删除任何东西：%v", h.host.removed)
	}
	if h.hostin.calls != 0 {
		t.Fatal("备份失败后不应执行第 5 步")
	}
}

func TestNetworkFailureStillDeletesData(t *testing.T) {
	h := newHarness(t)
	h.network.err = errors.New("权限不足")

	res, err := Run(context.Background(), h.opts)
	if err == nil {
		t.Fatal("网络清理失败时必须返回非零结果")
	}
	if len(res.NetworkErrors) == 0 {
		t.Fatal("Result.NetworkErrors 应记录失败项")
	}
	if !h.host.hasRemoved(h.paths.Kubernetes()) {
		t.Fatal("网络清理失败不应阻塞数据删除")
	}
}

func TestNetworkTargetsContent(t *testing.T) {
	paths := constants.NewPaths("")
	targets := DefaultNetworkTargets(paths)
	var chains []string
	for _, item := range targets.IPTables {
		chains = append(chains, string(item.Family)+"/"+item.Table+"/"+item.Prefix)
	}
	for _, want := range []string{"ipv4/nat/KUBE-", "ipv4/filter/KUBE-", "ipv6/filter/KUBE-"} {
		if !strings.Contains(strings.Join(chains, ","), want) {
			t.Fatalf("iptables 清单缺少 %s：%v", want, chains)
		}
	}
	for _, want := range []string{"kube-ipvs0", "kube-bridge", "kube-dummy-if"} {
		if !strings.Contains(strings.Join(targets.Links, ","), want) {
			t.Fatalf("接口清单缺少 %s：%v", want, targets.Links)
		}
	}
	if strings.Join(targets.LinkPrefixes, ",") != "tun-" {
		t.Fatalf("接口前缀应为 tun-：%v", targets.LinkPrefixes)
	}
	if strings.Join(targets.IPSetPrefixes, ",") != "kube-router-" {
		t.Fatalf("ipset 前缀应为 kube-router-：%v", targets.IPSetPrefixes)
	}
	if !targets.IPVS || !targets.RoutesViaLinks {
		t.Fatal("应清理 ipvs 与接口相关路由")
	}
	var nft []string
	for _, item := range targets.NFTables {
		nft = append(nft, item.Family+"/"+item.Name)
	}
	for _, want := range []string{"ip/kube-proxy", "ip6/kube-proxy"} {
		if !strings.Contains(strings.Join(nft, ","), want) {
			t.Fatalf("nftables 清单缺少 %s：%v", want, nft)
		}
	}
	wantPaths := strings.Join([]string{constants.CNIConfigDir, constants.CNIDataDir}, ",")
	if strings.Join(targets.Paths, ",") != wantPaths {
		t.Fatalf("CNI 目录清单不符：%v", targets.Paths)
	}
}

func TestBinaryModeStopsAndDisablesControlPlane(t *testing.T) {
	h := newHarness(t)
	for _, unit := range ControlPlaneUnits {
		h.runner.active[strings.TrimSuffix(unit, ".service")] = true
	}

	if _, err := Run(context.Background(), h.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, unit := range ControlPlaneUnits {
		if !h.runner.hasPrefix("systemctl stop " + unit) {
			t.Fatalf("未停止 %s：%v", unit, h.runner.calls)
		}
		if !h.runner.hasPrefix("systemctl disable " + unit) {
			t.Fatalf("未禁用 %s：%v", unit, h.runner.calls)
		}
	}
	if len(h.host.moved) != 0 {
		t.Fatal("binary 模式不应移动静态 Pod 清单")
	}
}

func TestRemoveFailureAbortsBeforeFurtherDeletion(t *testing.T) {
	h := newHarness(t)
	h.host.removeErrOn[h.paths.Kubernetes()] = errors.New("设备忙")

	_, err := Run(context.Background(), h.opts)
	if err == nil {
		t.Fatal("删除失败必须返回错误")
	}
	if h.host.hasRemoved(h.paths.NodeConfig()) {
		t.Fatal("删除失败后不应继续删除后续路径")
	}
}

func TestHostInitRevertFailureIsReportedAsWarning(t *testing.T) {
	h := newHarness(t)
	h.hostin.revertE = errors.New("有 2 项不可自动还原")

	res, err := Run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("主机初始化还原不完整不应让 reset 失败：%v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("还原不完整必须出现在 Warnings 里")
	}
}

func TestSafePathGuards(t *testing.T) {
	for _, bad := range []string{"", "/", "relative/path"} {
		if err := safePath(bad); err == nil {
			t.Fatalf("safePath(%q) 应当报错", bad)
		}
	}
	if err := safePath("/var/lib/kubelet"); err != nil {
		t.Fatalf("绝对路径不应被拒绝：%v", err)
	}
}

func TestUnitAndBinaryPathsAreRooted(t *testing.T) {
	paths := constants.NewPaths("/tmp/root")
	if got := unitFilePath(paths, "kubelet.service"); got != "/tmp/root/etc/systemd/system/kubelet.service" {
		t.Fatalf("unit 路径 = %q", got)
	}
	if got := filepath.Join(paths.LocalBin(), "kubelet"); got != "/tmp/root/usr/local/bin/kubelet" {
		t.Fatalf("二进制路径 = %q", got)
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/var/lib/kubelet/pods/a\040b`); got != "/var/lib/kubelet/pods/a b" {
		t.Fatalf("转义还原失败：%q", got)
	}
	if got := unescapeMount("/var/lib/kubelet"); got != "/var/lib/kubelet" {
		t.Fatalf("无转义时不应改变：%q", got)
	}
}

func TestStepNamesMatchSpec(t *testing.T) {
	order := []Step{StepControlPlane, StepKubelet, StepNetwork, StepData, StepRuntime}
	names := []string{"control-plane", "kubelet", "network", "data", "runtime"}
	for i, step := range order {
		if string(step) != names[i] {
			t.Fatalf("步骤 %d 名称不符：%q != %q", i, step, names[i])
		}
	}
	if fmt.Sprint(StepData) != "data" {
		t.Fatal("StepData 的字符串形式应为 data")
	}
}

// TestDefaultNetworkTargetsCoverReinstallTraps 锁住重装时必踩的两个坑。
// - kubemono0：kube-router 的 CNI 网桥。不删它，重装到新集群时 bridge 插件会报
// `"kubemono0" already has an IP address different from …`，Pod 永远起不来。
// - /var/lib/cni：CNI 的 IPAM 分配记录。不清它，新集群会复用旧 IP 或直接冲突。
func TestDefaultNetworkTargetsCoverReinstallTraps(t *testing.T) {
	paths := constants.NewPaths("/")
	targets := DefaultNetworkTargets(paths)

	hasLink := false
	for _, l := range targets.Links {
		if l == "kubemono0" {
			hasLink = true
		}
	}
	if !hasLink {
		t.Fatalf("清理目标必须包含 kubemono0 网桥，实际：%v", targets.Links)
	}
	desc := strings.Join(targets.Describe(), "、")
	for _, want := range []string{paths.CNIConfig(), paths.CNIData()} {
		if !strings.Contains(desc, want) {
			t.Errorf("清理目标缺少 %s：%s", want, desc)
		}
	}
}

// 默认 reset 只停服务，不删除 /usr/local/bin 与 /opt/cni/bin：
// 删除已被其他组件引用的二进制会让 containerd/kubelet 进入 203/EXEC 循环。
func TestDefaultResetPreservesInstalledBinaries(t *testing.T) {
	h := newHarness(t)
	binaries := []string{
		h.paths.Resolve("/usr/local/bin/containerd"),
		h.paths.Resolve("/usr/local/bin/kubelet"),
		filepath.Join(h.paths.CNIBin(), "bridge"),
	}
	for _, path := range binaries {
		h.host.exists[path] = true
	}

	if _, err := Run(context.Background(), h.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, path := range binaries {
		if h.host.hasRemoved(path) {
			t.Fatalf("默认 reset 不应删除 %s", path)
		}
	}
	// containerd 监视 /etc/cni/net.d；默认保留运行时的情况下目录必须留着。
	if _, err := os.Stat(h.paths.CNIConfig()); err != nil {
		t.Fatalf("默认 reset 应保留 CNI 配置目录: %v", err)
	}

	targets := deletionList(h.opts)
	for _, path := range binaries {
		for _, target := range targets {
			if target == path {
				t.Fatalf("deletionList 不应包含已安装二进制 %s", path)
			}
		}
	}
}

// TestDataBacksUpEncryptionConfig 固定 P0-8：
// reset 必须同时备份 /etc/kubernetes/encryption；只备份 PKI 时，
// --keep-data 保留下来的 etcd 数据里所有 Secret 都无法再解密。
func TestDataBacksUpEncryptionConfig(t *testing.T) {
	h := newHarness(t)
	h.host.exists[h.paths.EncryptionConfig()] = true
	res, err := Run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := filepath.Join(h.paths.Backup(), "20240501T120000Z", "encryption")
	found := false
	for _, cp := range h.host.copies {
		if cp[1] == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("未备份 encryption 配置到 %s：%v", want, h.host.copies)
	}
	if res.BackupDir == "" {
		t.Fatal("Result.BackupDir 未设置")
	}
}

// TestPurgeDeletionListIncludesRuntimeArtifacts 固定 --purge 的范围：
// 运行二进制、CNI、containerd 配置/数据、/usr/local/kubemono 与运行时 unit 都必须在清单里。
func TestPurgeDeletionListIncludesRuntimeArtifacts(t *testing.T) {
	h := newHarness(t)
	h.opts.Purge = true
	h.opts.RuntimeUnit = "containerd.service"
	h.opts.ContainerdDataDir = "/var/lib/containerd"

	for _, p := range []string{
		filepath.Join(h.paths.LocalBin(), "kubelet"),
		filepath.Join(h.paths.CNIBin(), "bridge"),
		filepath.Join(h.paths.Resolve("/etc/containerd"), "config.toml"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	list := deletionList(h.opts)
	for _, want := range []string{
		h.paths.LocalBin() + "/kubelet",
		h.paths.CNIBin() + "/bridge",
		h.paths.Kubemono(),
		h.paths.Resolve("/etc/containerd"),
		h.paths.Resolve("/var/lib/containerd"),
		unitFilePath(h.paths, "containerd.service"),
	} {
		if !slices.Contains(list, want) {
			t.Fatalf("--purge 清单缺少 %s：%v", want, list)
		}
	}
}

// TestPurgeRunRemovesAllManagedTargets 验证实际执行：运行二进制/CNI/数据目录被删；
// PKI 备份路径记录在 <kubemono-root>/backup（成功 purge 时随根目录一起删除）。
func TestPurgeRunRemovesAllManagedTargets(t *testing.T) {
	h := newHarness(t)
	h.opts.Purge = true
	h.opts.RuntimeUnit = "containerd.service"
	h.opts.ContainerdDataDir = "/var/lib/containerd"
	for _, p := range []string{
		filepath.Join(h.paths.LocalBin(), "kubelet"),
		filepath.Join(h.paths.CNIBin(), "bridge"),
		filepath.Join(h.paths.Kubemono(), "artifacts", "v1", "meta.json"),
		h.paths.Resolve("/var/lib/containerd"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		h.host.exists[p] = true
	}
	h.host.exists[h.paths.Resolve("/var/lib/containerd")] = true
	res, err := Run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BackupBase == "" || !strings.Contains(res.BackupBase, "usr/local/kubemono/backup") {
		t.Fatalf("purge 备份目录 %q 应在 kubemono 根目录下", res.BackupBase)
	}
	for _, p := range []string{
		filepath.Join(h.paths.LocalBin(), "kubelet"),
		filepath.Join(h.paths.CNIBin(), "bridge"),
		h.paths.Kubemono(),
		h.paths.Resolve("/var/lib/containerd"),
	} {
		if !h.host.hasRemoved(p) {
			t.Errorf("purge 未删除 %s", p)
		}
	}
}
