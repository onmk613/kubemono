package bom

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestParseComponentVersion(t *testing.T) {
	// 版本比较（Compare）已随死代码清理删除：它没有任何生产调用方。
	// 这里只锁住解析行为——解析结果仍决定 BOM 里组件版本的规范化。
	cases := []struct {
		in   string
		want [3]int
	}{
		{"v1.14.6", [3]int{1, 14, 6}},
		{"2.8", [3]int{2, 8, 0}},
		{"3.10.2", [3]int{3, 10, 2}},
		{"v1.7.24", [3]int{1, 7, 24}},
	}
	for _, tc := range cases {
		got, err := ParseComponentVersion(tc.in)
		if err != nil {
			t.Fatalf("ParseComponentVersion(%q): %v", tc.in, err)
		}
		if [3]int{got.Major, got.Minor, got.Patch} != tc.want {
			t.Errorf("ParseComponentVersion(%q) = %d.%d.%d, want %v",
				tc.in, got.Major, got.Minor, got.Patch, tc.want)
		}
	}
	for _, bad := range []string{"", "v", "v1.x", "v1..2", "v1.2.3.4", "latest"} {
		if _, err := ParseComponentVersion(bad); err == nil {
			t.Errorf("ParseComponentVersion(%q) succeeded, want error", bad)
		}
	}
}
func TestWithVersionOverrides(t *testing.T) {
	base := MustLoad()
	effective, warnings, err := base.WithVersionOverrides(map[string]string{
		"coredns":    "v1.14.7",
		"containerd": "v2.4.3",
	})
	if err != nil {
		t.Fatalf("WithVersionOverrides: %v", err)
	}
	if got := effective.ComponentVersion("coredns"); got != "v1.14.7" {
		t.Fatalf("coredns version = %s", got)
	}
	if got := effective.ComponentVersion("containerd"); got != "v2.4.3" {
		t.Fatalf("containerd version = %s", got)
	}
	if !isOverridden(effective, "coredns") || !isOverridden(effective, "containerd") {
		t.Fatal("overrides were not recorded")
	}
	if len(warnings) < 2 {
		t.Fatalf("expected at least two warnings, got %v", warnings)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "非默认版本") {
		t.Fatalf("warnings do not mention non-default version: %v", warnings)
	}
	// base 不能被修改。
	if base.ComponentVersion("coredns") != "v1.14.6" {
		t.Fatalf("base BOM was mutated: %s", base.ComponentVersion("coredns"))
	}
}

