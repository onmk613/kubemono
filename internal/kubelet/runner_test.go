package kubelet

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"kubemono/internal/kubelet/kubeletfake"
)

func TestExecRunnerIsUnsupportedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux 上执行的是真实实现")
	}
	_, err := NewExecRunner().Run(context.Background(), "systemctl", "is-active", UnitName)
	if err == nil {
		t.Fatal("非 linux 平台必须明确报错")
	}
	if !strings.Contains(err.Error(), runtime.GOOS) || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("错误信息应说明平台限制: %v", err)
	}
}

func TestMemoryServiceManagerTracksState(t *testing.T) {
	ctx := context.Background()
	m := kubeletfake.NewMemoryServiceManager()
	if active, err := m.IsActive(ctx, UnitName); err != nil || active {
		t.Fatalf("初始状态应为未运行: %v, %v", active, err)
	}
	if err := m.Enable(ctx, UnitName); err != nil {
		t.Fatal(err)
	}
	if !m.Enabled[UnitName] {
		t.Fatal("Enable 应记录已启用")
	}
	if err := m.Restart(ctx, UnitName); err != nil {
		t.Fatal(err)
	}
	if active, _ := m.IsActive(ctx, UnitName); !active {
		t.Fatal("Restart 之后应为 active")
	}
	if err := m.Stop(ctx, UnitName); err != nil {
		t.Fatal(err)
	}
	if active, _ := m.IsActive(ctx, UnitName); active {
		t.Fatal("Stop 之后应为 inactive")
	}
	m.RestartErr[UnitName] = fmt.Errorf("boom")
	if err := m.Restart(ctx, UnitName); err == nil {
		t.Fatal("注入的错误必须返回")
	}
	if !m.Called("is-active", UnitName) || !m.Called("stop", UnitName) {
		t.Fatalf("调用记录不完整: %v", m.Calls)
	}
}
