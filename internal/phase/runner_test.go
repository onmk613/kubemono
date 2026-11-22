package phase

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/util/output"
)

// fakePhase 是一个可编排的 phase，用于驱动 runner 的测试。
type fakePhase struct {
	name     string
	applies  bool
	done     bool
	runErr   error
	ranCount int
	checked  int
	checkErr error
	// onRun 在 Run 开始时执行，用于在测试中取消父 context。
	onRun func()
}

func (f *fakePhase) Name() string          { return f.name }
func (f *fakePhase) Applies(*Context) bool { return f.applies }
func (f *fakePhase) Check(context.Context, *Context) (bool, error) {
	f.checked++
	return f.done, f.checkErr
}

func (f *fakePhase) Run(context.Context, *Context) error {
	f.ranCount++
	if f.onRun != nil {
		f.onRun()
	}
	return f.runErr
}

func newTestContext() *Context {
	return &Context{
		Paths:  constants.NewPaths("/tmp/kubemono-test"),
		Report: output.Discard(),
		Now:    func() time.Time { return time.Unix(0, 0) },
	}
}

func newTestRunner(t *testing.T, phases ...Phase) (*Runner, *MemoryStore) {
	t.Helper()
	reg := NewRegistry()
	for _, p := range phases {
		if err := reg.Register(p); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	store := NewMemoryStore()
	r := NewRunner(reg, store)
	return r, store
}

func TestRunnerSkipsNonApplicablePhases(t *testing.T) {
	worker := &fakePhase{name: "cluster-init", applies: false}
	r, _ := newTestRunner(t, worker)
	summary, err := r.Run(context.Background(), newTestContext(), DefaultOptions("install"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if worker.ranCount != 0 {
		t.Fatalf("non-applicable phase ran %d times", worker.ranCount)
	}
	if len(summary.Results) != 1 || summary.Results[0].Applies {
		t.Fatalf("expected one skipped result, got %+v", summary.Results)
	}
}

func TestRunnerIsIdempotentThroughCheck(t *testing.T) {
	done := &fakePhase{name: "certs", applies: true, done: true}
	r, store := newTestRunner(t, done)
	summary, err := r.Run(context.Background(), newTestContext(), DefaultOptions("install"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done.ranCount != 0 {
		t.Fatalf("phase that reports done must not run, ran %d times", done.ranCount)
	}
	if !summary.Results[0].Done {
		t.Fatalf("result must be marked as already done: %+v", summary.Results[0])
	}
	rec, ok := store.Get("install", "certs")
	if !ok || !rec.AlreadyDone {
		t.Fatalf("state must record the already-done phase, got %+v (found=%v)", rec, ok)
	}
}

func TestRunnerDryRunDoesNotExecute(t *testing.T) {
	p := &fakePhase{name: "host-init", applies: true}
	r, _ := newTestRunner(t, p)
	c := newTestContext()
	c.DryRun = true
	summary, err := r.Run(context.Background(), c, DefaultOptions("install"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ranCount != 0 {
		t.Fatalf("dry-run must not execute phases, ran %d times", p.ranCount)
	}
	if summary.Results[0].Done || summary.Results[0].Err != nil {
		t.Fatalf("dry-run result must not be done or failed: %+v", summary.Results[0])
	}
}

func TestRunnerReportsCheckErrors(t *testing.T) {
	p := &fakePhase{name: "kubelet", applies: true, checkErr: errors.New("cannot read kubeconfig")}
	r, _ := newTestRunner(t, p)
	_, err := r.Run(context.Background(), newTestContext(), DefaultOptions("install"))
	if err == nil {
		t.Fatal("a check error must abort the run")
	}
	if p.ranCount != 0 {
		t.Fatal("phase must not run when Check fails")
	}
}

func TestRegistrySelectRejectsUnknownPhases(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&fakePhase{name: "preflight"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Select([]string{"preflight", "nope"}); err == nil {
		t.Fatal("unknown phase names must be rejected")
	}
	selected, err := reg.Select(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Name() != "preflight" {
		t.Fatalf("unexpected selection: %+v", selected)
	}
}

func TestRegistryRejectsDuplicateRegistration(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&fakePhase{name: "daemon"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&fakePhase{name: "daemon"}); err == nil {
		t.Fatal("duplicate registration must fail")
	}
}

func TestCanonicalOrderIsStable(t *testing.T) {
	reg := NewRegistry()
	// 乱序注册，Names 必须按规范顺序返回。
	for _, name := range []string{"addon", "preflight", "etcd", "network"} {
		if err := reg.Register(&fakePhase{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	got := reg.Names()
	want := []string{"preflight", "etcd", "network", "addon"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(constants.NewPaths(dir))
	rec := PhaseRecord{Name: "certs", Done: true, AppliedAt: time.Unix(1700000000, 0).UTC()}
	if err := store.Record("install", rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, ok := readState(t, store, "install")["certs"]
	if !ok {
		t.Fatal("record not found")
	}
	if !got.Done || !got.AppliedAt.Equal(rec.AppliedAt) {
		t.Fatalf("record mismatch: %+v", got)
	}
}

// readState 直接读状态文件；FileStore 的 Get/Reset 访问器只有测试使用，
// 生产需要的是 Record 与文件本身。
func readState(t *testing.T, store *FileStore, scope string) map[string]PhaseRecord {
	t.Helper()
	data, err := os.ReadFile(store.path(scope))
	if err != nil {
		t.Fatalf("read state %s: %v", scope, err)
	}
	recs := map[string]PhaseRecord{}
	if err := json.Unmarshal(data, &recs); err != nil {
		t.Fatalf("unmarshal state %s: %v", scope, err)
	}
	return recs
}

// TestRunnerRollsBackAfterCancellation 验证 Ctrl-C 之后仍然回滚，且回滚不被取消。
// TestFileStoreRebuildsCorruptState 验证损坏的状态文件会被重建，而不是永久损坏。
func TestFileStoreRebuildsCorruptState(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(constants.NewPaths(dir))
	if err := store.Record("install", PhaseRecord{Name: "certs", Done: true}); err != nil {
		t.Fatalf("record: %v", err)
	}
	path := filepath.Join(store.StateDir(), "install.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt the state file: %v", err)
	}
	// 损坏的事实必须报告给调用方（runner 只告警），但文件要同时被重建。
	if err := store.Record("install", PhaseRecord{Name: "etcd", Done: true}); err == nil {
		t.Fatal("a corrupt state file must be reported")
	}
	if _, ok := readState(t, store, "install")["etcd"]; !ok {
		t.Fatal("the record written during the rebuild must be readable")
	}
	if err := store.Record("install", PhaseRecord{Name: "network", Done: true}); err != nil {
		t.Fatalf("the rebuilt state file must be usable: %v", err)
	}
}

func TestFileStoreSanitizesScope(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(constants.NewPaths(dir))
	if err := store.Record("../../etc/passwd", PhaseRecord{Name: "x"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// scope 被清洗后，文件必须落在 state 目录里。
	if _, ok := readState(t, store, "../../etc/passwd")["x"]; !ok {
		t.Fatal("sanitized scope must be readable with the same name")
	}
	entries, err := readDirNames(store.StateDir())
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one state file, got %v", entries)
	}
}

// readDirNames 返回目录中的条目名。
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// DefaultOptions 是测试使用的默认运行选项（生产 install 显式构造 Options）。
func DefaultOptions(scope string) Options {
	return Options{Scope: scope}
}
