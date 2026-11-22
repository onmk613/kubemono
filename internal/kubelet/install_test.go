package kubelet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// artifactFixture 把一个组件二进制写入已导入的物料树。
func artifactFixture(t *testing.T, paths constants.Paths, release, rel, content string) string {
	t.Helper()
	p := filepath.Join(paths.ArtifactDir(release), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// metaFixture 写入描述单个二进制的最小 meta.json。
func metaFixture(t *testing.T, paths constants.Paths, release, bundlePath, component, category, content string) {
	t.Helper()
	m := artifact.Meta{
		FormatVersion: artifact.FormatVersion,
		Release:       release,
		Kubernetes:    "v1.31.4",
		Arch:          "amd64",
		OS:            "linux",
		Files: []artifact.FileEntry{
			{
				Path:      bundlePath,
				SHA256:    fsutil.SHA256Bytes([]byte(content)),
				Size:      int64(len(content)),
				Mode:      "0755",
				Component: component,
				Category:  category,
			},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ArtifactDir(release), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ArtifactDir(release), artifact.MetaName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallCopiesBinaryDirectlyAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	release := bom.MustLoad().Release
	artifactFixture(t, paths, release, "bin/k8s/kubelet", "kubelet-binary-1.31.4")
	inst := &BinaryInstaller{Paths: paths, Release: release, BOM: bom.MustLoad()}

	dir, err := inst.Install(context.Background(), constants.ComponentKubelet)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if want := paths.LocalBin(); dir != want {
		t.Fatalf("安装目录 = %s, want %s", dir, want)
	}
	dst := inst.InstallPath("kubelet")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("目标二进制不存在: %v", err)
	}
	if string(data) != "kubelet-binary-1.31.4" {
		t.Fatalf("目标内容 = %q", data)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("目标权限 = %04o, want 0755", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(paths.Kubemono(), "bin")); !os.IsNotExist(err) {
		t.Fatalf("/usr/local/kubemono/bin 不应存在: %v", err)
	}
	if ok, err := inst.Verify(context.Background(), constants.ComponentKubelet); err != nil || !ok {
		t.Fatalf("Verify = %v, %v, want true", ok, err)
	}

	before, _ := fsutil.SHA256File(dst)
	if _, err := inst.Install(context.Background(), constants.ComponentKubelet); err != nil {
		t.Fatalf("第二次 Install: %v", err)
	}
	after, _ := fsutil.SHA256File(dst)
	if before != after {
		t.Fatal("幂等安装不应改写二进制")
	}
}

func TestInstallSelectsSource(t *testing.T) {
	release := bom.MustLoad().Release
	cases := []struct {
		name       string
		setup      func(t *testing.T, paths constants.Paths) *BinaryInstaller
		wantErr    []string
		wantSource string
	}{
		{
			name: "BOM 回退路径",
			setup: func(t *testing.T, paths constants.Paths) *BinaryInstaller {
				artifactFixture(t, paths, release, "bin/k8s/kubelet", "bin-bom")
				return &BinaryInstaller{Paths: paths, Release: release}
			},
			wantSource: "bin/k8s/kubelet",
		},
		{
			name: "meta 记录的类别优先",
			setup: func(t *testing.T, paths constants.Paths) *BinaryInstaller {
				// 物料实际放在 bin/custom/ 下；BOM 声明的类别是 k8s。
				artifactFixture(t, paths, release, "bin/custom/kubelet", "bin-manifest")
				metaFixture(t, paths, release, "bin/custom/kubelet", constants.ComponentKubelet, "custom", "bin-manifest")
				return &BinaryInstaller{Paths: paths, Release: release}
			},
			wantSource: "bin/custom/kubelet",
		},
		{
			name: "manifest 哈希不一致",
			setup: func(t *testing.T, paths constants.Paths) *BinaryInstaller {
				artifactFixture(t, paths, release, "bin/k8s/kubelet", "tampered")
				metaFixture(t, paths, release, "bin/k8s/kubelet", constants.ComponentKubelet, "k8s", "original")
				return &BinaryInstaller{Paths: paths, Release: release}
			},
			wantErr: []string{"manifest", "重新导入"},
		},
		{
			name: "物料完全缺失",
			setup: func(t *testing.T, paths constants.Paths) *BinaryInstaller {
				return &BinaryInstaller{Paths: paths, Release: release}
			},
			wantErr: []string{"bundle load", "bin/k8s/kubelet"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := constants.NewPaths(t.TempDir())
			inst := tc.setup(t, paths)
			dir, err := inst.Install(context.Background(), constants.ComponentKubelet)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatal("应当报错")
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("错误 %q 缺少 %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "kubelet"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(paths.ArtifactDir(release), filepath.FromSlash(tc.wantSource)))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("安装内容 = %q, want %q（来自 %s）", got, want, tc.wantSource)
			}
		})
	}
}

func TestInstallFromManifestWithoutComponentUsesBasename(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	release := bom.MustLoad().Release
	artifactFixture(t, paths, release, "bin/k8s/kubelet", "no-component")
	metaFixture(t, paths, release, "bin/k8s/kubelet", "", "k8s", "no-component")
	inst := &BinaryInstaller{Paths: paths, Release: release}
	if _, err := inst.Install(context.Background(), constants.ComponentKubelet); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(inst.InstallPath("kubelet")); err != nil {
		t.Fatalf("目标二进制不存在: %v", err)
	}
}

func TestVerifyDetectsDrift(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	release := bom.MustLoad().Release
	artifactFixture(t, paths, release, "bin/k8s/kubelet", "expected")
	inst := &BinaryInstaller{Paths: paths, Release: release}
	if _, err := inst.Install(context.Background(), constants.ComponentKubelet); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, _ := inst.Verify(ctx, constants.ComponentKubelet); !ok {
		t.Fatal("刚安装完应通过 Verify")
	}
	dst := inst.InstallPath("kubelet")
	if err := os.WriteFile(dst, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := inst.Verify(ctx, constants.ComponentKubelet); ok {
		t.Fatal("二进制被篡改时 Verify 必须返回 false")
	}
	if _, err := inst.Install(ctx, constants.ComponentKubelet); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := inst.Verify(ctx, constants.ComponentKubelet); ok {
		t.Fatal("权限漂移时 Verify 必须返回 false")
	}
	// 物料缺失时是“需要重装”，不是错误。
	if err := os.Remove(filepath.Join(paths.ArtifactDir(release), "bin/k8s/kubelet")); err != nil {
		t.Fatal(err)
	}
	if ok, err := inst.Verify(ctx, constants.ComponentKubelet); ok || err != nil {
		t.Fatalf("物料缺失时 Verify = %v, %v, want false, nil", ok, err)
	}
}

func TestInstallRejectsUnknownComponentAndBadRelease(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	inst := &BinaryInstaller{Paths: paths, Release: bom.MustLoad().Release}
	if _, err := inst.Install(context.Background(), "not-a-component"); err == nil {
		t.Fatal("未知组件必须报错")
	}
	noRelease := &BinaryInstaller{Paths: paths}
	if _, err := noRelease.Install(context.Background(), constants.ComponentKubelet); err == nil || !strings.Contains(err.Error(), "Release") {
		t.Fatalf("release 为空时必须报错并说明原因: %v", err)
	}
}

// TestInstalledBinarySurvivesArtifactRemoval 固定"运行不依赖 kubemono 物料"：
// 安装后的二进制在 /usr/local/bin，删掉整个 artifacts 目录仍然可执行；
// 只是 Verify 会因为物料缺失返回 false（等待 node repair 重新导入）。
func TestInstalledBinarySurvivesArtifactRemoval(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	release := bom.MustLoad().Release
	artifactFixture(t, paths, release, "bin/k8s/kubelet", "kubelet-binary")
	inst := &BinaryInstaller{Paths: paths, Release: release}
	if _, err := inst.Install(context.Background(), constants.ComponentKubelet); err != nil {
		t.Fatal(err)
	}
	target := inst.InstallPath("kubelet")
	if err := os.RemoveAll(paths.ArtifactDir(release)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("删除物料后运行二进制不应消失: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("删除物料后运行二进制不可执行: %04o", info.Mode().Perm())
	}
	if ok, err := inst.Verify(context.Background(), constants.ComponentKubelet); ok || err != nil {
		t.Fatalf("物料缺失时 Verify = %v, %v, want false, nil", ok, err)
	}
}

// TestInstallReplacesSymlink 固定软链处理：/usr/local/bin 里即使已有指向同一内容的
// 软链，也必须替换成真实文件，运行不依赖 /usr/local/kubemono/bin。
func TestInstallReplacesSymlink(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	release := bom.MustLoad().Release
	src := artifactFixture(t, paths, release, "bin/k8s/kubelet", "kubelet-binary")
	if err := os.MkdirAll(paths.LocalBin(), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(paths.LocalBin(), "kubelet")
	if err := os.Symlink(src, target); err != nil {
		t.Fatal(err)
	}
	inst := &BinaryInstaller{Paths: paths, Release: release}
	if _, err := inst.Install(context.Background(), constants.ComponentKubelet); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("/usr/local/bin/kubelet 仍为软链")
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("权限 = %04o", info.Mode().Perm())
	}
}
