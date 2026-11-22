package hostinit

import (
	"context"
	"io/fs"
	"strings"
)

// recorder 记录 Applier 的执行顺序与文件状态。
type recorder struct {
	ops          []string
	files        map[string]string
	modes        map[string]fs.FileMode
	sysctlValues map[string]string
	// failOn 里以某个前缀开头的操作会返回对应错误（用于注入失败）。
	failOn map[string]error
}

func newRecorder() *recorder {
	return &recorder{
		files:        map[string]string{},
		modes:        map[string]fs.FileMode{},
		sysctlValues: map[string]string{},
		failOn:       map[string]error{},
	}
}

func (r *recorder) record(op string) error {
	r.ops = append(r.ops, op)
	for prefix, err := range r.failOn {
		if strings.HasPrefix(op, prefix) {
			return err
		}
	}
	return nil
}

// coreApplier 只实现 Applier 的必需方法，用于验证可选扩展缺席时的行为。
type coreApplier struct{ *recorder }

// NewCoreApplier 返回只实现必需方法的 Applier。
func NewCoreApplier() *coreApplier { return &coreApplier{newRecorder()} }

func (a *coreApplier) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error {
	_ = ctx
	a.files[path] = string(content)
	a.modes[path] = mode
	return a.record("write:" + path)
}

func (a *coreApplier) RemoveFile(ctx context.Context, path string) error {
	_ = ctx
	delete(a.files, path)
	return a.record("remove:" + path)
}

func (a *coreApplier) Sysctl(ctx context.Context, key, value string) error {
	_ = ctx
	a.sysctlValues[key] = value
	return a.record("sysctl:" + key + "=" + value)
}

func (a *coreApplier) LoadModule(ctx context.Context, name string) error {
	return a.record("module:" + name)
}

func (a *coreApplier) RunCommand(ctx context.Context, name string, args ...string) error {
	_ = ctx
	return a.record("cmd:" + strings.TrimSpace(name+" "+strings.Join(args, " ")))
}

// fakeApplier 在 coreApplier 之上实现全部可选扩展。
type fakeApplier struct{ *coreApplier }

// NewFakeApplier 返回带全部可选扩展的 Applier。
func NewFakeApplier() *fakeApplier { return &fakeApplier{NewCoreApplier()} }

func (a *fakeApplier) ReadFile(ctx context.Context, path string) ([]byte, error) {
	_ = ctx
	if data, ok := a.files[path]; ok {
		return []byte(data), nil
	}
	return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

func (a *fakeApplier) MakeDir(ctx context.Context, path string, mode fs.FileMode) error {
	_ = ctx
	a.modes[path] = mode
	return a.record("mkdir:" + path)
}

func (a *fakeApplier) ReadSysctl(ctx context.Context, key string) (string, error) {
	_ = ctx
	if value, ok := a.sysctlValues[key]; ok {
		return value, nil
	}
	return "", fs.ErrNotExist
}
