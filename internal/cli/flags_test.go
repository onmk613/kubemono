package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// sharedFlagNames 是按命令注册的共享标志；本测试固定每个命令接受哪些标志。
var sharedFlagNames = []string{
	"config", "output", "node", "assume-yes", "allow-address-mismatch",
	"dry-run", "wait", "kubeconfig", "data-dir",
}

func findCommand(t *testing.T, root *cobra.Command, path string) *cobra.Command {
	t.Helper()
	cmd := root
	for _, name := range strings.Fields(path) {
		if cmd.Name() == name {
			continue
		}
		var next *cobra.Command
		for _, child := range cmd.Commands() {
			// 隐藏命令也参与检查：它们代码仍然存在（例如 systemd 调用的 daemon run），
			// flag 作用域同样不能错。
			if child.Name() == name {
				next = child
				break
			}
		}
		if next == nil {
			t.Fatalf("command %q not found below %q", path, cmd.CommandPath())
		}
		cmd = next
	}
	return cmd
}

func sharedFlagsOf(cmd *cobra.Command) []string {
	found := make([]string, 0, len(sharedFlagNames))
	for _, name := range sharedFlagNames {
		if cmd.Flags().Lookup(name) != nil {
			found = append(found, name)
		}
	}
	return found
}

func TestRootOnlyHasGlobalFlags(t *testing.T) {
	root := NewRootCmd()
	for _, name := range sharedFlagNames {
		if root.PersistentFlags().Lookup(name) != nil {
			t.Errorf("root still exposes shared flag --%s", name)
		}
	}
	if root.PersistentFlags().Lookup("verbose") == nil {
		t.Error("root must expose --verbose")
	}
	if root.PersistentFlags().Lookup("root-dir") != nil {
		t.Fatal("root must not expose a path-prefix flag")
	}
	if root.PersistentFlags().Lookup("data-dir") != nil {
		t.Fatal("root must not expose --data-dir：只有真正读取数据目录的命令才挂它")
	}

	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, child := range cmd.Commands() {
			if child.Hidden {
				continue
			}
			child.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
				for _, name := range sharedFlagNames {
					if flag.Name == name {
						t.Errorf("%s still inherits shared flag --%s", child.CommandPath(), name)
					}
				}
			})
			walk(child)
		}
	}
	walk(root)
}

func TestSharedFlagMatrix(t *testing.T) {
	want := map[string][]string{
		"version":               {"output"},
		"config default":        {"output"},
		"config validate":       {"config", "output", "data-dir"},
		"init":                  {"config", "node", "allow-address-mismatch", "dry-run", "data-dir"},
		"join":                  {"config", "node", "allow-address-mismatch", "dry-run", "wait", "data-dir"},
		"token create":          {"config", "kubeconfig", "dry-run", "data-dir"},
		"node repair":           {"config", "node", "dry-run", "kubeconfig", "data-dir"},
		"cert renew":            {"config", "output", "dry-run", "data-dir", "assume-yes"},
		"cert check-expiration": {"output", "data-dir"},
		"reset":                 {"config", "dry-run", "kubeconfig", "data-dir", "assume-yes"},
		"bundle list":           {"config", "output", "data-dir"},
		"bundle load":           {"output", "dry-run", "data-dir"},
		"bundle pack":           {"config", "output", "data-dir", "dry-run"},
		"bundle verify":         {"config", "output", "data-dir"},
		"status":                {"config", "node", "allow-address-mismatch", "output", "kubeconfig", "data-dir"},
		"whoami":                {"config", "node", "allow-address-mismatch", "output", "data-dir"},
	}
	root := NewRootCmd()
	for path, expected := range want {
		cmd := findCommand(t, root, path)
		got := sharedFlagsOf(cmd)
		slices.Sort(got)
		slices.Sort(expected)
		if !slices.Equal(got, expected) {
			t.Errorf("%s shared flags = %v, want %v", path, got, expected)
		}
	}
}

func TestRejectsFlagsOutsideCommandScope(t *testing.T) {
	cases := [][]string{
		{"version", "--force"},
		{"version", "--wait", "1m"},
		{"config", "default", "-f", "cluster.yaml"},
		{"reset", "--level", "L2"},
	}
	for _, args := range cases {
		if out, err := executeRoot(t, args...); err == nil {
			t.Errorf("kubemono %s should fail, got:\n%s", strings.Join(args, " "), out)
		}
	}
}

// TestGlobalVerboseIsInherited：--verbose 是 root 的 persistent flag，
// 各命令不用同名本地 flag 遮蔽它，确保 Global Flags 段始终可见。
func TestGlobalVerboseIsInherited(t *testing.T) {
	for _, args := range [][]string{
		{"config", "default", "-v"},
		{"-v", "config", "default"},
		{"config", "default", "--verbose"},
	} {
		if out, err := executeRoot(t, args...); err != nil {
			t.Errorf("kubemono %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	root := NewRootCmd()
	if findCommand(t, root, "config default").InheritedFlags().Lookup("verbose") == nil {
		t.Fatal("config default 应继承 root 的 --verbose")
	}
}

func TestHelpOnlyShowsRelevantFlags(t *testing.T) {
	cases := []struct {
		args    []string
		want    []string
		notWant []string
	}{
		{
			args:    []string{"version", "--help"},
			want:    []string{"--output"},
			notWant: []string{"--config", "--wait", "--kubeconfig", "--config-store"},
		},
		{
			args:    []string{"config", "default", "--help"},
			want:    []string{"--output"},
			notWant: []string{"--force", "--node", "--kubeconfig", "--dry-run"},
		},
		{
			args:    []string{"join", "--help"},
			want:    []string{"--config", "--node", "--allow-address-mismatch", "--dry-run", "--wait", "--token", "--control-plane", "--certificate-key", "--discovery-token-ca-cert-hash", "--replace-pki"},
			notWant: []string{"--output", "--config-store", "--force"},
		},
		{
			args:    []string{"node", "repair", "--help"},
			want:    []string{"--only", "--rebind-identity", "--kubeconfig", "--data-dir"},
			notWant: []string{"--force", "--wait"},
		},
		{
			args:    []string{"cert", "renew", "--help"},
			want:    []string{"--all", "--assume-yes", "--cert"},
			notWant: []string{"--force", "--node"},
		},
	}
	for _, tc := range cases {
		out, err := executeRoot(t, tc.args...)
		if err != nil {
			t.Fatalf("kubemono %s: %v\n%s", strings.Join(tc.args, " "), err, out)
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("kubemono %s help missing %q:\n%s", strings.Join(tc.args, " "), want, out)
			}
		}
		for _, notWant := range tc.notWant {
			if strings.Contains(out, notWant) {
				t.Errorf("kubemono %s help should not contain %q:\n%s", strings.Join(tc.args, " "), notWant, out)
			}
		}
	}
}
