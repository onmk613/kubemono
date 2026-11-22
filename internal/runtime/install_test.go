package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// TestContainerdInstallPlacesBinariesDirectly 验证二进制直接落到 /usr/local/bin、CNI 解压与幂等。
func TestContainerdInstallPlacesBinariesDirectly(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	stageRuntimeArtifacts(t, root)
	opts := containerdOptions(root, newFakeServices(), &fakeRunner{})
	prov := newTestContainerdProvider(t, opts)
	ctx := context.Background()

	if err := prov.Install(ctx); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// 1. 二进制直接落在 /usr/local/bin（含 runc，来自独立组件）。
	installDir := opts.InstallDir
	for _, name := range []string{"containerd", "containerd-shim-runc-v2", "ctr", "runc"} {
		p := filepath.Join(installDir, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s 未安装: %v", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s 不应是软链", p)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s 不可执行（%04o）", p, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(paths.Kubemono(), "bin")); !os.IsNotExist(err) {
		t.Fatalf("/usr/local/kubemono/bin 不应存在: %v", err)
	}

	// 3. CNI 插件安装到 Paths.CNIBin，权限保留。
	bridge := filepath.Join(paths.CNIBin(), "bridge")
	info, err := os.Stat(bridge)
	if err != nil {
		t.Fatalf("CNI 插件未安装: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("CNI 插件权限为 %04o，期望 0755", info.Mode().Perm())
	}

	// 4. unit 落盘。
	unit, err := os.ReadFile(paths.Resolve(UnitPath))
	if err != nil {
		t.Fatalf("unit 未写入: %v", err)
	}
	if !strings.Contains(string(unit), "ExecStart=/usr/local/bin/containerd") {
		t.Errorf("unit 缺少 ExecStart: %s", unit)
	}

	// 5. 幂等：第二次 Install 不报错、内容不变。
	before, err := fsutil.SHA256File(filepath.Join(installDir, "containerd"))
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.Install(ctx); err != nil {
		t.Fatalf("第二次 Install: %v", err)
	}
	after, err := fsutil.SHA256File(filepath.Join(installDir, "containerd"))
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("幂等性被破坏：%s -> %s", before, after)
	}

	// 6. 物料更新后重新安装会替换内容。
	src := filepath.Join(artifactDir(root), "bin", "runtime", "containerd")
	writeTestFile(t, src, "containerd-binary-v2", 0o755)
	if err := prov.Install(ctx); err != nil {
		t.Fatalf("第三次 Install: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(installDir, "containerd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "containerd-binary-v2" {
		t.Errorf("二进制未更新，内容为 %q", data)
	}
}

// TestContainerdInstallMissingRequiredBinary 缺少必需物料时报出可操作的错误。
func TestContainerdInstallMissingRequiredBinary(t *testing.T) {
	root := t.TempDir()
	// 不放任何物料，先触发运行时二进制的缺失。
	prov := newTestContainerdProvider(t, containerdOptions(root, newFakeServices(), &fakeRunner{}))
	err := prov.Install(context.Background())
	if err == nil {
		t.Fatal("缺少必需二进制时应报错")
	}
	if !strings.Contains(err.Error(), "containerd") || !strings.Contains(err.Error(), "bundle load") {
		t.Errorf("错误信息应指明缺失的二进制与检查动作: %v", err)
	}
}

// TestContainerdInstallMissingCNIPlugins 物料缺少 CNI 插件二进制时报错。
func TestContainerdInstallMissingCNIPlugins(t *testing.T) {
	root := t.TempDir()
	stageRuntimeBinaries(t, root)

	prov := newTestContainerdProvider(t, containerdOptions(root, newFakeServices(), &fakeRunner{}))
	err := prov.Install(context.Background())
	if err == nil {
		t.Fatal("缺少 CNI 插件时应报错")
	}
	if !strings.Contains(err.Error(), "CNI") {
		t.Errorf("错误信息应提到 CNI: %v", err)
	}
	// 默认是全量插件：缺失时要一次列全，而不是在第一个缺失处中断。
	if !strings.Contains(err.Error(), "bridge") || !strings.Contains(err.Error(), "vrf") {
		t.Errorf("错误信息应列出全部缺失插件（含 bridge 与 vrf）: %v", err)
	}
}

// TestRenderUnitContent 校验 要求的 unit 指令。
func TestRenderUnitContent(t *testing.T) {
	content, err := RenderUnit()
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	unit := string(content)
	for _, want := range []string{
		"ExecStart=/usr/local/bin/containerd",
		"Restart=always",
		"Delegate=yes",
		"KillMode=process",
		"LimitNOFILE=1048576",
		"OOMScoreAdjust=-999",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit 缺少 %q:\n%s", want, unit)
		}
	}
}

// TestLocateBinaryLayouts 校验物料只接受 canonical 的 bin/<category>/<name> 布局。
func TestLocateBinaryLayouts(t *testing.T) {
	bin := Binary{Name: "containerd", Component: ComponentContainerd, Category: "runtime", Version: "v1.7.24"}

	t.Run("canonical 布局", func(t *testing.T) {
		root := t.TempDir()
		dir := artifactDir(root)
		writeTestFile(t, filepath.Join(dir, "bin", "runtime", "containerd"), "flat", 0o755)
		got, err := locateBinary(dir, bin)
		if err != nil {
			t.Fatalf("locateBinary: %v", err)
		}
		rel, err := filepath.Rel(dir, got)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.ToSlash(rel) != "bin/runtime/containerd" {
			t.Errorf("应优先取 bin/runtime/containerd，实际 %s", rel)
		}
	})

	t.Run("清单优先", func(t *testing.T) {
		root := t.TempDir()
		dir := artifactDir(root)
		writeTestFile(t, filepath.Join(dir, "bin", "runtime", "containerd"), "flat", 0o755)
		body := "nested-from-manifest"
		writeTestFile(t, filepath.Join(dir, "bin", "runtime", "nested", "containerd"), body, 0o755)
		m := &artifact.Meta{
			FormatVersion: artifact.FormatVersion,
			Release:       bom.MustLoad().Release,
			Kubernetes:    bom.MustLoad().Kubernetes,
			Arch:          "amd64",
			OS:            "linux",
			Files: []artifact.FileEntry{
				{
					Path: "bin/runtime/nested/containerd", SHA256: fsutil.SHA256Bytes([]byte(body)),
					Size: int64(len(body)), Mode: "0755", Component: ComponentContainerd, Category: "runtime",
				},
			},
		}
		data, err := artifact.MarshalMeta(m)
		if err != nil {
			t.Fatalf("序列化清单: %v", err)
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, artifact.MetaName), data, 0o644); err != nil {
			t.Fatalf("写清单: %v", err)
		}
		got, err := locateBinary(dir, bin)
		if err != nil {
			t.Fatalf("locateBinary: %v", err)
		}
		if !strings.Contains(got, "nested") {
			t.Errorf("清单记录的路径优先，实际 %s", got)
		}
	})

	t.Run("缺失时给出可操作错误", func(t *testing.T) {
		root := t.TempDir()
		_, err := locateBinary(artifactDir(root), bin)
		if err == nil {
			t.Fatal("物料缺失时应报错")
		}
		if !strings.Contains(err.Error(), "bundle load") {
			t.Errorf("错误应告诉操作者如何修复: %v", err)
		}
	})
}

// TestMissingBinaries 校验 Check 的二进制判据。
func TestMissingBinaries(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	bins := Binaries(containerdSpec(), bom.MustLoad())

	if missing := MissingBinaries(paths, "", bins); len(missing) != 4 {
		t.Fatalf("未安装时应缺少 4 个必需二进制，实际 %v", missing)
	}

	stageRuntimeArtifacts(t, root)
	prov := newTestContainerdProvider(t, containerdOptions(root, newFakeServices(), &fakeRunner{}))
	if err := prov.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if missing := MissingBinaries(paths, containerdOptions(root, nil, nil).InstallDir, bins); len(missing) != 0 {
		t.Fatalf("安装后不应缺少二进制，实际 %v", missing)
	}
}
