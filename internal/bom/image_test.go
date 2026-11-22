package bom

import (
	"strings"
	"testing"
)

// TestEmbeddedBOMImageReferences 复核内置 BOM 里每个镜像都写成完整引用。
func TestEmbeddedBOMImageReferences(t *testing.T) {
	b := MustLoad()
	cases := map[string]string{
		"pause":       "registry.k8s.io/pause:3.10.2",
		"kube-proxy":  "registry.k8s.io/kube-proxy:" + b.Components["kube-proxy"].Version,
		"coredns":     "registry.k8s.io/coredns/coredns:" + b.Components["coredns"].Version,
		"kube-router": "docker.io/cloudnativelabs/kube-router:" + b.Components["kube-router"].Version,
		"haproxy":     "docker.io/library/haproxy:" + b.Components["haproxy"].Version,
	}
	got := map[string]string{}
	for _, img := range b.Images() {
		if _, want := cases[img.Component]; want {
			got[img.Component] = img.Reference()
		}
	}
	for comp, want := range cases {
		if got[comp] != want {
			t.Errorf("组件 %s 的镜像引用 = %q，期望 %q", comp, got[comp], want)
		}
	}
}

// TestWithImageOverrides 确认按组件覆盖只改目标镜像，且拒绝没有镜像的组件。
func TestWithImageOverrides(t *testing.T) {
	b := MustLoad()
	effective, err := b.WithImageOverrides(map[string]string{
		"coredns": "harbor.example.com/kubemono/coredns/coredns:v1.14.6",
		"pause":   "harbor.example.com/kubemono/pause:3.10.2",
	})
	if err != nil {
		t.Fatalf("WithImageOverrides: %v", err)
	}
	if got, _ := effective.Component("coredns"); got.Image != "harbor.example.com/kubemono/coredns/coredns:v1.14.6" {
		t.Fatalf("coredns image = %q", got.Image)
	}
	if got, err := effective.Image("coredns"); err != nil || got.Reference() != "harbor.example.com/kubemono/coredns/coredns:v1.14.6" {
		t.Fatalf("coredns ref = %q, err=%v", got.Reference(), err)
	}
	// 原 BOM 不被修改。
	if orig, _ := b.Image("coredns"); strings.Contains(orig.Reference(), "harbor.example.com") {
		t.Fatalf("原 BOM 被修改: %q", orig.Reference())
	}
	if _, err := b.WithImageOverrides(map[string]string{"etcd": "harbor.example.com/etcd:v3.7.2"}); err == nil {
		t.Fatal("没有镜像的组件应拒绝覆盖")
	}
	if _, err := b.WithImageOverrides(map[string]string{"not-a-component": "harbor.example.com/x:1"}); err == nil {
		t.Fatal("未知组件应拒绝覆盖")
	}
	if _, err := b.WithImageOverrides(map[string]string{"coredns": "not-a-full-ref"}); err == nil {
		t.Fatal("非完整镜像引用应拒绝")
	}
}
