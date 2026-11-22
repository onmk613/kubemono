//go:build !unix

package lock

import "os"

// Lock 在非 unix 平台是空实现（项目只发布 Linux；保留 build 兼容）。
type Lock struct{}

// Acquire 在非 unix 平台不提供互斥。
func Acquire(string) (*Lock, error) { return &Lock{}, nil }

// Close 实现空释放。
func (l *Lock) Close() error { return nil }

var _ = os.Getpid
