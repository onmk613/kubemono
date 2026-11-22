//go:build linux

package reset

import (
	"kubemono/internal/hostinit"
	phasehostinit "kubemono/internal/phase/hostinit"
)

// newHostInitReverter 返回使用真实 Applier 的还原实现（linux）。
// 复用 internal/phase/hostinit 的 Applier：文件、sysctl、内核模块、systemd、
// 时区、swap 的实现只有一份，reset 与 host reset 的行为因此完全一致。
func newHostInitReverter() HostInitReverter {
	return NewHostInitReverter(phasehostinit.NewApplier())
}

// 编译期确认 Applier 实现了 hostinit.Applier。
var _ hostinit.Applier = phasehostinit.NewApplier()
