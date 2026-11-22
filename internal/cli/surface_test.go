package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestEveryCommandHelpRenders 遍历整棵命令树并运行 `--help`。
// 这覆盖每个命令构造函数（flag 注册、默认值、Short/Long），包括
// reset/join/node 这些没有独立测试包的接线代码；它们最容易在改 flag
// 或默认值时被悄悄破坏。
func TestEveryCommandHelpRenders(t *testing.T) {
	root := NewRootCmd()
	var paths [][]string
	var walk func(cmd *cobra.Command, prefix []string)
	walk = func(cmd *cobra.Command, prefix []string) {
		for _, child := range cmd.Commands() {
			if child.Name() == "help" || child.Name() == "completion" {
				continue
			}
			path := append(append([]string(nil), prefix...), child.Name())
			paths = append(paths, path)
			walk(child, path)
		}
	}
	walk(root, nil)
	if len(paths) < 18 {
		t.Fatalf("命令树过小：只找到 %d 个命令", len(paths))
	}
	for _, path := range paths {
		args := append(append([]string(nil), path...), "--help")
		out, err := executeRoot(t, args...)
		if err != nil {
			t.Errorf("kubemono %s: %v\n%s", strings.Join(args, " "), err, out)
			continue
		}
		if !strings.Contains(out, "Usage:") {
			t.Errorf("kubemono %s 的帮助缺少 Usage:\n%s", strings.Join(args, " "), out)
		}
	}
}

func TestCriticalFlagsAreRegistered(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"reset", []string{"--leave", "--keep-data", "--keep-hostinit", "--purge", "--assume-yes", "--force-leave", "--dry-run"}},
		{"join", []string{"--token", "--control-plane", "--certificate-key", "--discovery-token-ca-cert-hash", "--config", "--node", "--allow-address-mismatch", "--replace-pki", "--dry-run", "--wait"}},
		{"init", []string{"--allow-existing-cluster", "--force-init"}},
		{"token create", []string{"--role", "--ttl", "--config", "--kubeconfig", "--dry-run"}},
		{"bundle pack", []string{"--dry-run", "--images", "--fetch-images"}},
		{"node repair", []string{"--config", "--node", "--dry-run", "--kubeconfig", "--only", "--rebind-identity", "--diff", "--timeout"}},
		{"pki issue", []string{"--ca", "--ca-key", "--profile", "--san", "--organization", "--days", "--out-dir", "--cn"}},
		{"cert renew", []string{"--cert", "--all", "--assume-yes", "--dry-run"}},
	}
	for _, tc := range cases {
		out, err := executeRoot(t, append(strings.Fields(tc.path), "--help")...)
		if err != nil {
			t.Fatalf("kubemono %s --help: %v\n%s", tc.path, err, out)
		}
		for _, flag := range tc.want {
			if !strings.Contains(out, flag) {
				t.Errorf("kubemono %s 的帮助未列出 %s:\n%s", tc.path, flag, out)
			}
		}
	}
}
