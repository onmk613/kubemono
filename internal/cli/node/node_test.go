package node

import (
	"strings"
	"testing"

	"kubemono/internal/cli/options"
)

func TestNodeRequiresRepairSubcommand(t *testing.T) {
	cmd := New(options.Default())
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil {
		t.Fatal("kubemono node 不带子命令应失败")
	}
	if !strings.Contains(out.String(), "repair") {
		t.Fatalf("提示里应指向 node repair:\n%s", out.String())
	}
	var repair bool
	for _, c := range cmd.Commands() {
		if c.Name() == "repair" {
			repair = true
		}
	}
	if !repair {
		t.Fatal("缺少 repair 子命令")
	}
}
