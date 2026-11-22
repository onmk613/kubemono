package hostinit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type failingKernelApplier struct {
	*fakeApplier
	moduleErr error
	sysctlErr error
}

func (a *failingKernelApplier) LoadModule(context.Context, string) error { return a.moduleErr }
func (a *failingKernelApplier) Sysctl(context.Context, string, string) error {
	return a.sysctlErr
}

func kernelFailureSteps() []Step {
	return []Step{
		{ID: "module-zfs", Title: "load zfs", Kind: KindModule, Payload: ModulePayload{Name: "zfs"}},
		{ID: "sysctl-custom", Title: "set custom", Kind: KindSysctl, Payload: SysctlPayload{Key: "vm.custom", Value: "1"}},
	}
}

func TestApplyKernelErrorsAreFatalByDefault(t *testing.T) {
	ap := &failingKernelApplier{fakeApplier: NewFakeApplier(), moduleErr: errors.New("no module")}
	if err := apply(context.Background(), kernelFailureSteps()[:1], ap, NewMemory()); err == nil {
		t.Fatal("默认情况下 modprobe 失败应中止")
	}
}

func TestApplyCanSkipKernelErrorsWithWarnings(t *testing.T) {
	ap := &failingKernelApplier{
		fakeApplier: NewFakeApplier(),
		moduleErr:   errors.New("no module"),
		sysctlErr:   errors.New("read-only sysctl"),
	}
	var warnings []string
	err := ApplyWithOptions(context.Background(), kernelFailureSteps(), ap, NewMemory(), ApplyOptions{
		SkipKernelErrors: true,
		Warnf: func(format string, args ...any) {
			warnings = append(warnings, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatalf("跳过内核错误后不应失败: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2", warnings)
	}
	if !strings.Contains(warnings[0], "zfs") || !strings.Contains(warnings[1], "vm.custom") {
		t.Fatalf("warning 内容不正确: %v", warnings)
	}
}
