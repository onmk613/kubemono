package config

import (
	"testing"
)

// TestExtraArgsMayOverrideDefaults 确认 extraArgs 可以覆盖“非保留”的默认 flag。
// 保留 flag（--secure-port/--data-dir 等）由 kubemono 管理，见
// TestExtraArgsRejectsReservedFlags。
func TestExtraArgsMayOverrideDefaults(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Cluster)
	}{
		{"apiserver", func(c *Cluster) { c.ControlPlane.APIServer.ExtraArgs = map[string]string{"v": "4"} }},
		{"etcd", func(c *Cluster) { c.ControlPlane.Etcd.Local.ExtraArgs = map[string]string{"snapshot-count": "5000"} }},
		{"kubelet", func(c *Cluster) { c.Node.Kubelet.ExtraArgs = map[string]string{"max-pods": "5"} }},
		{"kube-router", func(c *Cluster) { c.Network.KubeRouter.ExtraArgs = map[string]string{"run-router": "true"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCluster()
			tc.set(c)
			if err := Validate(c).Err(); err != nil {
				t.Fatalf("%s: extraArgs 覆盖必须被接受: %v", tc.name, err)
			}
		})
	}
}

// TestExtraArgsRejectsReservedFlags 确认会破坏集群接线/安全基线的 flag
// 在 validate 阶段就被拒绝，而不是渲染时覆盖后组件起不来。
func TestExtraArgsRejectsReservedFlags(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Cluster)
	}{
		{"apiserver authorization-mode", func(c *Cluster) {
			c.ControlPlane.APIServer.ExtraArgs = map[string]string{"authorization-mode": "AlwaysAllow"}
		}},
		{"etcd data-dir", func(c *Cluster) {
			c.ControlPlane.Etcd.Local.ExtraArgs = map[string]string{"data-dir": "/tmp/etcd"}
		}},
		{"kubelet kubeconfig", func(c *Cluster) {
			c.Node.Kubelet.ExtraArgs = map[string]string{"kubeconfig": "/tmp/x"}
		}},
		{"kube-router enable-cni", func(c *Cluster) {
			c.Network.KubeRouter.ExtraArgs = map[string]string{"enable-cni": "true"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCluster()
			tc.set(c)
			if err := Validate(c).Err(); err == nil {
				t.Fatal("保留 flag 必须被 validate 拒绝")
			}
		})
	}
}

// TestExtraArgsAllowUnmodelledFlags 确认 kubemono 没建模的 flag 仍可使用。
func TestExtraArgsAllowUnmodelledFlags(t *testing.T) {
	c := DefaultCluster()
	c.Node.Kubelet.ExtraArgs = map[string]string{"v": "2"}
	if err := Validate(c).Err(); err != nil {
		t.Fatalf("未建模的 flag 不应被拒绝: %v", err)
	}
}
