package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/constants"
	"kubemono/internal/render"
)

// TestApplyConfigClassification 验证 config.toml 变化重启、仅 certs.d 变化不重启。
func TestApplyConfigClassification(t *testing.T) {
	ctx := context.Background()

	t.Run("config.toml 变化 → 重启", func(t *testing.T) {
		root := t.TempDir()
		paths := constants.NewPaths(root)
		oldContent := "version = 2\nroot = \"/var/lib/containerd\"\n"
		old := managedSpecFor(t, paths, oldContent, nil)
		desired := managedSpecFor(t, paths, "version = 2\nroot = \"/var/lib/containerd\"\nsandbox = \"pause:3.10\"\n", nil)
		// Current 描述的是“上一次 kubemono 写下的内容”，因此磁盘上必须有旧文件。
		writeTestFile(t, render.ContainerdConfigPath(&render.Context{Paths: paths}), oldContent, 0o644)

		svc := newFakeServices(DefaultServiceName)
		opts := containerdOptions(root, svc, &fakeRunner{})
		opts.Current = old
		prov := newTestContainerdProvider(t, opts)

		plan, err := prov.ApplyConfig(ctx, desired)
		if err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		if plan.Decision != DecisionRestart {
			t.Errorf("判定为 %s，期望 restart", plan.Decision)
		}
		if n := svc.countActions("restart"); n != 1 {
			t.Errorf("重启 %d 次，期望 1 次（动作：%v）", n, svc.recorded())
		}
		if !plan.Restarted {
			t.Error("plan.Restarted 应为 true")
		}
		data, err := os.ReadFile(render.ContainerdConfigPath(&render.Context{Paths: paths}))
		if err != nil {
			t.Fatalf("配置未写入: %v", err)
		}
		if string(data) != "version = 2\nroot = \"/var/lib/containerd\"\nsandbox = \"pause:3.10\"\n" {
			t.Errorf("配置内容不正确: %q", data)
		}
		if len(plan.Applied) != 1 || plan.Applied[0].BackupPath == "" {
			// 旧文件存在，必须备份（回滚的基础）。
			t.Errorf("应备份旧配置: %+v", plan.Applied)
		} else if filepath.Dir(plan.Applied[0].BackupPath) != plan.BackupDir {
			// 不能出现 /backup/<ts>/<ts>/<file> 的双时间戳。
			t.Errorf("备份路径应为 %s/<file>，实际 %s", plan.BackupDir, plan.Applied[0].BackupPath)
		}
	})

	t.Run("只有 certs.d 变化 → reload，不重启", func(t *testing.T) {
		root := t.TempDir()
		paths := constants.NewPaths(root)
		old := managedSpecFor(t, paths, "version = 2\n", map[string]string{"reg.example.com": "server = \"https://reg.example.com\"\n"})
		desired := managedSpecFor(t, paths, "version = 2\n", map[string]string{"reg.example.com": "server = \"https://mirror.example.com\"\n"})

		svc := newFakeServices(DefaultServiceName)
		opts := containerdOptions(root, svc, &fakeRunner{})
		opts.Current = old
		prov := newTestContainerdProvider(t, opts)

		plan, err := prov.ApplyConfig(ctx, desired)
		if err != nil {
			t.Fatalf("ApplyConfig: %v", err)
		}
		if plan.Decision != DecisionReload {
			t.Errorf("判定为 %s，期望 reload", plan.Decision)
		}
		if n := svc.countActions("restart"); n != 0 {
			t.Errorf("certs.d 变化不应重启，实际重启 %d 次", n)
		}
		if plan.Restarted {
			t.Error("plan.Restarted 应为 false")
		}
		hosts := filepath.Join(render.ContainerdCertsDir(&render.Context{Paths: paths}), "reg.example.com", "hosts.toml")
		data, err := os.ReadFile(hosts)
		if err != nil {
			t.Fatalf("hosts.toml 未写入: %v", err)
		}
		if string(data) != "server = \"https://mirror.example.com\"\n" {
			t.Errorf("hosts.toml 内容不正确: %q", data)
		}
	})

	t.Run("无变化 → none", func(t *testing.T) {
		root := t.TempDir()
		paths := constants.NewPaths(root)
		spec := managedSpecFor(t, paths, "version = 2\n", map[string]string{"reg.example.com": "server = \"https://reg.example.com\"\n"})
		// 先落盘，再用磁盘状态（Options.Current 为空）比较。
		svc := newFakeServices(DefaultServiceName)
		opts := containerdOptions(root, svc, &fakeRunner{})
		prov := newTestContainerdProvider(t, opts)
		if _, err := prov.ApplyConfig(ctx, spec); err != nil {
			t.Fatalf("首次 ApplyConfig: %v", err)
		}
		plan, err := prov.ApplyConfig(ctx, spec)
		if err != nil {
			t.Fatalf("第二次 ApplyConfig: %v", err)
		}
		if plan.Decision != DecisionNone || len(plan.Changes) != 0 {
			t.Errorf("重复应用应为 none/无变化，实际 %s/%d", plan.Decision, len(plan.Changes))
		}
		if n := svc.countActions("restart"); n != 1 {
			t.Errorf("只应在首次重启一次，实际 %d", n)
		}
	})
}

