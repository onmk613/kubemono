package network_test

import (
	"testing"

	"kubemono/internal/network"
)

func TestNodeCIDRDerivesFirstSubnet(t *testing.T) {
	got, err := network.NodeCIDR("10.244.0.0/16", 24, "")
	if err != nil {
		t.Fatalf("NodeCIDR: %v", err)
	}
	if want := "10.244.1.0/24"; got != want {
		t.Fatalf("network.NodeCIDR() = %s, want %s", got, want)
	}
}

func TestNodeCIDRRejectsAddressOverflow(t *testing.T) {
	// base 本身已到 IPv4 上界时，base+offset 会回绕成 0.0.0.0/32；
	// 必须在推导阶段报错，而不是静默返回一个错误网段。
	if got, err := network.NodeCIDR("255.255.255.255/32", 32, ""); err == nil {
		t.Fatalf("network.NodeCIDR() = %s, want overflow error", got)
	}
}

func TestNodeCIDRPrefersAssigned(t *testing.T) {
	got, err := network.NodeCIDR("10.244.0.0/16", 24, "10.244.9.0/24")
	if err != nil {
		t.Fatalf("NodeCIDR: %v", err)
	}
	if got != "10.244.9.0/24" {
		t.Fatalf("network.NodeCIDR() = %s, want assigned value", got)
	}
}
