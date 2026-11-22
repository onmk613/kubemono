package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/bom"
	"kubemono/internal/constants"
)

// TestCustomInstallInstallsCNIPlugins 断言 custom 也安装 CNI 插件。
func TestCustomInstallInstallsCNIPlugins(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	stageRuntimeArtifacts(t, root)

	opts := containerdOptions(root, newFakeServices("crio"), &fakeRunner{})
	prov := newTestCustomProvider(t, v1CustomSpec(), opts)
	if err := prov.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, name := range []string{"bridge", "host-local", "loopback", "portmap"} {
		path := filepath.Join(paths.CNIBin(), name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("CNI 插件 %s 未安装: %v", name, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("CNI 插件 %s 不可执行（%04o）", name, info.Mode().Perm())
		}
	}

	// custom 不写 containerd unit。
	if _, err := os.Stat(paths.Resolve(UnitPath)); !os.IsNotExist(err) {
		t.Errorf("runtime.type=custom 不应写 containerd unit（err=%v）", err)
	}
}

// TestCustomInstallMissingCNIPlugins custom 同样要求 CNI 插件物料。
func TestCustomInstallMissingCNIPlugins(t *testing.T) {
	root := t.TempDir()
	stageRuntimeBinaries(t, root)

	opts := containerdOptions(root, newFakeServices("crio"), &fakeRunner{})
	prov := newTestCustomProvider(t, v1CustomSpec(), opts)
	err := prov.Install(context.Background())
	if err == nil {
		t.Fatal("缺少 CNI 插件时应报错")
	}
	if !strings.Contains(err.Error(), "CNI") {
		t.Errorf("错误信息应提到 CNI: %v", err)
	}
	if !strings.Contains(err.Error(), "bridge") || !strings.Contains(err.Error(), "vrf") {
		t.Errorf("错误信息应列出全部缺失插件（含 bridge 与 vrf）: %v", err)
	}
}

// TestCustomInstallDryRunDoesNotWriteCNIPlugins dry-run 不写插件。
func TestCustomInstallDryRunDoesNotWriteCNIPlugins(t *testing.T) {
	root := t.TempDir()
	stageRuntimeArtifacts(t, root)

	opts := containerdOptions(root, newFakeServices("crio"), &fakeRunner{})
	opts.DryRun = true
	prov := newTestCustomProvider(t, v1CustomSpec(), opts)

	before := listFiles(t, root)
	if err := prov.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	after := listFiles(t, root)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("dry-run 不应写任何文件\n之前: %v\n之后: %v", before, after)
	}
}

// TestMissingCNIPlugins 缺失或不可执行都算未就绪。
func TestMissingCNIPlugins(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	stageRuntimeArtifacts(t, root)
	opts := containerdOptions(root, newFakeServices("crio"), &fakeRunner{})
	if err := installCNIPlugins(opts); err != nil {
		t.Fatalf("installCNIPlugins: %v", err)
	}
	if got := MissingCNIPlugins(paths, bom.MustLoad()); len(got) != 0 {
		t.Fatalf("安装后不应缺少 CNI 插件，实际 %v", got)
	}

	bridge := filepath.Join(paths.CNIBin(), "bridge")
	if err := os.Remove(bridge); err != nil {
		t.Fatal(err)
	}
	if got := MissingCNIPlugins(paths, bom.MustLoad()); len(got) != 1 || got[0] != "bridge" {
		t.Errorf("删除 bridge 后缺失列表 = %v，期望 [bridge]", got)
	}

	if err := os.WriteFile(bridge, []byte("not-executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := MissingCNIPlugins(paths, bom.MustLoad()); len(got) != 1 || got[0] != "bridge" {
		t.Errorf("不可执行的 bridge 应计为缺失，实际 %v", got)
	}

	if got := MissingCNIPlugins(paths, nil); len(got) != 0 {
		t.Errorf("BOM 为空时不应判定，实际 %v", got)
	}
}