// TestClassifyChanges 覆盖判定规则本身，包括保守分支。
func TestClassifyChanges(t *testing.T) {
	const cfg = "/etc/containerd/config.toml"
	const certs = "/etc/containerd/certs.d"
	change := func(p string) render.FileChange {
		return render.FileChange{File: render.File{Path: p}, Kind: render.ChangeModified}
	}
	cases := []struct {
		name    string
		changes []render.FileChange
		want    ConfigDecision
	}{
		{"无变化", nil, DecisionNone},
		{"config.toml", []render.FileChange{change(cfg)}, DecisionRestart},
		{"certs.d", []render.FileChange{change(certs + "/a/hosts.toml")}, DecisionReload},
		{"多个 certs.d", []render.FileChange{change(certs + "/a/hosts.toml"), change(certs + "/b/hosts.toml")}, DecisionReload},
		{"混合", []render.FileChange{change(certs + "/a/hosts.toml"), change(cfg)}, DecisionRestart},
		{"未知路径保守重启", []render.FileChange{change("/etc/containerd/other.toml")}, DecisionRestart},
		{"相似前缀不算 certs.d", []render.FileChange{change(certs + "-backup/x")}, DecisionRestart},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyChanges(cfg, certs, tc.changes); got != tc.want {
				t.Errorf("ClassifyChanges = %s，期望 %s", got, tc.want)
			}
		})
	}
}

// TestLiveSpecAndDesiredHash 验证“有效配置摘要”两侧算法一致。
func TestLiveSpecAndDesiredHash(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	spec := managedSpecFor(t, paths, "version = 2\n", map[string]string{"reg.example.com": "server = \"https://reg.example.com\"\n"})

	svc := newFakeServices(DefaultServiceName)
	opts := containerdOptions(root, svc, &fakeRunner{})
	prov := newTestContainerdProvider(t, opts)
	if _, err := prov.ApplyConfig(context.Background(), spec); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	live, err := LiveSpec(paths)
	if err != nil {
		t.Fatalf("LiveSpec: %v", err)
	}
	if got, want := ConfigHash(ManagedFiles(live)), DesiredConfigHash(spec); got != want {
		t.Errorf("磁盘摘要 %s != 期望摘要 %s", got, want)
	}

	// 手工改坏 config.toml 后摘要必须变化。
	cfg := render.ContainerdConfigPath(&render.Context{Paths: paths})
	writeTestFile(t, cfg, "version = 2\nroot = \"/tmp\"\n", 0o644)
	live, err = LiveSpec(paths)
	if err != nil {
		t.Fatalf("LiveSpec: %v", err)
	}
	if got := ConfigHash(ManagedFiles(live)); got == DesiredConfigHash(spec) {
		t.Error("配置漂移后摘要不应再相等")
	}
}

// TestManagedFilesFiltering 只把 containerd 组件拥有的文件算作受管配置。
func TestManagedFilesFiltering(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	spec := managedSpecFor(t, paths, "version = 2\n", map[string]string{"reg.example.com": "server = \"x\"\n"})
	spec.AddFile("/etc/kubernetes/manifests/kube-apiserver.yaml", []byte("kind: Pod\n"), 0o644,
		"kube-apiserver", render.KindManifest, render.ActionRecreate)
	got := ManagedFiles(spec)
	if len(got) != 2 {
		t.Fatalf("受管文件 = %v，期望 2 个", got)
	}
	for _, f := range got {
		if !strings.Contains(f.Path, "containerd") {
			t.Errorf("受管文件混入了非 containerd 路径：%s", f.Path)
		}
	}
}

// TestDesiredConfigHashCustomIsEmpty custom 运行时没有受管配置，摘要为空。
func TestDesiredConfigHashCustomIsEmpty(t *testing.T) {
	spec := &render.Spec{}
	spec.AddFile("/etc/kubelet/config.yaml", []byte("x"), 0o644, "kubelet", render.KindConfig, render.ActionRestart)
	if got := DesiredConfigHash(spec); got != "" {
		t.Errorf("非 containerd 组件的摘要应为空，实际 %s", got)
	}
}

// TestLiveSpecKeepsRealMode：certs.d/hosts.toml 含 registry 凭据，渲染为 0600；
// LiveSpec 必须记录真实权限，否则 ComputeChanges 每次都判成漂移。
func TestLiveSpecKeepsRealMode(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	rc := &render.Context{Paths: paths}
	cfg := render.ContainerdConfigPath(rc)
	hostsPath := render.ContainerdCertsDir(rc) + "/docker.io/hosts.toml"
	writeTestFile(t, cfg, "version = 2\n", 0o644)
	writeTestFile(t, hostsPath, "server = \"https://docker.io\"\n", 0o600)

	live, err := LiveSpec(paths)
	if err != nil {
		t.Fatalf("LiveSpec: %v", err)
	}
	found := false
	for _, f := range live.Files {
		if f.Path == hostsPath {
			found = true
			if f.Mode != 0o600 {
				t.Fatalf("hosts.toml 权限 = %04o, want 0600", f.Mode)
			}
		}
	}
	if !found {
		t.Fatalf("LiveSpec 缺少 %s", hostsPath)
	}
	desired := &render.Spec{}
	desired.AddFile(cfg, []byte("version = 2\n"), 0o644, ComponentContainerd, render.KindConf, render.ActionRestart)
	desired.AddFile(hostsPath, []byte("server = \"https://docker.io\"\n"), 0o600, ComponentContainerd, render.KindConf, render.ActionNone)
	if changes := render.ComputeChanges(live, desired); len(changes) != 0 {
		t.Fatalf("内容/权限一致时不应有变化：%+v", changes)
	}
}
