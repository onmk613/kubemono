package utils

import (
	"os"
	"path/filepath"
	"testing"

	"kubemono/internal/cli/options"
)

// TestNewClusterClientHonorsKubeconfig 覆盖 reset --leave 的远端退群场景：
// 本机没有 admin.conf 时，--kubeconfig 必须被使用，而不是固定读本机路径。
func TestNewClusterClientHonorsKubeconfig(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "remote.conf")
	const cfg = `apiVersion: v1
kind: Config
clusters:
- name: remote
  cluster:
    server: https://10.0.0.10:6443
contexts:
- name: remote
  context:
    cluster: remote
    user: admin
current-context: remote
users:
- name: admin
  user: {}
`
	if err := os.WriteFile(kubeconfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := options.Default()
	opts.TestRoot = t.TempDir() // 确保本机 admin.conf 不存在
	opts.KubeconfigPath = kubeconfig
	client, err := NewClusterClient(opts)
	if err != nil {
		t.Fatalf("使用 --kubeconfig 时应成功: %v", err)
	}
	if client == nil {
		t.Fatal("NewClusterClient 返回了 nil")
	}

	opts.KubeconfigPath = ""
	if _, err := NewClusterClient(opts); err == nil {
		t.Fatal("没有 --kubeconfig 且没有 admin.conf 时应失败")
	}
}
