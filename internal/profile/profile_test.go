package profile_test

import (
	"strings"
	"testing"

	kubeletv1beta1 "k8s.io/kubelet/config/v1beta1"
	"sigs.k8s.io/yaml"

	"kubemono/internal/constants"
	"kubemono/internal/profile"
)

func load(t *testing.T) *profile.Profile {
	t.Helper()
	p, err := profile.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p
}

// TestLoadBase 检查唯一基线的组件集合与关键模板。
func TestLoadBase(t *testing.T) {
	p := load(t)
	for _, name := range []string{
		constants.ComponentAPIServer, constants.ComponentControllerManager,
		constants.ComponentScheduler, constants.ComponentEtcd, constants.ComponentKubelet,
		constants.ComponentKubeRouter, constants.ComponentLocalProxy,
	} {
		if _, err := p.Component(name); err != nil {
			t.Errorf("缺少组件 %s: %v", name, err)
		}
	}
	if _, err := p.Component("not-a-component"); err == nil {
		t.Error("未知组件不应出现在 profile 中")
	}
	for _, name := range []string{
		profile.TmplAPIServerFlags, profile.TmplKubeletConfig,
		profile.TmplKubeRouterPodspec, profile.TmplLocalProxyConfig,
	} {
		if _, err := p.Template(name); err != nil {
			t.Errorf("缺少模板 %s: %v", name, err)
		}
	}
}

// TestRenderFlagsExtraArgsOverride 检查 extraArgs 覆盖同名默认 flag 并追加新 flag。
func TestRenderFlagsExtraArgsOverride(t *testing.T) {
	p := load(t)
	data := &profile.APIServerFlags{
		CommonFlags: profile.CommonFlags{
			Binary: "/usr/local/bin/kube-apiserver",
			ExtraArgs: map[string]string{
				"secure-port":      "7443",
				"audit-log-format": "json",
			},
		},
		APIServerPort:     6443,
		ServiceCIDR:       "10.96.0.0/12",
		AdvertiseAddress:  "10.0.0.1",
		ClientCAFile:      "/etc/kubernetes/pki/ca.crt",
		APIServerCertFile: "/etc/kubernetes/pki/apiserver.crt",
		APIServerKeyFile:  "/etc/kubernetes/pki/apiserver.key",
	}
	first, err := p.RenderFlags(constants.ComponentAPIServer, data)
	if err != nil {
		t.Fatalf("RenderFlags: %v", err)
	}
	second, err := p.RenderFlags(constants.ComponentAPIServer, data)
	if err != nil {
		t.Fatalf("RenderFlags second: %v", err)
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatal("RenderFlags 结果不稳定")
	}
	joined := strings.Join(first, "\n")
	if strings.Contains(joined, "--secure-port=6443") {
		t.Error("默认 secure-port 应被 extraArgs 覆盖")
	}
	if !strings.Contains(joined, "--secure-port=7443") {
		t.Error("extraArgs 的 secure-port 应生效")
	}
	if !strings.Contains(joined, "--audit-log-format=json") {
		t.Error("extraArgs 的新 flag 应被追加")
	}
}

// TestKubeletConfigParses 检查 kubelet 配置模板仍是合法的 v1beta1 结构。
func TestKubeletConfigParses(t *testing.T) {
	p := load(t)
	data := &profile.KubeletFlags{
		ClientCAFile:             "/etc/kubernetes/pki/ca.crt",
		KubeletCgroupDriver:      "systemd",
		ClusterDomain:            "cluster.local",
		KubeletClusterDNS:        []string{"10.96.0.10"},
		ContainerRuntimeEndpoint: "unix:///run/containerd/containerd.sock",
		KubeletResolvConf:        "/etc/resolv.conf",
	}
	rendered, err := p.RenderConfig(constants.ComponentKubelet, data)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	var cfg kubeletv1beta1.KubeletConfiguration
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		t.Fatalf("kubelet 配置不是合法 YAML: %v", err)
	}
	if cfg.StaticPodPath != "/etc/kubernetes/manifests" {
		t.Errorf("staticPodPath = %q", cfg.StaticPodPath)
	}
}
