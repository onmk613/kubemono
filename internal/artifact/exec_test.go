package artifact

import (
	"os"
	"runtime"
	"testing"
)

func TestCheckExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable:", err)
	}
	if err := CheckExecutable(exe, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("CheckExecutable(%s/%s): %v", runtime.GOOS, runtime.GOARCH, err)
	}
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	if err := CheckExecutable(exe, runtime.GOOS, other); err == nil {
		t.Fatalf("arch mismatch should fail（%s vs %s）", runtime.GOARCH, other)
	}
}

func TestSniffExecutableRejectsText(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "text-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("#!/bin/sh\necho hi\n")
	_ = f.Close()
	info, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = info.Close() }()
	if got := SniffExecutable(info); got.Format != "" {
		t.Fatalf("text file detected as %+v", got)
	}
}
