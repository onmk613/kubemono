package runtime

import (
	"fmt"
	"slices"
	"testing"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
)

// binaryNames 把 Binary 列表压成名字列表，便于断言。
func binaryNames(bins []Binary) []string {
	out := make([]string, 0, len(bins))
	for _, b := range bins {
		out = append(out, b.Name)
	}
	return out
}

// TestNewProviderTable 覆盖 表格的每一种 clusterconfig.RuntimeSpec 组合。
// 关键断言：
// - type 为空或 containerd → 托管 provider，运行时二进制包含 ctr；
// - type=custom → 用户托管 provider，不安装运行时二进制，
// 且 criSocket/serviceName 必须存在；
// - 未知 type 直接报错，绝不静默按 containerd 处理。
func TestNewProviderTable(t *testing.T) {
	cdVersion := bom.MustLoad().ComponentVersion("containerd")

	cases := []struct {
		name         string
		cfg          clusterconfig.RuntimeSpec
		wantErr      bool
		wantName     string
		wantSocket   string
		wantService  string
		wantContains []string
		wantExcludes []string
	}{
		{
			name:         "没有 custom 字段按 containerd 处理",
			cfg:          clusterconfig.RuntimeSpec{},
			wantName:     ComponentContainerd,
			wantSocket:   DefaultCRISocket,
			wantService:  DefaultServiceName,
			wantContains: []string{"containerd", "containerd-shim-runc-v2", "runc", "ctr"},
		},
		{
			name: "containerd + 默认配置",
			cfg: clusterconfig.RuntimeSpec{
				Containerd: clusterconfig.ContainerdSpec{Root: "/var/lib/containerd", SystemdCgroup: clusterconfig.BoolPtr(true)},
			},
			wantName:     ComponentContainerd,
			wantSocket:   DefaultCRISocket,
			wantService:  DefaultServiceName,
			wantContains: []string{"containerd", "containerd-shim-runc-v2", "runc", "ctr"},
		},
		{
			name: "containerd + 自定义 root 与仓库镜像",
			cfg: clusterconfig.RuntimeSpec{
				Containerd: clusterconfig.ContainerdSpec{
					Root:          "/data/containerd",
					SystemdCgroup: clusterconfig.BoolPtr(false),
					Registries: clusterconfig.RegistriesSpec{
						Mirrors: map[string][]string{"registry.k8s.io": {"https://mirror.example.com"}},
					},
				},
			},
			wantName:     ComponentContainerd,
			wantSocket:   DefaultCRISocket,
			wantService:  DefaultServiceName,
			wantContains: []string{"containerd", "ctr"},
		},
		{
			name: "custom + systemd cgroup",
			cfg: clusterconfig.RuntimeSpec{
				Custom: clusterconfig.CustomRuntimeSpec{
					CRISocket:    "unix:///run/crio/crio.sock",
					CgroupDriver: "systemd",
					ServiceName:  "crio",
				},
			},
			wantName:     clusterconfig.RuntimeCustom,
			wantSocket:   "unix:///run/crio/crio.sock",
			wantService:  "crio",
			wantExcludes: []string{"ctr", "containerd", "containerd-shim-runc-v2", "runc"},
		},
		{
			name: "custom + cgroupfs",
			cfg: clusterconfig.RuntimeSpec{
				Custom: clusterconfig.CustomRuntimeSpec{
					CRISocket:    "unix:///run/containerd/containerd.sock",
					CgroupDriver: "cgroupfs",
					ServiceName:  "containerd",
				},
			},
			wantName:     clusterconfig.RuntimeCustom,
			wantSocket:   "unix:///run/containerd/containerd.sock",
			wantService:  "containerd",
			wantExcludes: []string{"ctr"},
		},
		{
			name:    "custom 缺少 criSocket",
			cfg:     clusterconfig.RuntimeSpec{Custom: clusterconfig.CustomRuntimeSpec{ServiceName: "crio"}},
			wantErr: true,
		},
		{
			name:    "custom 缺少 serviceName",
			cfg:     clusterconfig.RuntimeSpec{Custom: clusterconfig.CustomRuntimeSpec{CRISocket: "unix:///run/crio/crio.sock"}},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov, err := NewProvider(tc.cfg, Options{InstallDir: t.TempDir()})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %T", prov)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			if got := prov.Name(); got != tc.wantName {
				t.Errorf("Name() = %q，期望 %q", got, tc.wantName)
			}
			if got := CRISocket(tc.cfg); got != tc.wantSocket {
				t.Errorf("CRISocket() = %q，期望 %q", got, tc.wantSocket)
			}
			if got := ServiceName(tc.cfg); got != tc.wantService {
				t.Errorf("ServiceName() = %q，期望 %q", got, tc.wantService)
			}
			bins := Binaries(tc.cfg, bom.MustLoad())
			names := binaryNames(bins)
			for _, want := range tc.wantContains {
				if !containsString(names, want) {
					t.Errorf("Binaries() = %v，应包含 %q", names, want)
				}
			}
			for _, exclude := range tc.wantExcludes {
				if containsString(names, exclude) {
					t.Errorf("Binaries() = %v，不应包含 %q", names, exclude)
				}
			}
			// 版本必须来自 BOM，否则 Check 的缺失判据失效。
			for _, b := range bins {
				if b.Name == "containerd" && b.Version != cdVersion {
					t.Errorf("containerd 版本 = %q，期望 %q", b.Version, cdVersion)
				}
				if b.Component == "" || b.Category == "" {
					t.Errorf("%s 缺少 component/category: %+v", b.Name, b)
				}
			}
		})
	}
}

// TestNewProviderDefaultsPlugins 默认构造会补齐 Report/Commands/Services，避免 nil panic。
func TestNewProviderDefaultsPlugins(t *testing.T) {
	prov, err := NewProvider(containerdSpec(), Options{})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cp := prov.(*containerdProvider)
	if cp.opts.installDir() == "" || cp.opts.commands() == nil || cp.opts.services() == nil || cp.opts.reporter() == nil {
		t.Fatalf("默认值未补齐: %+v", cp.opts)
	}
	if cp.opts.installDir() != "/usr/local/bin" {
		t.Errorf("默认安装目录 = %q，期望 /usr/local/bin", cp.opts.installDir())
	}
}

// TestProviderImplementsInterface 编译期确认两个 provider 都实现了 Provider。
func TestProviderImplementsInterface(t *testing.T) {
	var _ Provider = (*containerdProvider)(nil)
	var _ Provider = (*customProvider)(nil)
}

func containsString(list []string, want string) bool {
	return slices.Contains(list, want)
}

// TestBinariesMissingBOMVersion 没有 BOM 时版本为空，Check 会因此判定未就绪。
func TestBinariesMissingBOMVersion(t *testing.T) {
	bins := Binaries(containerdSpec(), nil)
	for _, b := range bins {
		if b.Version != "" {
			t.Errorf("%s 的版本应为空，实际 %q", b.Name, b.Version)
		}
	}
	_ = fmt.Sprintf("%v", bins)
}
