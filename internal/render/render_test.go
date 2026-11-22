package render

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/util/output"
)

func testContext(root string) *Context {
	cluster := &clusterconfig.Cluster{}
	clusterconfig.SetDefaults(cluster)
	return &Context{
		Cluster: cluster,
		Paths:   constants.NewPaths(root),
		Release: "v1.31.4-km.1",
	}
}

func newSpec(root string, files ...File) *Spec {
	s := &Spec{Node: "m1", ClusterID: "cid", Release: "v1.31.4-km.1"}
	for _, f := range files {
		if !filepath.IsAbs(f.Path) {
			f.Path = filepath.Join(root, f.Path)
		}
		s.Add(f)
	}
	s.Sort()
	return s
}

func TestSpecValidateRejectsUnsafeFiles(t *testing.T) {
	cases := []struct {
		name string
		file File
	}{
		{"relative path", File{Path: "etc/foo", Component: "c"}},
		{"traversal", File{Path: "/etc/../etc/shadow", Component: "c"}},
		{"unclean", File{Path: "/etc//foo", Component: "c"}},
		{"no component", File{Path: "/etc/foo"}},
		{"empty path", File{Component: "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Spec{}
			s.Add(tc.file)
			if err := s.Validate(); err == nil {
				t.Fatalf("expected Validate to reject %+v", tc.file)
			}
		})
	}
}

