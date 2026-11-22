package options

import "testing"

func TestDefaultAndPaths(t *testing.T) {
	o := Default()
	if o.Output != "table" || o.Reporter == nil || o.Wait <= 0 {
		t.Fatalf("Default 未填充: %+v", o)
	}
	o.TestRoot = "relative-root"
	if got := o.Paths().Root; got != "relative-root" {
		t.Fatalf("Paths().Root = %q", got)
	}
	o.DataDir = "/srv/kubemono"
	if got, want := o.Paths().Kubemono(), "relative-root/srv/kubemono"; got != want {
		t.Fatalf("Paths().Kubemono() = %q, want %q", got, want)
	}
	if got := o.ReporterOrDiscard(); got == nil {
		t.Fatal("ReporterOrDiscard 返回 nil")
	}
	o.Reporter = nil
	if got := o.ReporterOrDiscard(); got == nil {
		t.Fatal("nil Reporter 时应退回 Discard")
	}
}

func TestLockForWriteIsExclusiveAndDryRunSkips(t *testing.T) {
	opts := Default()
	opts.TestRoot = t.TempDir()
	first, err := opts.LockForWrite()
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := opts.LockForWrite(); err == nil {
		t.Fatal("第二个写锁必须失败")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := opts.LockForWrite()
	if err != nil {
		t.Fatalf("释放后应能重新获取: %v", err)
	}
	_ = third.Close()

	dry := Default()
	dry.TestRoot = t.TempDir()
	dry.DryRun = true
	if lk, err := dry.LockForWrite(); err != nil || lk != nil {
		t.Fatalf("DryRun 不应获取锁: lk=%v err=%v", lk, err)
	}
}
