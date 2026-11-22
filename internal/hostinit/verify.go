package hostinit

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
)

// Verify 检查计划里的步骤是否已经落在主机上。
// 它复用 Apply 的读/算逻辑（renderFile、目录、sysctl 读取器），因此 Check
// 不维护第二套容易漏项的 switch。
// 无法观测的步骤（内核模块、hook）返回“已完成”：它们没有可靠的只读判据，
// 宁可让 phase 跳过，也不要在 Check 里制造假阴性。
func Verify(ctx context.Context, steps []Step, ap Applier, checkContent bool) (bool, error) {
	results, err := VerifySteps(ctx, steps, ap, checkContent)
	if err != nil {
		return false, err
	}
	for _, r := range results {
		if !r.OK {
			return false, nil
		}
	}
	return true, nil
}

// StepVerify 是单个 host-init 步骤的只读校验结果。
type StepVerify struct {
	// Step 是被校验的步骤。
	Step Step
	// OK 表示该步骤已经符合期望。
	OK bool
}

// VerifySteps 返回每个步骤的校验结果，供 --diff 打印具体哪一步不一致。
// 遇到无法继续的读取错误时立即返回错误；不可观测步骤视为 OK（与 Verify 一致）。
func VerifySteps(ctx context.Context, steps []Step, ap Applier, checkContent bool) ([]StepVerify, error) {
	out := make([]StepVerify, 0, len(steps))
	for _, st := range steps {
		ok, err := verifyStep(ctx, st, ap, checkContent)
		if err != nil {
			return nil, err
		}
		out = append(out, StepVerify{Step: st, OK: ok})
	}
	return out, nil
}

func verifyStep(ctx context.Context, st Step, ap Applier, checkContent bool) (bool, error) {
	switch p := st.Payload.(type) {
	case DirPayload:
		return verifyDir(p, checkContent)
	case FilePayload:
		return verifyFile(ctx, p, ap, checkContent)
	case SysctlPayload:
		return verifySysctl(ctx, p, ap)
	default:
		// 无法观测的步骤（内核模块、hook）返回“已完成”：
		// 它们没有可靠的只读判据，宁可让 phase 跳过，也不要在 Check 里制造假阴性。
		return true, nil
	}
}

func verifyDir(p DirPayload, checkContent bool) (bool, error) {
	if !checkContent {
		return true, nil
	}
	info, err := os.Stat(p.Path)
	if err != nil || !info.IsDir() {
		return false, nil
	}
	return true, nil
}

func verifyFile(ctx context.Context, p FilePayload, ap Applier, checkContent bool) (bool, error) {
	if !checkContent {
		return true, nil
	}
	want, _, _, err := renderFile(ctx, p, ap)
	if err != nil {
		return false, err
	}
	reader, ok := ap.(FileReader)
	if !ok {
		return true, nil
	}
	got, err := reader.ReadFile(ctx, p.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return string(got) == want, nil
}

func verifySysctl(ctx context.Context, p SysctlPayload, ap Applier) (bool, error) {
	got, ok := readSysctlValue(ctx, ap, p.Key)
	if !ok {
		return true, nil
	}
	return normalizeSysctlValue(got) == normalizeSysctlValue(p.Value), nil
}

// normalizeSysctlValue 把 "1" 与 "1\n" 之类的写法归一化。
func normalizeSysctlValue(s string) string {
	return strings.TrimSpace(s)
}
