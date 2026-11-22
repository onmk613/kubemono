package reset

import (
	"context"

	"kubemono/internal/constants"
	"kubemono/internal/hostinit"
	"kubemono/internal/util/output"
)

// HostInitReverter 还原主机初始化所做的改动。
type HostInitReverter interface {
	// Revert 依据 /usr/local/kubemono/hostinit.json 逆序还原主机改动。
	Revert(ctx context.Context, paths constants.Paths) error
}

// journalHostInitReverter 复用 internal/hostinit.Reset 与真实 Applier。
// 不做"是否记录过"之外的判断：hostinit.Reset 自己会报告哪些变更无法自动还原，
// 调用方把错误原样交给运维人员，绝不静默丢弃。
type journalHostInitReverter struct {
	// Applier 执行还原动作；nil 时用平台默认实现。
	Applier hostinit.Applier
	// Report 接收进度消息。
	Report output.Reporter
}

// NewHostInitReverter 返回基于变更记录的还原实现。
func NewHostInitReverter(applier hostinit.Applier) HostInitReverter {
	return &journalHostInitReverter{Applier: applier}
}

// Revert 实现 HostInitReverter。
func (r *journalHostInitReverter) Revert(ctx context.Context, paths constants.Paths) error {
	journal, err := hostinit.Load(paths.HostInitRecord())
	if err != nil {
		return err
	}
	if journal == nil || len(journal.Entries) == 0 {
		r.report().Infof("没有主机初始化记录（%s），跳过还原", paths.HostInitRecord())
		return nil
	}
	return hostinit.Reset(ctx, journal, r.Applier)
}

func (r *journalHostInitReverter) report() output.Reporter {
	if r.Report == nil {
		return output.Discard()
	}
	return r.Report
}