// TestSpecAddForcesSecretMode 记录安全默认值：标记为 Secret 的文件绝不会变成
// 组/其他用户可读，即使渲染器忘了设置。
func TestSpecAddForcesSecretMode(t *testing.T) {
	s := &Spec{}
	s.Add(File{Path: "/etc/kubernetes/pki/ca.key", Component: "certs", Secret: true, Mode: 0o644})
	if s.Files[0].Mode != 0o600 {
		t.Fatalf("mode = %04o, want 0600", s.Files[0].Mode)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestSpecValidateRejectsDuplicates(t *testing.T) {
	s := &Spec{}
	s.Add(File{Path: "/etc/kubernetes/manifests/a.yaml", Component: "kube-apiserver", Mode: 0o644})
	s.Add(File{Path: "/etc/kubernetes/manifests/a.yaml", Component: "kubelet", Mode: 0o644})
	if err := s.Validate(); err == nil {
		t.Fatal("duplicate paths must be rejected")
	}
}

func TestSpecHashIsDeterministic(t *testing.T) {
	a := newSpec("/root", File{Path: "/a", Content: []byte("1"), Component: "x"}, File{Path: "/b", Content: []byte("2"), Component: "x"})
	b := newSpec("/root", File{Path: "/b", Content: []byte("2"), Component: "x"}, File{Path: "/a", Content: []byte("1"), Component: "x"})
	if a.Hash() != b.Hash() {
		t.Fatalf("spec hash must not depend on insertion order: %s vs %s", a.Hash(), b.Hash())
	}
	c := newSpec("/root", File{Path: "/a", Content: []byte("1!"), Component: "x"}, File{Path: "/b", Content: []byte("2"), Component: "x"})
	if a.Hash() == c.Hash() {
		t.Fatal("spec hash must change when content changes")
	}
}

func TestRenderRunsOnlyApplicableBuilders(t *testing.T) {
	var called []string
	master := stubBuilder{name: "control-plane", applies: func(c *Context) bool { return c.IsMaster() }, onRender: func() { called = append(called, "cp") }}
	all := stubBuilder{name: "kubelet", applies: func(*Context) bool { return true }, onRender: func() { called = append(called, "kubelet") }}
	c := testContext(t.TempDir())
	c.Host = hostFor("m1", true, "10.0.0.1")

	spec, err := Render(c, master, all)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !reflect.DeepEqual(called, []string{"cp", "kubelet"}) {
		t.Fatalf("called = %v", called)
	}
	if spec.Node != "m1" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
}

func TestRenderWorkerSkipsControlPlane(t *testing.T) {
	b := stubBuilder{name: "control-plane", applies: func(c *Context) bool { return c.IsMaster() }}
	c := testContext(t.TempDir())
	c.Host = hostFor("w1", false, "10.0.0.11")
	spec, err := Render(c, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Files) != 0 {
		t.Fatalf("worker must not receive control plane files: %+v", spec.Files)
	}
}

func TestComputeChanges(t *testing.T) {
	root := t.TempDir()
	oldSpec := newSpec(root,
		File{Path: filepath.Join(root, "keep"), Content: []byte("same"), Component: "c", Mode: 0o644},
		File{Path: filepath.Join(root, "mod"), Content: []byte("old"), Component: "c", Mode: 0o644},
		File{Path: filepath.Join(root, "gone"), Content: []byte("bye"), Component: "c", Mode: 0o644},
	)
	newSpec2 := newSpec(root,
		File{Path: filepath.Join(root, "keep"), Content: []byte("same"), Component: "c", Mode: 0o644},
		File{Path: filepath.Join(root, "mod"), Content: []byte("new"), Component: "c", Mode: 0o644},
		File{Path: filepath.Join(root, "added"), Content: []byte("hi"), Component: "c", Mode: 0o644},
	)
	changes := ComputeChanges(oldSpec, newSpec2)
	got := map[string]ChangeKind{}
	for _, ch := range changes {
		got[filepath.Base(ch.File.Path)] = ch.Kind
	}
	want := map[string]ChangeKind{"mod": ChangeModified, "added": ChangeAdded, "gone": ChangeRemoved}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}

func TestComputeChangesTreatsModeChangeAsModified(t *testing.T) {
	root := t.TempDir()
	oldSpec := newSpec(root, File{Path: filepath.Join(root, "kc"), Content: []byte("x"), Component: "kubelet", Mode: 0o644})
	newSpec2 := newSpec(root, File{Path: filepath.Join(root, "kc"), Content: []byte("x"), Component: "kubelet", Mode: 0o600})
	changes := ComputeChanges(oldSpec, newSpec2)
	if len(changes) != 1 || changes[0].Kind != ChangeModified {
		t.Fatalf("mode change must be a modification: %+v", changes)
	}
}

func TestPendingActionsPicksStrongest(t *testing.T) {
	changes := []FileChange{
		{File: File{Path: "/a", Component: "kubelet", OnChange: ActionRestart}, Kind: ChangeModified},
		{File: File{Path: "/b", Component: "kubelet", OnChange: ActionReload}, Kind: ChangeModified},
		{File: File{Path: "/c", Component: "certs", OnChange: ActionNone}, Kind: ChangeAdded},
		{File: File{Path: "/d", Component: "gone", OnChange: ActionRecreate}, Kind: ChangeRemoved},
	}
	got := PendingActions(changes)
	if got["kubelet"] != ActionRestart {
		t.Fatalf("kubelet action = %s, want restart", got["kubelet"])
	}
	if got["certs"] != ActionNone {
		t.Fatalf("certs action = %s, want none", got["certs"])
	}
	if _, ok := got["gone"]; ok {
		t.Fatal("removed files must not schedule a component action")
	}
}

func TestApplyAndRollback(t *testing.T) {
	root := t.TempDir()
	backup := filepath.Join(root, "backup")
	target := filepath.Join(root, "etc", "config.yaml")

	// 首次写入。
	newSpec2 := newSpec(root, File{Path: target, Content: []byte("v1"), Component: "kubelet", Mode: 0o600, OnChange: ActionRestart})
	changes := ComputeChanges(nil, newSpec2)
	applied, err := Apply(changes, ApplyOptions{BackupDir: backup, Timestamp: "t1", Reporter: output.Discard()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "v1" {
		t.Fatalf("target content = %q, err = %v", data, err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err = %v", info.Mode(), err)
	}

	// 修改：必须备份旧内容。
	modified := newSpec(root, File{Path: target, Content: []byte("v2"), Component: "kubelet", Mode: 0o600, OnChange: ActionRestart})
	applied2, err := Apply(ComputeChanges(newSpec2, modified), ApplyOptions{BackupDir: backup, Timestamp: "t2", Reporter: output.Discard()})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied2) != 1 || applied2[0].BackupPath == "" {
		t.Fatalf("expected a backup path: %+v", applied2)
	}
	backed, err := os.ReadFile(applied2[0].BackupPath)
	if err != nil || string(backed) != "v1" {
		t.Fatalf("backup content = %q, err = %v", backed, err)
	}

	// 回滚修改。
	if err := Rollback(applied2); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "v1" {
		t.Fatalf("after rollback content = %q, want v1", data)
	}

	// 回滚首次创建：文件必须消失。
	if err := Rollback(applied); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected the created file to be removed, stat err = %v", err)
	}
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "etc", "x")
	s := newSpec(root, File{Path: target, Content: []byte("v"), Component: "c"})
	if _, err := Apply(ComputeChanges(nil, s), ApplyOptions{DryRun: true, Reporter: output.Discard()}); err != nil {
		t.Fatalf("Apply dry-run: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("dry-run must not create files")
	}
}

func TestDesiredStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	store := NewStore(paths)
	spec := newSpec(filepath.Join(root, "real"),
		File{Path: paths.ManifestPath("kube-apiserver"), Content: []byte("kind: Pod"), Component: "kube-apiserver", Kind: KindManifest, Mode: 0o600, OnChange: ActionRecreate},
	)
	if err := store.Write(spec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	snap, err := store.Load()
	if err != nil || snap == nil {
		t.Fatalf("Load: %v (%v)", err, snap)
	}
	if snap.SpecHash != spec.Hash() {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
	restored, err := store.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Hash() != spec.Hash() {
		t.Fatalf("restored hash %s != %s", restored.Hash(), spec.Hash())
	}
}

func TestDrift(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "etc", "cfg")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := newSpec(root, File{Path: target, Content: []byte("expected"), Component: "c", Mode: 0o640})

	items, err := Drift(spec)
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if len(items) != 1 || items[0].State != DriftMissing {
		t.Fatalf("expected a missing file, got %+v", items)
	}

	if err := os.WriteFile(target, []byte("tampered"), 0o640); err != nil {
		t.Fatal(err)
	}
	items, _ = Drift(spec)
	if len(items) != 1 || items[0].State != DriftModified {
		t.Fatalf("expected a modified file, got %+v", items)
	}

	if err := os.WriteFile(target, []byte("expected"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	items, _ = Drift(spec)
	if len(items) != 1 || items[0].State != DriftModeChanged {
		t.Fatalf("expected a mode change, got %+v", items)
	}

	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	items, _ = Drift(spec)
	if len(items) != 0 {
		t.Fatalf("expected no drift, got %+v", items)
	}
}

// hostFor 构造测试用的 clusterconfig.Host。
func hostFor(name string, master bool, address string) *clusterconfig.Host {
	roles := []clusterconfig.Role{clusterconfig.RoleWorker}
	if master {
		roles = []clusterconfig.Role{clusterconfig.RoleMaster}
	}
	return &clusterconfig.Host{Name: name, Address: address, Roles: roles}
}

// stubBuilder 是用于演练 Render 的最小 Builder。
type stubBuilder struct {
	name     string
	applies  func(c *Context) bool
	onRender func()
}

func (s stubBuilder) Name() string { return s.name }

func (s stubBuilder) Applies(c *Context) bool {
	if s.applies == nil {
		return true
	}
	return s.applies(c)
}

func (s stubBuilder) Render(c *Context, spec *Spec) error {
	if s.onRender != nil {
		s.onRender()
	}
	spec.Add(File{
		Path:      c.Paths.KubeconfigPath(s.name + ".conf"),
		Content:   []byte(s.name),
		Component: s.name,
		Mode:      0o600,
		Kind:      KindKubeconfig,
		OnChange:  ActionNone,
	})
	return nil
}

// TestRegistryHostsRenderBasicAuth 守住 registry 认证：凭据必须渲染在真实 host 下，
// 否则配了 username/password 的用户只会得到 401。
func TestRegistryHostsRenderBasicAuth(t *testing.T) {
	const host = "reg.internal:5000"
	cluster := clusterFor("staticpod")
	cluster.Node.Runtime.Containerd.Registries.Configs = map[string]clusterconfig.RegistryAuthSpec{
		host: {Username: "kubemono", Password: "s3cret", CAFile: "/usr/local/kubemono/registry-ca.crt"},
	}
	c := builderContext(t, cluster, "w1")

	path := ContainerdCertsDir(c) + "/" + host + "/hosts.toml"
	hosts := RenderRegistryHosts(c)
	raw, ok := hosts[path]
	if !ok {
		t.Fatalf("没有渲染 %s，实际 %v", path, hosts)
	}
	text := string(raw)
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("kubemono:s3cret"))
	for _, want := range []string{
		`[host."https://` + host + `".header]`,
		`authorization = "` + wantAuth + `"`,
		`ca = "/usr/local/kubemono/registry-ca.crt"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("hosts.toml 缺少 %q：\n%s", want, text)
		}
	}

	// 凭据文件必须 0600：里面是明文 Basic 头。
	spec, err := Render(c, DefaultBuilders()...)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f, ok := spec.Find(path)
	if !ok {
		t.Fatalf("Render 没有产出 %s", path)
	}
	if f.Mode != 0o600 {
		t.Errorf("%s 权限 = %04o，必须是 0600（含 Basic 凭据）", path, f.Mode)
	}
}

// TestSandboxImageFollowsImageOverride 守住私有仓库/离线场景：
// 按组件覆盖 pause 镜像后，containerd 的 sandbox_image 必须跟着换，
// 否则所有 Pod 都会去 registry.k8s.io 拉 pause。
func TestSandboxImageFollowsImageOverride(t *testing.T) {
	cluster := clusterFor("staticpod")
	effective, err := bom.MustLoad().WithImageOverrides(map[string]string{
		"pause": "registry.internal:5000/mirror/pause:3.10.2",
	})
	if err != nil {
		t.Fatalf("WithImageOverrides: %v", err)
	}
	c := builderContext(t, cluster, "m1")
	c.BOM = effective
	conf, err := RenderContainerdConfig(c)
	if err != nil {
		t.Fatalf("RenderContainerdConfig: %v", err)
	}
	text := string(conf)
	want, err := c.Image("pause")
	if err != nil {
		t.Fatalf("Context.Image(pause): %v", err)
	}
	if !strings.Contains(want, "registry.internal:5000/mirror") {
		t.Fatalf("测试前提不成立：pause 引用没有套用镜像覆盖：%s", want)
	}
	if !strings.Contains(text, want) {
		t.Errorf("sandbox_image 必须是套用 repository 后的 %s：\n%s", want, text)
	}
	if strings.Contains(text, "registry.k8s.io/pause") {
		t.Errorf("sandbox_image 不得回落到 registry.k8s.io：\n%s", text)
	}
}

// TestKubeletCgroupDriverFollowsContainerd 守住 cgroup 驱动一致性：
// systemdCgroup=false 时 containerd 用 cgroupfs，kubelet 也必须用 cgroupfs，
// 否则 kubelet 会以"驱动不匹配"拒绝或异常管理容器。
func TestKubeletCgroupDriverFollowsContainerd(t *testing.T) {
	falseVal := false
	cluster := clusterFor("staticpod")
	cluster.Node.Runtime.Containerd.SystemdCgroup = &falseVal
	c := builderContext(t, cluster, "w1")
	spec, err := Render(c, DefaultBuilders()...)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f, ok := spec.Find(c.Paths.KubeletConfig())
	if !ok {
		t.Fatalf("没有渲染出 kubelet 配置 %s", c.Paths.KubeletConfig())
	}
	if !strings.Contains(string(f.Content), "cgroupDriver: cgroupfs") {
		t.Errorf("systemdCgroup=false 时 kubelet 必须是 cgroupfs：\n%s", f.Content)
	}
}

// TestContainerdConfigUsesContainerd1xSchema 锁住真实环境踩到的坑。
// BOM 固定 containerd v1.7.24，它只认 containerd 1.x 的 schema
// （[plugins."io.containerd.grpc.v1.cri"]）。写成 2.x 的顶层 `plugin_config` +
// `io.containerd.cri.v1.*` 时，1.7 会**整段静默忽略**：sandbox_image 退回内置的
// pause:3.8（离线包里没有这个镜像，Pod 起不来）、SystemdCgroup 退回 false
// （与 kubelet 的 systemd cgroup 驱动不一致）、registry.config_path 失效。
// 判据来自真机 `containerd config dump`。
func TestContainerdConfigUsesContainerd1xSchema(t *testing.T) {
	c := builderContext(t, clusterFor("staticpod"), "m1")
	conf, err := RenderContainerdConfig(c)
	if err != nil {
		t.Fatalf("RenderContainerdConfig: %v", err)
	}
	text := string(conf)

	if strings.Contains(text, "plugin_config") {
		t.Errorf("containerd 1.7 不认识顶层 plugin_config，必须用 plugins.<id>：\n%s", text)
	}
	if !strings.Contains(text, "io.containerd.grpc.v1.cri") {
		t.Errorf("缺少 1.x 的 CRI 插件段：\n%s", text)
	}
	// pause 镜像必须与 BOM 一致，否则离线包里没有运行时真正要的那个镜像。
	wantPause := "registry.k8s.io/pause:3.10.2"
	if b := bom.MustLoad(); b != nil {
		if img, err := b.Image("pause"); err == nil {
			wantPause = img.Reference()
		}
	}
	if !strings.Contains(text, wantPause) {
		t.Errorf("sandbox_image 必须是 BOM 里的 %s：\n%s", wantPause, text)
	}
	if !strings.Contains(text, "SystemdCgroup") || !strings.Contains(text, "true") {
		t.Errorf("默认必须启用 SystemdCgroup（与 kubelet 的 cgroup 驱动一致）：\n%s", text)
	}
	if !strings.Contains(text, "/etc/containerd/certs.d") {
		t.Errorf("registry.config_path 必须写进 CRI 插件段：\n%s", text)
	}
}

// TestDesiredStoreOmitsSecretsAndPrunes：desired 镜像不能保存 Secret 内容，
// 且切换配置后要删除不再声明的旧文件（否则旧 registry 凭据会永久留存）。
func TestDesiredStoreOmitsSecretsAndPrunes(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	store := NewStore(paths)
	normal := filepath.Join(root, "etc", "vm", "normal.conf")
	secret := filepath.Join(root, "etc", "vm", "hosts.toml")

	spec := &Spec{}
	spec.Add(File{Path: normal, Content: []byte("normal"), Mode: 0o644, Component: "c", Kind: KindConf})
	spec.Add(File{Path: secret, Content: []byte("password"), Mode: 0o600, Component: "c", Kind: KindConf, Secret: true})
	if err := store.Write(spec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), "files", "etc", "vm", "normal.conf")); err != nil {
		t.Fatalf("普通文件应镜像到 desired: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), "files", "etc", "vm", "hosts.toml")); !os.IsNotExist(err) {
		t.Fatalf("Secret 文件不应镜像到 desired: %v", err)
	}
	restored, err := store.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(restored.Files) != 1 || restored.Files[0].Path != normal {
		t.Fatalf("Restore 应只包含非 Secret 文件，got %+v", restored.Files)
	}

	// 新 spec 不再声明 normal：旧镜像必须被清理。
	if err := store.Write(&Spec{}); err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), "files", "etc", "vm", "normal.conf")); !os.IsNotExist(err) {
		t.Fatalf("未声明的旧 desired 文件应被删除: %v", err)
	}
}
