package bom

import (
	"slices"
	"testing"
)

// TestEmbeddedBOMBundlesAllCNIPlugins 锁定默认打包的 CNI 插件集合。
// kubemono 自己的 kube-router 链路只用 bridge/host-local/loopback/portmap，但
// plugin=none 与自定义网络需要上游归档里的全部插件，所以默认全量入包。
// 需要裁剪时改 bom.yaml 的 binaries，并同步改这个用例。
func TestEmbeddedBOMBundlesAllCNIPlugins(t *testing.T) {
	want := []string{
		"bandwidth", "bridge", "dhcp", "dummy", "firewall", "host-device",
		"host-local", "ipvlan", "loopback", "macvlan", "portmap", "ptp",
		"sbr", "static", "tap", "tuning", "vlan", "vrf",
	}
	b := MustLoad()
	c, ok := b.Components["cni-plugins"]
	if !ok {
		t.Fatal("BOM 缺少 cni-plugins 组件")
	}
	got := slices.Clone(c.Binaries)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("cni-plugins 默认打包集合 = %v，期望 %v", got, want)
	}
}
