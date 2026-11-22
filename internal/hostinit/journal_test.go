package hostinit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJournalRecordMemory(t *testing.T) {
	j := NewMemory()
	entry := Entry{StepID: "s1", Kind: KindFile, Target: "/tmp/x", Before: "old", After: "new", Revertible: true}
	if err := j.Record(entry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(j.Entries) != 1 || j.Entries[0].ID == "" {
		t.Fatalf("entries = %+v", j.Entries)
	}
}

func TestJournalRecordLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostinit.json")
	j := &Journal{Path: path}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := j.Record(Entry{StepID: "s1", Kind: KindSysctl, Target: "net.ipv4.ip_forward", Before: "0", After: "1", AppliedAt: now}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Before != "0" || !loaded.Entries[0].AppliedAt.Equal(now) {
		t.Fatalf("loaded = %+v", loaded.Entries)
	}
}

func TestJournalLoadEdgeCases(t *testing.T) {
	dir := t.TempDir()
	if j, err := Load(filepath.Join(dir, "missing.json")); err != nil || len(j.Entries) != 0 {
		t.Fatalf("missing: %v, %+v", err, j)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if j, err := Load(empty); err != nil || len(j.Entries) != 0 {
		t.Fatalf("empty: %v, %+v", err, j)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("损坏的记录应报错")
	}
}

func TestResetRestoresFilesAndSysctls(t *testing.T) {
	ap := NewFakeApplier()
	ap.files["/tmp/f"] = "old"
	ap.sysctlValues["net.ipv4.ip_forward"] = "0"
	j := NewMemory()
	steps := []Step{
		{ID: "file", Kind: KindFile, Payload: FilePayload{Path: "/tmp/f", Mode: 0o644, Content: "new"}},
		{ID: "sysctl", Kind: KindSysctl, Payload: SysctlPayload{Key: "net.ipv4.ip_forward", Value: "1"}},
	}
	if err := ApplyWithOptions(context.Background(), steps, ap, j, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ap.files["/tmp/f"] != "new" {
		t.Fatalf("apply 后文件 = %q", ap.files["/tmp/f"])
	}
	if err := Reset(context.Background(), j, ap); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if ap.files["/tmp/f"] != "old" {
		t.Fatalf("reset 后文件 = %q", ap.files["/tmp/f"])
	}
	if ap.sysctlValues["net.ipv4.ip_forward"] != "0" {
		t.Fatalf("reset 后 sysctl = %q", ap.sysctlValues["net.ipv4.ip_forward"])
	}
}

func TestResetReportsNonRevertible(t *testing.T) {
	j := NewMemory()
	j.Entries = []Entry{
		{ID: "1", Kind: KindModule, Target: "overlay", Revertible: false},
		{ID: "2", Kind: KindDir, Target: "/tmp/x", Revertible: false},
	}
	err := Reset(context.Background(), j, NewFakeApplier())
	if err == nil || !strings.Contains(err.Error(), "不可自动还原") {
		t.Fatalf("应报告不可还原项: %v", err)
	}
}

func TestResetAggregatesFailures(t *testing.T) {
	ap := NewFakeApplier()
	ap.failOn["write:/tmp/f"] = errors.New("boom")
	j := NewMemory()
	j.Entries = []Entry{{ID: "1", Kind: KindFile, Target: "/tmp/f", Before: "old", Revertible: true}}
	err := Reset(context.Background(), j, ap)
	if err == nil || !strings.Contains(err.Error(), "/tmp/f") {
		t.Fatalf("应汇总还原失败: %v", err)
	}
}

func TestJournalRecordFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := &Journal{Path: filepath.Join(blocker, "hostinit.json")}
	if err := j.Record(Entry{Kind: KindFile, Target: "/tmp/x"}); err == nil {
		t.Fatal("写入失败应返回错误")
	}
}

func TestRecordDeduplicatesStepID(t *testing.T) {
	j := NewMemory()
	if err := j.Record(Entry{StepID: "s1", Kind: KindFile, Target: "/tmp/f", Before: "old", After: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Record(Entry{StepID: "s1", Kind: KindFile, Target: "/tmp/f", Before: "ignored", After: "v2"}); err != nil {
		t.Fatal(err)
	}
	if len(j.Entries) != 1 || j.Entries[0].Before != "old" || j.Entries[0].After != "v2" {
		t.Fatalf("entries = %+v", j.Entries)
	}
}
