package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// executeRoot 用给定参数运行 kubemono 命令树，并返回写入其输出流的全部内容。
func executeRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestCommandTreeHasShort(t *testing.T) {
	root := NewRootCmd()
	count := 0
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, c := range cmd.Commands() {
			count++
			if strings.TrimSpace(c.Short) == "" {
				t.Errorf("command %q has an empty Short", c.CommandPath())
			}
			walk(c)
		}
	}
	walk(root)
	if count < 18 {
		t.Fatalf("command tree looks truncated: only %d commands", count)
	}
}

func TestRootHelpRenders(t *testing.T) {
	out, err := executeRoot(t, "--help")
	if err != nil {
		t.Fatalf("kubemono --help returned error: %v", err)
	}
	for _, want := range []string{"init", "join", "reset", "bundle", "cert"} {
		if !strings.Contains(out, want) {
			t.Errorf("kubemono --help does not mention %q", want)
		}
	}
	for _, gone := range []string{"\n  plan ", "\n  network "} {
		if strings.Contains(out, gone) {
			t.Errorf("kubemono --help still lists a removed command %q", strings.TrimSpace(gone))
		}
	}
}

func TestGroupWithoutSubcommandFails(t *testing.T) {
	if _, err := executeRoot(t, "node"); err == nil {
		t.Fatal("kubemono node returned nil, want a 'subcommand required' error")
	}
}

const validClusterYAML = `cluster:
  hosts:
    - {name: m1, address: 10.0.0.1, roles: [master]}
`

func TestConfigValidate(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(valid, []byte(validClusterYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := executeRoot(t, "config", "validate", "-f", valid); err != nil {
		t.Fatalf("valid cluster.yaml rejected: %v\n%s", err, out)
	}

	invalid := filepath.Join(dir, "unknown-field.yaml")
	if err := os.WriteFile(invalid, []byte(validClusterYAML+"  bogusField: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := executeRoot(t, "config", "validate", "-f", invalid); err == nil {
		t.Fatalf("unknown field accepted, want an error\n%s", out)
	}
}

func TestDataDirRejectsFilesystemRoot(t *testing.T) {
	if _, err := executeRoot(t, "version", "--data-dir", "/"); err == nil {
		t.Fatal("--data-dir / 必须被拒绝")
	}
}
