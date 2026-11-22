package install

import (
	"context"
	"fmt"
	"os"
	"strings"

	"kubemono/internal/hostinit"
	"kubemono/internal/phase"
	hostinitphase "kubemono/internal/phase/hostinit"
	renderpkg "kubemono/internal/render"
)

// runDiff 打印本机受管文件与期望状态的差异，不做任何写操作。
//
// 它复用与 writeDesired 完全相同的渲染路径（同样的身份、同样的
// HostMTU/podCIDR 推导），因此 --diff 看到的就是下一次 node repair 会写的内容。
// 只比较本机受管文件，不读集群对象；需要 apiserver 的 phase（network 对象、
// addon）不在 diff 范围内。
func runDiff(ctx context.Context, opts *Options, pc *phase.Context, plan *Plan) error {
	spec, err := buildDesiredSpec(ctx, pc, plan)
	if err != nil {
		return err
	}
	// 用与 node repair 完全相同的路径计算差异：previous 快照 + 磁盘现实 vs 期望。
	// 只做 Drift(spec) 看不到“这一代不再需要、必须删除”的文件。
	previous, _ := renderpkg.NewStore(opts.Paths).Restore()
	changes := renderpkg.ComputeChanges(renderpkg.OverlayLive(previous, spec), spec)
	driftCount := len(changes)
	report := opts.Report
	if driftCount == 0 {
		report.Infof("本机 %d 个受管文件与期望状态一致", len(spec.Files))
	} else {
		report.Infof("发现 %d 个文件与期望状态不一致（共 %d 个受管文件）：", driftCount, len(spec.Files))
		for _, ch := range changes {
			report.Infof("%s  %s", changeLabel(ch.Kind), ch.File.Path)
			if ch.Kind == renderpkg.ChangeRemoved || ch.File.Secret {
				continue
			}
			live, err := os.ReadFile(ch.File.Path)
			if err != nil {
				continue
			}
			for _, line := range shortLineDiff(live, ch.File.Content, 6) {
				report.Infof("    %s", line)
			}
		}
	}
	// host-init 的文件/sysctl 不在 render.Spec 里；在真实主机上顺带做一次只读校验。
	// 测试根（Root != "") 下跳过：hostinit 的路径是真实主机的绝对路径。
	if opts.Paths.Root == "" && opts.Cluster != nil {
		host, ok := opts.Cluster.Cluster.Hosts.FindHost(plan.ConfigName)
		if ok {
			steps, err := hostinit.Plan(opts.Cluster, host)
			if err == nil {
				applier := hostinitphase.NewApplier()
				results, verifyErr := hostinit.VerifySteps(ctx, steps, applier, true)
				if verifyErr != nil {
					report.Warnf("主机基线校验失败：%v", verifyErr)
				} else {
					for _, r := range results {
						if r.OK {
							continue
						}
						driftCount++
						report.Infof("差异  host-init/%s  %s", r.Step.ID, r.Step.Title)
					}
				}
			}
		}
	}
	if driftCount > 0 && opts.DiffExitCode {
		return fmt.Errorf("%w（%d 处）", ErrDiffFound, driftCount)
	}
	return nil
}

func changeLabel(kind renderpkg.ChangeKind) string {
	switch kind {
	case renderpkg.ChangeAdded:
		return "缺失"
	case renderpkg.ChangeRemoved:
		return "残留"
	default:
		return "内容"
	}
}

// shortLineDiff 输出“磁盘 → 期望”的短 diff：去掉公共前后缀，只展示变化段，
// 每侧最多 max 行。配置文件的改动通常集中在一小段，这足以定位；完整 diff
// 交给 operator 自己用 diff -u 对比（我们不引入 diff 依赖）。
func shortLineDiff(live, want []byte, max int) []string {
	a := splitLines(live)
	b := splitLines(want)
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	oldMid := a[prefix : len(a)-suffix]
	newMid := b[prefix : len(b)-suffix]
	if len(oldMid) == 0 && len(newMid) == 0 {
		return nil
	}
	out := []string{fmt.Sprintf("@@ 第 %d 行起（磁盘 → 期望）@@", prefix+1)}
	for i, line := range oldMid {
		if i >= max {
			out = append(out, fmt.Sprintf("- …（共 %d 行，已截断）", len(oldMid)))
			break
		}
		out = append(out, "- "+line)
	}
	for i, line := range newMid {
		if i >= max {
			out = append(out, fmt.Sprintf("+ …（共 %d 行，已截断）", len(newMid)))
			break
		}
		out = append(out, "+ "+line)
	}
	return out
}

func splitLines(data []byte) []string {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
