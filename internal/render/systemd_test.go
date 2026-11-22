package render

import (
	"strings"
	"testing"
)

// TestSystemdUnitRendersExecStartAsContinuationLines 锁定 unit 的可读性：
// 参数一多时 ExecStart 按 systemd 反斜杠续行展开，一行一个参数。
func TestSystemdUnitRendersExecStartAsContinuationLines(t *testing.T) {
	unit := SystemdUnit{
		Description: "kubemono managed test",
		ExecStart:   "/usr/local/bin/kube-apiserver --a=1 --b=2",
	}
	got := unit.Render()
	want := "ExecStart=/usr/local/bin/kube-apiserver \\\n" +
		"    --a=1 \\\n" +
		"    --b=2\n"
	if !strings.Contains(got, want) {
		t.Fatalf("ExecStart 未按续行渲染:\n%s", got)
	}
	if strings.Contains(got, "--b=2 \\") {
		t.Fatalf("最后一个参数不应带续行反斜杠:\n%s", got)
	}
}

// TestSystemdUnitSingleArgExecStartStaysOneLine 确认无参数组件（containerd）
// 不会被改写成带反斜杠的空续行。
func TestSystemdUnitSingleArgExecStartStaysOneLine(t *testing.T) {
	unit := SystemdUnit{ExecStart: "/usr/local/bin/containerd"}
	got := unit.Render()
	if !strings.Contains(got, "ExecStart=/usr/local/bin/containerd\n") {
		t.Fatalf("单参数 ExecStart 应保持一行:\n%s", got)
	}
	if strings.Contains(got, "ExecStart=/usr/local/bin/containerd \\") {
		t.Fatalf("单参数 ExecStart 不应有续行:\n%s", got)
	}
}

// TestRenderExecStartEmpty 防御空值：仍输出一条合法的 ExecStart 指令。
func TestRenderExecStartEmpty(t *testing.T) {
	if got := renderExecStart(""); got != "ExecStart=\n" {
		t.Fatalf("空 ExecStart 渲染为 %q", got)
	}
}
