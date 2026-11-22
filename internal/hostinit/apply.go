package hostinit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ApplyOptions 控制 Apply 的错误策略。
type ApplyOptions struct {
	// SkipKernelErrors 在 modprobe/sysctl 失败时只警告并继续。
	// 持久化文件（modules-load.d/sysctl.d）仍会写入，重启后会再次尝试。
	SkipKernelErrors bool
	// Warnf 接收被跳过的内核错误。
	Warnf func(format string, args ...any)
}

func (o ApplyOptions) warn(format string, args ...any) {
	if o.Warnf != nil {
		o.Warnf(format, args...)
	}
}

// ApplyWithOptions 按顺序执行计划，并按 opts 处理内核错误。
func ApplyWithOptions(ctx context.Context, steps []Step, ap Applier, journal *Journal, opts ApplyOptions) error {
	if ap == nil {
		return errors.New("hostinit: Applier 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	j := journal
	if j == nil {
		j = NewMemory()
	}
	for _, st := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := applyStep(ctx, st, ap, j, opts); err != nil {
			return fmt.Errorf("hostinit: 应用步骤 %s（%s）失败: %w", st.ID, st.Title, err)
		}
	}
	return nil
}

// Reset 依据 journal 逆序还原主机变更。
// 无法自动还原的记录会被跳过，但一定会出现在返回的错误里（绝不静默丢弃）；
// 单条还原失败也会被收集进同一个错误，其余记录继续还原。
func Reset(ctx context.Context, journal *Journal, ap Applier) error {
	if journal == nil {
		return errors.New("hostinit: Journal 不能为 nil")
	}
	if ap == nil {
		return errors.New("hostinit: Applier 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var failures []string
	var skipped []string
	for _, entry := range slices.Backward(journal.Entries) {
		if !entry.Revertible {
			skipped = append(skipped, entry.Kind+" "+entry.Target)
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := revertEntry(ctx, entry, ap); err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: %v", entry.Kind, entry.Target, err))
		}
	}
	var parts []string
	if len(failures) > 0 {
		parts = append(parts, "以下变更还原失败："+strings.Join(failures, "；"))
	}
	if len(skipped) > 0 {
		parts = append(parts, fmt.Sprintf("以下 %d 项变更不可自动还原，需要人工处理：%s",
			len(skipped), strings.Join(skipped, "、")))
	}
	if len(parts) > 0 {
		return errors.New("hostinit: 主机初始化还原不完整：" + strings.Join(parts, "；"))
	}
	return nil
}

// applyStep 派发单个步骤：先记录，后动作。
func applyStep(ctx context.Context, st Step, ap Applier, j *Journal, opts ApplyOptions) error {
	switch p := st.Payload.(type) {
	case SysctlPayload:
		return applySysctl(ctx, st, p, ap, j, opts)
	case ModulePayload:
		return applyModule(ctx, st, p, ap, j, opts)
	case FilePayload:
		return applyFile(ctx, st, p, ap, j)
	case DirPayload:
		return applyDir(ctx, st, p, ap, j)
	case HookPayload:
		return applyHook(ctx, st, p, ap, j)
	default:
		return fmt.Errorf("未知的步骤 payload 类型 %T（kind=%q）", st.Payload, st.Kind)
	}
}

func applySysctl(ctx context.Context, st Step, p SysctlPayload, ap Applier, j *Journal, opts ApplyOptions) error {
	key, value := p.Key, p.Value
	before, known := readSysctlValue(ctx, ap, key)
	if err := j.Record(Entry{
		StepID: st.ID, Kind: KindSysctl, Target: key,
		Before: before, After: value, Revertible: known,
	}); err != nil {
		return err
	}
	if err := ap.Sysctl(ctx, key, value); err != nil {
		if !opts.SkipKernelErrors {
			return err
		}
		opts.warn("设置内核参数 %s=%s 失败，已跳过: %v", key, value, err)
	}
	return nil
}

func applyModule(ctx context.Context, st Step, p ModulePayload, ap Applier, j *Journal, opts ApplyOptions) error {
	name := p.Name
	if err := j.Record(Entry{
		StepID: st.ID, Kind: KindModule, Target: name,
		Before: EntryAbsent, After: "loaded", Revertible: false,
	}); err != nil {
		return err
	}
	if err := ap.LoadModule(ctx, name); err != nil {
		if !opts.SkipKernelErrors {
			return err
		}
		opts.warn("加载内核模块 %s 失败，已跳过: %v", name, err)
	}
	return nil
}

func applyFile(ctx context.Context, st Step, p FilePayload, ap Applier, j *Journal) error {
	path := p.Path
	mode := fileModeOr(p.Mode, defaultFileMode)
	content, before, revertible, err := renderFile(ctx, p, ap)
	if err != nil {
		return err
	}
	if err := j.Record(Entry{
		StepID: st.ID, Kind: st.Kind, Target: path,
		Before: before, After: content, Revertible: revertible,
	}); err != nil {
		return err
	}
	return ap.WriteFile(ctx, path, []byte(content), mode)
}

func applyDir(ctx context.Context, st Step, p DirPayload, ap Applier, j *Journal) error {
	path := p.Path
	mode := fileModeOr(p.Mode, 0o755)
	maker, ok := ap.(DirMaker)
	if !ok {
		return fmt.Errorf("%w: dir 步骤需要 Applier 实现 DirMaker", ErrUnsupported)
	}
	if err := j.Record(Entry{
		StepID: st.ID, Kind: KindDir, Target: path,
		Before: EntryAbsent, After: "created", Revertible: false,
	}); err != nil {
		return err
	}
	return maker.MakeDir(ctx, path, mode)
}

func applyHook(ctx context.Context, st Step, p HookPayload, ap Applier, j *Journal) error {
	command := p.Command
	if err := j.Record(Entry{
		StepID: st.ID, Kind: KindHook, Target: command,
		Before: EntryAbsent, After: "executed", Revertible: false,
	}); err != nil {
		return err
	}
	return ap.RunCommand(ctx, "sh", "-c", command)
}

// revertEntry 依据一条记录还原单个变更。
func revertEntry(ctx context.Context, entry Entry, ap Applier) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch entry.Kind {
	case KindFile:
		if entry.Before == EntryAbsent {
			return ap.RemoveFile(ctx, entry.Target)
		}
		return ap.WriteFile(ctx, entry.Target, []byte(entry.Before), defaultFileMode)
	case KindSysctl:
		if entry.Before == EntryAbsent {
			return errors.New("记录中没有变更前的 sysctl 值")
		}
		return ap.Sysctl(ctx, entry.Target, entry.Before)
	case KindModule, KindDir, KindHook:
		return fmt.Errorf("%s 变更不支持自动还原", entry.Kind)
	default:
		return fmt.Errorf("未知的变更类型 %q", entry.Kind)
	}
}
