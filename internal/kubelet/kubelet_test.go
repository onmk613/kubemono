package kubelet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
)

// clusterForTest 构造一个已填充默认值的两节点集群。
func clusterForTest(mode string) *clusterconfig.Cluster {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(c)
	return c
}

func renderContextFor(t *testing.T, cluster *clusterconfig.Cluster, node string) *render.Context {
	t.Helper()
	host, ok := cluster.Cluster.Hosts.FindHost(node)
	if !ok {
		t.Fatalf("节点 %s 不在配置里", node)
	}
	return &render.Context{
		Cluster:   cluster,
		Host:      host,
		BOM:       bom.MustLoad(),
		Paths:     constants.NewPaths(t.TempDir()),
		Release:   bom.MustLoad().Release,
		ClusterID: "cid-1",
	}
}

func TestUnitFileRequiresSpecAndPath(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	if _, ok := UnitFile(nil, paths); ok {
		t.Fatal("nil spec 不应报告有 unit")
	}
	spec := &render.Spec{}
	spec.AddFile(paths.Resolve("/etc/systemd/system/etcd.service"), []byte("[Unit]\n"), 0o644, "etcd", render.KindUnit, render.ActionRestart)
	if _, ok := UnitFile(spec, paths); ok {
		t.Fatal("只有 etcd.service 时不应报告 kubelet unit")
	}
	want := UnitPath(paths)
	if want != filepath.Join(paths.Root, "etc/systemd/system/kubelet.service") {
		t.Fatalf("UnitPath 未按 root 解析: %s", want)
	}
}

// TestRenderedKubeletConfigIsParseable 确认本包所断言的渲染流水线仍然产出
// kubelet.config.k8s.io/v1beta1 文件。
func TestRenderedKubeletConfigIsParseable(t *testing.T) {
	rc := renderContextFor(t, clusterForTest("binary"), "m1")
	spec, err := render.Render(rc, render.DefaultBuilders()...)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f, ok := spec.Find(rc.Paths.KubeletConfig())
	if !ok {
		t.Fatal("缺少 kubelet 配置文件")
	}
	var cfg struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Healthz    string `json:"healthzBindAddress"`
		HealthPort int    `json:"healthzPort"`
	}
	if err := yaml.Unmarshal(f.Content, &cfg); err != nil {
		t.Fatalf("kubelet 配置不是合法 YAML: %v", err)
	}
	if cfg.APIVersion != "kubelet.config.k8s.io/v1beta1" || cfg.Kind != "KubeletConfiguration" {
		t.Fatalf("kubelet 配置类型不对: %s/%s", cfg.APIVersion, cfg.Kind)
	}
	if cfg.HealthPort != constants.KubeletHealthzPort {
		t.Fatalf("kubelet healthz 端口 = %d, want %d", cfg.HealthPort, constants.KubeletHealthzPort)
	}
}

func TestInstallFailsClearlyWhenArtifactMissing(t *testing.T) {
	root := t.TempDir()
	paths := constants.NewPaths(root)
	inst := &BinaryInstaller{
		Paths:   paths,
		Release: bom.MustLoad().Release,
	}
	if _, err := os.Stat(paths.ArtifactDir(inst.Release)); err == nil {
		t.Fatal("前置条件：物料目录不应存在")
	}
	_, err := inst.Install(context.Background(), constants.ComponentKubelet)
	if err == nil {
		t.Fatal("物料缺失时必须报错")
	}
	for _, want := range []string{"kubelet", "bundle load", inst.Release} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息 %q 缺少 %q（必须告诉操作员怎么做）", err.Error(), want)
		}
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), filepath.Join("artifacts", inst.Release)) {
		t.Errorf("错误信息应包含期望路径: %v", err)
	}
}
