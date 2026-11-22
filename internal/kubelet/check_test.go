package kubelet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/bom"
	"kubemono/internal/constants"
	"kubemono/internal/kubelet/kubeletfake"
	"kubemono/internal/render"
)

// checkFixture 准备一个节点：其渲染出的 kubelet 文件已落盘、二进制已安装。
type checkFixture struct {
	paths    constants.Paths
	spec     *render.Spec
	inst     *BinaryInstaller
	svc      *kubeletfake.MemoryServiceManager
	node     *kubeletfake.FakeNodeClient
	noClient bool
}

func newCheckFixture(t *testing.T, mode string) *checkFixture {
	t.Helper()
	rc := renderContextFor(t, clusterForTest(mode), "w1")
	spec, err := render.Render(rc, render.DefaultBuilders()...)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// 把渲染结果落盘，模拟“已经安装好”。
	for _, f := range spec.FilesOf(constants.ComponentKubelet) {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.Path, f.Content, os.FileMode(f.Mode)); err != nil {
			t.Fatal(err)
		}
	}
	release := bom.MustLoad().Release
	artifactFixture(t, rc.Paths, release, "bin/k8s/kubelet", "kubelet-bin")
	inst := &BinaryInstaller{Paths: rc.Paths, Release: release, BOM: bom.MustLoad()}
	if _, err := inst.Install(context.Background(), constants.ComponentKubelet); err != nil {
		t.Fatalf("Install: %v", err)
	}
	svc := kubeletfake.NewMemoryServiceManager()
	svc.Active[UnitName] = true
	return &checkFixture{
		paths: rc.Paths,
		spec:  spec,
		inst:  inst,
		svc:   svc,
		node:  &kubeletfake.FakeNodeClient{Ready: map[string]bool{"w1": true}},
	}
}

func (f *checkFixture) opts() CheckOptions {
	opts := CheckOptions{
		Paths:          f.paths,
		Spec:           f.spec,
		Node:           "w1",
		Installer:      f.inst,
		ServiceManager: f.svc,
	}
	if !f.noClient {
		opts.NodeClient = f.node
	}
	return opts
}

func TestCheckTable(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, f *checkFixture)
		want    bool
		wantErr string
	}{
		{name: "全部就绪", mutate: func(*testing.T, *checkFixture) {}, want: true},
		{
			name: "配置文件缺失",
			mutate: func(t *testing.T, f *checkFixture) {
				if err := os.Remove(f.paths.KubeletConfig()); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			name: "配置文件内容被篡改",
			mutate: func(t *testing.T, f *checkFixture) {
				if err := os.WriteFile(f.paths.KubeletConfig(), []byte("tampered"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			name: "二进制漂移",
			mutate: func(t *testing.T, f *checkFixture) {
				dir := f.inst.InstallDir()
				if err := os.WriteFile(filepath.Join(dir, "kubelet"), []byte("drift"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			name: "unit 未运行",
			mutate: func(t *testing.T, f *checkFixture) {
				f.svc.Active[UnitName] = false
			},
			want: false,
		},
		{
			name: "节点未 Ready",
			mutate: func(t *testing.T, f *checkFixture) {
				f.node.Ready["w1"] = false
			},
			want: false,
		},
		{
			name: "没有 Kubernetes 客户端",
			mutate: func(t *testing.T, f *checkFixture) {
				f.noClient = true
			},
			want: false,
		},
		{
			name: "节点名为空",
			mutate: func(t *testing.T, f *checkFixture) {
				_ = f
			},
			want:    false,
			wantErr: "节点名",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckFixture(t, "binary")
			tc.mutate(t, f)
			opts := f.opts()
			if tc.name == "节点名为空" {
				opts.Node = ""
			}
			done, err := Check(context.Background(), opts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Check 错误 = %v, want 包含 %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if done != tc.want {
				t.Fatalf("Check = %v, want %v", done, tc.want)
			}
		})
	}
}

func TestCheckStaticPodRequiresActiveUnit(t *testing.T) {
	// 两种模式都由 systemd 运行 kubelet：unit 不在运行就不算完成。
	f := newCheckFixture(t, "staticpod")
	f.svc.Active[UnitName] = false
	done, err := Check(context.Background(), f.opts())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("staticpod 模式下 kubelet.service 未运行时不应报告完成")
	}
	f.svc.Active[UnitName] = true
	done, err = Check(context.Background(), f.opts())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatal("unit 运行且节点 Ready 时应报告完成")
	}
}

func TestCheckRejectsEmptySpec(t *testing.T) {
	_, err := Check(context.Background(), CheckOptions{Paths: constants.NewPaths(t.TempDir()), Spec: &render.Spec{}})
	if err == nil || !strings.Contains(err.Error(), "kubelet") {
		t.Fatalf("没有 kubelet 文件时必须报错: %v", err)
	}
}

func TestCheckTreatsNodeReadyErrorAsNotDone(t *testing.T) {
	f := newCheckFixture(t, "binary")
	f.node.NodeReadyErr = os.ErrDeadlineExceeded
	done, err := Check(context.Background(), f.opts())
	if err != nil {
		t.Fatalf("临时 API 错误不应让 Check 失败: %v", err)
	}
	if done {
		t.Fatal("查询失败时不能报告完成")
	}
}

func TestCheckToleratesMissingServiceManagerError(t *testing.T) {
	f := newCheckFixture(t, "binary")
	opts := f.opts()
	opts.ServiceManager = nil
	_, err := Check(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), UnitName) {
		t.Fatalf("binary 模式缺少 ServiceManager 时必须报错: %v", err)
	}
}
