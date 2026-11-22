package constants

import (
	"path/filepath"
	"testing"
)

// TestWithKubemonoDirOnlyMovesKubemonoRoot 固定 --data-dir 的边界：
// 只改 kubemono 自己的状态/配置目录，不动 /etc/kubernetes、/usr/local/bin 等系统路径。
func TestWithKubemonoDirOnlyMovesKubemonoRoot(t *testing.T) {
	p := NewPaths("").WithKubemonoDir("/srv/kubemono")
	if got, want := p.Kubemono(), "/srv/kubemono"; got != want {
		t.Fatalf("Kubemono() = %q, want %q", got, want)
	}
	for _, tc := range []struct {
		name, got, want string
	}{
		{"NodeConfig", p.NodeConfig(), "/srv/kubemono/node.yaml"},
		{"ClusterConfig", p.ClusterConfig(), "/srv/kubemono/cluster.yaml"},
		{"Desired", p.Desired(), "/srv/kubemono/desired"},
		{"Artifacts", p.Artifacts(), "/srv/kubemono/artifacts"},
		{"State", p.State(), "/srv/kubemono/state"},
		{"Backup", p.Backup(), "/srv/kubemono/backup"},
		{"HostInitRecord", p.HostInitRecord(), "/srv/kubemono/hostinit.json"},
		{"Kubernetes", p.Kubernetes(), "/etc/kubernetes"},
		{"LocalBin", p.LocalBin(), "/usr/local/bin"},
		{"CNIBin", p.CNIBin(), "/opt/cni/bin"},
	} {
		if tc.got != filepath.Clean(tc.want) {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestDefaultKubemonoRoot(t *testing.T) {
	if got, want := NewPaths("").Kubemono(), "/usr/local/kubemono"; got != want {
		t.Fatalf("默认 Kubemono() = %q, want %q", got, want)
	}
}

func TestWithKubemonoDirRespectsTestRoot(t *testing.T) {
	p := NewPaths("/tmp/root").WithKubemonoDir("/srv/kubemono")
	if got, want := p.Kubemono(), "/tmp/root/srv/kubemono"; got != want {
		t.Fatalf("Kubemono() = %q, want %q", got, want)
	}
	if got, want := p.Artifacts(), "/tmp/root/srv/kubemono/artifacts"; got != want {
		t.Fatalf("Artifacts() = %q, want %q", got, want)
	}
}