func TestWithVersionOverridesRejects(t *testing.T) {
	base := MustLoad()
	cases := map[string]map[string]string{
		"k8s partial group": {"kube-apiserver": "v1.37.2"},
		"kubemono":          {"kubemono": "v1.37.1-km.2"},
		"unknown":           {"not-a-component": "v1.0.0"},
		"empty version":     {"coredns": " "},
	}
	for name, overrides := range cases {
		if _, _, err := base.WithVersionOverrides(overrides); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// k8sGroup 返回把全部 k8s 组件固定到 version 的覆盖集合。
func k8sGroup(t *testing.T, b *BOM, version string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range b.kubernetesComponentNames() {
		out[name] = version
	}
	if len(out) == 0 {
		t.Fatal("BOM 里没有 k8s 组件")
	}
	return out
}

func TestWithVersionOverridesKubernetesPatch(t *testing.T) {
	base := MustLoad()
	cur, err := ParseComponentVersion(base.Kubernetes)
	if err != nil {
		t.Fatalf("parse kubernetes: %v", err)
	}
	next := fmt.Sprintf("v%d.%d.%d", cur.Major, cur.Minor, cur.Patch+1)

	effective, warnings, err := base.WithVersionOverrides(k8sGroup(t, base, next))
	if err != nil {
		t.Fatalf("整组固定 k8s patch: %v", err)
	}
	for _, name := range base.kubernetesComponentNames() {
		if got := effective.ComponentVersion(name); got != next {
			t.Errorf("%s version = %s, want %s", name, got, next)
		}
		if !isOverridden(effective, name) {
			t.Errorf("%s 没有记录为 overridden", name)
		}
	}
	if effective.Kubernetes != next {
		t.Errorf("kubernetes = %s, want %s", effective.Kubernetes, next)
	}
	wantRelease := next + "-km." + strconv.Itoa(mustRelease(t, base.Release).Patch)
	if effective.Release != wantRelease {
		t.Errorf("release = %s, want %s", effective.Release, wantRelease)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "整组固定") {
		t.Fatalf("缺少整组固定的告警: %v", warnings)
	}
	// 基线不能被改写。
	if base.Kubernetes != cur.Raw {
		t.Fatalf("base kubernetes was mutated: %s", base.Kubernetes)
	}
	if base.ComponentVersion("kube-apiserver") != cur.Raw {
		t.Fatalf("base kube-apiserver was mutated: %s", base.ComponentVersion("kube-apiserver"))
	}
	if isOverridden(base, "kube-apiserver") {
		t.Fatal("base BOM 不应记录 override")
	}
}

func TestWithVersionOverridesKubernetesAllowsAnyVersion(t *testing.T) {
	base := MustLoad()
	cur, err := ParseComponentVersion(base.Kubernetes)
	if err != nil {
		t.Fatalf("parse kubernetes: %v", err)
	}
	crossMinor := fmt.Sprintf("v%d.%d.0", cur.Major, cur.Minor+1)
	effective, warnings, err := base.WithVersionOverrides(k8sGroup(t, base, crossMinor))
	if err != nil {
		t.Fatalf("跨小版本应只告警、不拒绝: %v", err)
	}
	if effective.Kubernetes != crossMinor {
		t.Fatalf("kubernetes = %s, want %s", effective.Kubernetes, crossMinor)
	}
	if len(warnings) == 0 {
		t.Fatal("非默认 Kubernetes 版本应告警")
	}

	// 组内不一致仍然拒绝：这不是版本范围限制，而是防止控制面版本漂移。
	group := k8sGroup(t, base, crossMinor)
	group["kubelet"] = base.Kubernetes
	if _, _, err := base.WithVersionOverrides(group); err == nil {
		t.Fatal("组内不一致应拒绝")
	}
}

func TestWithVersionOverridesKubernetesEqualToDefault(t *testing.T) {
	base := MustLoad()
	effective, warnings, err := base.WithVersionOverrides(k8sGroup(t, base, base.Kubernetes))
	if err != nil {
		t.Fatalf("与基线相同的整组固定: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("与基线相同不应告警: %v", warnings)
	}
	if effective.Release != base.Release || effective.Kubernetes != base.Kubernetes {
		t.Fatalf("release/kubernetes 不应被改写: %s / %s", effective.Release, effective.Kubernetes)
	}
	if isOverridden(effective, "kube-apiserver") {
		t.Fatal("与基线相同不应记录为 override")
	}
}

func mustRelease(t *testing.T, s string) Release {
	t.Helper()
	r, err := ParseRelease(s)
	if err != nil {
		t.Fatalf("parse release %q: %v", s, err)
	}
	return r
}

func TestWithVersionOverridesEqualToDefault(t *testing.T) {
	base := MustLoad()
	effective, warnings, err := base.WithVersionOverrides(map[string]string{
		"coredns": base.ComponentVersion("coredns"),
	})
	if err != nil {
		t.Fatalf("equal override: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("equal override should not warn: %v", warnings)
	}
	if isOverridden(effective, "coredns") {
		t.Fatal("equal version should not be treated as an override")
	}
}

func TestWithVersionOverridesAcceptsBelowOldFloor(t *testing.T) {
	base := MustLoad()
	effective, warnings, err := base.WithVersionOverrides(map[string]string{"coredns": "v1.10.0"})
	if err != nil {
		t.Fatalf("任意版本都应允许: %v", err)
	}
	if got := effective.ComponentVersion("coredns"); got != "v1.10.0" {
		t.Fatalf("coredns = %s, want v1.10.0", got)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "非默认版本") {
		t.Fatalf("非默认版本应告警: %v", warnings)
	}
}

// isOverridden 是 BOM.Overrides 的测试侧存在性判断（生产不需要该访问器）。
func isOverridden(b *BOM, name string) bool {
	_, ok := b.Overrides[name]
	return ok
}
