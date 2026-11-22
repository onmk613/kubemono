//go:build unix

package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "kubemono.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}
	defer first.Close()

	second, err := Acquire(path)
	if err == nil {
		_ = second.Close()
		t.Fatal("同一路径的第二个锁必须失败")
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Acquire(path)
	if err != nil {
		t.Fatalf("释放后应能重新获取: %v", err)
	}
	_ = third.Close()
}
