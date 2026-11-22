package controlplane

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"kubemono/internal/constants"
)

// fakeSystemd 记录 systemctl 调用。
type fakeSystemd struct {
	calls    []string
	failOn   string
	reloaded int
}

func (f *fakeSystemd) DaemonReload(context.Context) error {
	f.reloaded++
	f.calls = append(f.calls, "daemon-reload")
	return nil
}

func (f *fakeSystemd) Action(_ context.Context, unit, action string) error {
	f.calls = append(f.calls, action+" "+unit)
	if f.failOn == action+" "+unit {
		return errors.New("systemd 拒绝")
	}
	return nil
}

func (f *fakeSystemd) IsActive(context.Context, string) (bool, error) { return true, nil }

// fakeHealth 是可控的健康检查器。
type fakeHealth struct{ err error }

// Check 实现 HealthChecker。
func (f *fakeHealth) Check(context.Context, HealthCheck) error { return f.err }

func unitFor(component Component) []byte {
	return []byte("[Unit]\nDescription=kubemono managed " + string(component) + "\n\n[Service]\nExecStart=/usr/local/bin/" + string(component) + "\nRestart=always\n")
}

func binaryRunner(t *testing.T, sys *fakeSystemd, health HealthChecker) (*BinaryRunner, constants.Paths) {
	t.Helper()
	paths := constants.NewPaths(t.TempDir())
	opts := Options{
		Paths:        paths,
		Health:       health,
		Timeout:      40 * time.Millisecond,
		PollInterval: time.Millisecond,
		Sleep:        func(time.Duration) {},
	}
	return NewBinaryRunner(opts, sys), paths
}

func TestBinaryApplyWritesUnitAndStartsService(t *testing.T) {
	sys := &fakeSystemd{}
	r, paths := binaryRunner(t, sys, &fakeHealth{})
	spec := &RenderedSpec{Component: ComponentAPIServer, Unit: unitFor(ComponentAPIServer)}
	if err := r.Apply(context.Background(), spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	target := r.UnitPath(ComponentAPIServer)
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("unit 未写入: %v", err)
	}
	if !strings.Contains(string(data), "ExecStart=/usr/local/bin/kube-apiserver") {
		t.Fatalf("unit 内容不正确:\n%s", data)
	}
	// 顺序必须是 daemon-reload → enable → restart。
	want := []string{"daemon-reload", "enable kube-apiserver.service", "restart kube-apiserver.service"}
	if strings.Join(sys.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("systemctl 调用序列 = %v, want %v", sys.calls, want)
	}
	if sys.reloaded != 1 {
		t.Fatalf("daemon-reload 次数 = %d", sys.reloaded)
	}
	_ = paths
}

func TestBinaryApplyRequiresUnit(t *testing.T) {
	r, _ := binaryRunner(t, &fakeSystemd{}, &fakeHealth{})
	if err := r.Apply(context.Background(), nil); err == nil {
		t.Fatal("空渲染结果必须拒绝")
	}
	if err := r.Apply(context.Background(), &RenderedSpec{Component: ComponentEtcd}); err == nil {
		t.Fatal("没有 unit 内容必须拒绝（binary 模式不能退化成静态 Pod）")
	}
}

func TestBinaryApplyRollsBackUnitOnUnhealthy(t *testing.T) {
	sys := &fakeSystemd{}
	r, _ := binaryRunner(t, sys, &fakeHealth{err: errors.New("一直起不来")})
	target := r.UnitPath(ComponentAPIServer)

	// 旧 unit 先在位（模拟版本变更后的重启）。
	if err := os.MkdirAll(r.UnitPath(ComponentAPIServer)[:strings.LastIndex(target, "/")], 0o755); err != nil {
		t.Fatal(err)
	}
	old := []byte("[Service]\nExecStart=/usr/local/bin/kube-apiserver --old\n")
	if err := os.WriteFile(target, old, 0o644); err != nil {
		t.Fatal(err)
	}
	err := r.Apply(context.Background(), &RenderedSpec{Component: ComponentAPIServer, Unit: unitFor(ComponentAPIServer)})
	if err == nil {
		t.Fatal("健康检查失败必须返回错误")
	}
	if !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("错误信息应说明已回滚: %v", err)
	}
	restored, _ := os.ReadFile(target)
	if string(restored) != string(old) {
		t.Fatalf("unit 未回滚:\n%s", restored)
	}
	// 回滚后必须重新 daemon-reload 并重启回上一份可用产物。
	joined := strings.Join(sys.calls, "|")
	if !strings.Contains(joined, "restart kube-apiserver.service") {
		t.Fatalf("回滚后应重启: %v", sys.calls)
	}
}

func TestBinaryApplyStartFailureRollsBack(t *testing.T) {
	sys := &fakeSystemd{failOn: "restart kube-scheduler.service"}
	r, _ := binaryRunner(t, sys, &fakeHealth{})
	err := r.Apply(context.Background(), &RenderedSpec{Component: ComponentScheduler, Unit: unitFor(ComponentScheduler)})
	if err == nil {
		t.Fatal("启动失败必须报错")
	}
	// 新 unit（原本不存在）必须被清理，避免留一个坏 unit 在系统里。
	if _, statErr := os.Stat(r.UnitPath(ComponentScheduler)); !os.IsNotExist(statErr) {
		t.Fatal("新建的 unit 在启动失败后应被清理")
	}
}

func TestBinaryRestartStopRemove(t *testing.T) {
	sys := &fakeSystemd{}
	r, _ := binaryRunner(t, sys, &fakeHealth{})
	ctx := context.Background()
	if err := r.Restart(ctx, ComponentControllerManager); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := r.Stop(ctx, ComponentControllerManager); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Remove 需要 unit 存在才能删除。
	if _, err := os.Stat(r.UnitPath(ComponentControllerManager)); err == nil {
		if err := r.Remove(ctx, ComponentControllerManager); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}
	calls := strings.Join(sys.calls, "|")
	for _, want := range []string{"restart kube-controller-manager.service", "stop kube-controller-manager.service"} {
		if !strings.Contains(calls, want) {
			t.Errorf("缺少调用 %q: %v", want, sys.calls)
		}
	}
}

func TestNewRunnerRequiresServiceManager(t *testing.T) {
	paths := constants.NewPaths(t.TempDir())
	opts := Options{Paths: paths, Health: &fakeHealth{}}
	if _, err := NewRunner(opts, nil); err == nil {
		t.Fatal("缺少 systemd 必须报错")
	}
	r, err := NewRunner(opts, &fakeSystemd{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if r == nil {
		t.Fatal("NewRunner returned nil")
	}
}
