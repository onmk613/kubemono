package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
	"kubemono/internal/util/output"
)

// testRelease 是测试用的 kubemono 版本。
const testRelease = "v1.31.4-km.1"

// fakeRunner 是可注入的 CommandRunner：记录调用并按需返回结果。
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	fn    func(name string, args []string) (CommandResult, error)
}

// Run 实现 CommandRunner。
func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (CommandResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(name, args)
	}
	return CommandResult{}, nil
}

// count 返回命令被调用的次数。
func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// lastCall 返回最近一次调用。
func (f *fakeRunner) lastCall() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

// fakeServices 是可注入的 ServiceManager。
type fakeServices struct {
	mu      sync.Mutex
	active  map[string]bool
	actions []string
	fail    map[string]error
}

func newFakeServices(active ...string) *fakeServices {
	s := &fakeServices{active: map[string]bool{}, fail: map[string]error{}}
	for _, unit := range active {
		s.active[unit] = true
	}
	return s
}

// Action 实现 ServiceManager。
func (s *fakeServices) Action(_ context.Context, unit, action string) error {
	s.mu.Lock()
	s.actions = append(s.actions, action+":"+unit)
	err := s.fail[action+":"+unit]
	s.mu.Unlock()
	return err
}

// IsActive 实现 ServiceManager。
func (s *fakeServices) IsActive(_ context.Context, unit string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.fail["is-active:"+unit]; ok {
		return false, err
	}
	return s.active[unit], nil
}

// DaemonReload 实现 ServiceManager。
func (s *fakeServices) DaemonReload(context.Context) error {
	s.mu.Lock()
	s.actions = append(s.actions, "daemon-reload")
	s.mu.Unlock()
	return nil
}

// countActions 返回 action 被执行的次数。
func (s *fakeServices) countActions(action string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.actions {
		if strings.HasPrefix(a, action+":") {
			n++
		}
	}
	return n
}

// recorded 按顺序返回全部 action。
func (s *fakeServices) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.actions...)
}

// writeTestFile 写入一个文件并创建父目录。
func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录 %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}

// artifactDir 返回 release 的物料目录。
func artifactDir(root string) string {
	return constants.NewPaths(root).ArtifactDir(testRelease)
}

// stageRuntimeArtifacts 造出一份完整的运行时物料：运行时二进制与 CNI 插件二进制。
func stageRuntimeArtifacts(t *testing.T, root string) {
	t.Helper()
	stageRuntimeBinaries(t, root)
	dir := artifactDir(root)
	// 按 BOM 声明的插件逐个落位：默认已是全量，写死名单会随 BOM 变化而失配。
	for _, name := range CNIPlugins(bom.MustLoad()) {
		writeTestFile(t, filepath.Join(dir, "bin", "cni", name), name+"-plugin", 0o755)
	}
}

// stageRuntimeBinaries 只放运行时二进制，用于验证 CNI 包缺失的分支。
// 布局采用 的规范形式 bin/<category>/<name>（与 internal/kubelet 一致）。
func stageRuntimeBinaries(t *testing.T, root string) {
	t.Helper()
	dir := artifactDir(root)
	bins := map[string]string{
		"bin/runtime/containerd":              "containerd-binary",
		"bin/runtime/containerd-shim-runc-v2": "shim-binary",
		"bin/runtime/ctr":                     "ctr-binary",
		"bin/runtime/runc":                    "runc-binary",
	}
	for rel, content := range bins {
		writeTestFile(t, filepath.Join(dir, filepath.FromSlash(rel)), content, 0o755)
	}
}

// containerdSpec 返回默认（kubemono 托管）运行时配置。
func containerdSpec() clusterconfig.RuntimeSpec {
	return clusterconfig.RuntimeSpec{}
}

// containerdOptions 返回一套指向临时目录的 Options。
func containerdOptions(root string, svc *fakeServices, cmd *fakeRunner) Options {
	return Options{
		Paths:      constants.NewPaths(root),
		BOM:        bom.MustLoad(),
		Release:    testRelease,
		InstallDir: filepath.Join(root, "usr", "local", "bin"),
		Report:     output.Discard(),
		Services:   svc,
		Commands:   cmd,
	}
}

// newTestContainerdProvider 构造一个默认值齐备的 containerd provider。
func newTestContainerdProvider(t *testing.T, opts Options) *containerdProvider {
	t.Helper()
	prov, err := NewProvider(clusterconfig.RuntimeSpec{}, opts)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cp, ok := prov.(*containerdProvider)
	if !ok {
		t.Fatalf("provider 类型是 %T，期望 *containerdProvider", prov)
	}
	return cp
}

// newTestCustomProvider 构造一个默认值齐备的 custom provider。
func newTestCustomProvider(t *testing.T, cfg clusterconfig.CustomRuntimeSpec, opts Options) *customProvider {
	t.Helper()
	prov, err := NewProvider(clusterconfig.RuntimeSpec{Custom: cfg}, opts)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cp, ok := prov.(*customProvider)
	if !ok {
		t.Fatalf("provider 类型是 %T，期望 *customProvider", prov)
	}
	return cp
}

// managedSpecFor 构造一个只含 containerd 受管文件的渲染结果。
// config 为空表示不包含 config.toml；hosts 的键是仓库名。
func managedSpecFor(t *testing.T, paths constants.Paths, config string, hosts map[string]string) *render.Spec {
	t.Helper()
	spec := &render.Spec{}
	rc := &render.Context{Paths: paths}
	if config != "" {
		spec.AddFile(render.ContainerdConfigPath(rc), []byte(config), 0o644, ComponentContainerd, render.KindConf, render.ActionRestart)
	}
	names := make([]string, 0, len(hosts))
	for host := range hosts {
		names = append(names, host)
	}
	sortStrings(names)
	for _, host := range names {
		spec.AddFile(filepath.Join(render.ContainerdCertsDir(rc), host, "hosts.toml"),
			[]byte(hosts[host]), 0o644, ComponentContainerd, render.KindConf, render.ActionNone)
	}
	spec.Sort()
	return spec
}

// sortStrings 是 sort.Strings 的小包装，避免测试文件到处导入 sort。
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// listFiles 返回 root 下所有普通文件的相对路径。
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if _, err := os.Stat(root); err != nil {
		return out
	}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", root, err)
	}
	sortStrings(out)
	return out
}
