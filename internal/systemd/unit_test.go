package systemd

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingRunner struct {
	calls [][]string
	out   map[string]string
	err   map[string]error
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	full := append([]string{name}, args...)
	r.calls = append(r.calls, full)
	key := strings.Join(full, " ")
	return []byte(r.out[key]), r.err[key]
}

func (r *recordingRunner) called(want string) bool {
	for _, c := range r.calls {
		if strings.Join(c, " ") == want {
			return true
		}
	}
	return false
}

func TestUnitControllerEnableRestartStop(t *testing.T) {
	ctx := context.Background()
	run := &recordingRunner{
		out: map[string]string{"systemctl is-active kubelet.service": "active\n"},
		err: map[string]error{},
	}
	u := UnitController{Runner: run}
	if err := u.Enable(ctx, "kubelet.service"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !run.called("systemctl daemon-reload") || !run.called("systemctl enable kubelet.service") {
		t.Fatalf("Enable 应先 daemon-reload 再 enable: %v", run.calls)
	}
	if err := u.Restart(ctx, "kubelet.service"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !run.called("systemctl restart kubelet.service") {
		t.Fatalf("缺少 restart: %v", run.calls)
	}
	if err := u.Stop(ctx, "kubelet.service"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !run.called("systemctl stop kubelet.service") {
		t.Fatalf("缺少 stop: %v", run.calls)
	}
}

func TestUnitControllerDryRunDoesNotExecute(t *testing.T) {
	run := &recordingRunner{out: map[string]string{}, err: map[string]error{}}
	u := UnitController{Runner: run, DryRun: true}
	ctx := context.Background()
	for _, fn := range []func() error{
		func() error { return u.Enable(ctx, "x.service") },
		func() error { return u.Restart(ctx, "x.service") },
		func() error { return u.Stop(ctx, "x.service") },
		func() error { return u.Disable(ctx, "x.service") },
	} {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}
	if len(run.calls) != 0 {
		t.Fatalf("dry-run 不应执行命令: %v", run.calls)
	}
}

func TestUnitControllerErrorMentionsNextStep(t *testing.T) {
	run := &recordingRunner{
		out: map[string]string{},
		err: map[string]error{"systemctl restart kubelet.service": errors.New("unit not found")},
	}
	err := (UnitController{Runner: run}).Restart(context.Background(), "kubelet.service")
	if err == nil {
		t.Fatal("应当报错")
	}
	for _, want := range []string{"kubelet.service", "systemctl status", "journalctl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 %q 缺少操作指引 %q", err.Error(), want)
		}
	}
	if err := (UnitController{}).Restart(context.Background(), "x"); err == nil {
		t.Fatal("没有命令执行器时必须报错")
	}
}

func TestShowParsesProperties(t *testing.T) {
	key := "systemctl show kubemono-cert-renew.service --no-page -p Result -p ExecMainStatus"
	r := &recordingRunner{out: map[string]string{key: "Result=failed\nExecMainStatus=1\n"}}
	got, err := Show(context.Background(), r, "kubemono-cert-renew.service", "Result", "ExecMainStatus")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got["Result"] != "failed" || got["ExecMainStatus"] != "1" {
		t.Fatalf("Show = %+v", got)
	}
}
