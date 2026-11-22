package reset

import (
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/cli/options"
)

func TestNewRegistersDestructiveFlags(t *testing.T) {
	cmd := New(options.Default())
	if cmd.Use != "reset" {
		t.Fatalf("Use = %q", cmd.Use)
	}
	for _, name := range []string{"leave", "keep-data", "keep-hostinit", "purge", "assume-yes", "force-leave", "dry-run", "config", "kubeconfig"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("reset 缺少 flag --%s", name)
		}
	}
	if cmd.Args == nil {
		t.Error("reset 必须拒绝位置参数")
	}
}

func TestPurgeFlagAndConflict(t *testing.T) {
	cmd := New(options.Default())
	if cmd.Flags().Lookup("purge") == nil {
		t.Fatal("reset 缺少 --purge")
	}
	if cmd.Flags().Lookup("assume-yes") == nil {
		t.Fatal("reset 缺少 -y/--assume-yes（只跳过确认）")
	}
	cmd.SetArgs([]string{"--purge", "--keep-data"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--purge 与 --keep-data 必须冲突")
	}
	if !strings.Contains(err.Error(), "不能与") {
		t.Fatalf("错误信息应说明冲突: %v", err)
	}
}

// TestResetRejectsUnreadableExplicitConfig：reset 是破坏性命令，显式 -f 读不到
// 必须直接报错，不能退回默认路径/默认运行时去清理。
func TestResetRejectsUnreadableExplicitConfig(t *testing.T) {
	cmd := New(options.Default())
	cmd.SetArgs([]string{"--dry-run", "-f", filepath.Join(t.TempDir(), "missing.yaml")})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("显式 -f 读不到时必须报错")
	}
	if !strings.Contains(err.Error(), "cluster configuration") {
		t.Fatalf("错误信息应说明配置读取失败，got %v", err)
	}
}
