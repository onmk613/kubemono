// Package hostinit 实现 `host-init` 阶段：按 的顺序把主机改成
// Kubernetes 需要的状态，并把每一项变更记入 hostinit.json 以便精确还原
package hostinit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/hostinit"
	"kubemono/internal/phase"
	"kubemono/internal/util/fsutil"
)

// PhaseName 是 `host-init` phase 的名称。
const PhaseName = "host-init"

func init() { phase.MustRegister(&Phase{}) }

// Phase 应用声明式的主机初始化计划。
type Phase struct {
	// Applier 执行变更；为 nil 时使用真实主机的 applier。
	Applier hostinit.Applier
	// Steps 可选地覆盖计划（测试用）。
	Steps []hostinit.Step
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 报告本节点是否需要主机初始化：所有节点都需要。
func (p *Phase) Applies(c *phase.Context) bool {
	_, ok := c.Self()
	return ok
}

// Check 报告本节点是否已经初始化。
// 判据是两条可验证的不变量，而不是"每个步骤都有日志"：
// 1. 存在非空的变更记录（说明 host-init 已经跑过）；
// 2. 全部受管文件与目录都在位。
// 不看 swap：kubemono 按设计不修改 swap（kubelet 已配 failSwapOn: false）；
// 以"swap 已关闭"为完成判据会让开启 swap 的主机永远重跑 host-init，
// 连带每次重跑用户的 pre-host-init hook。
// 之所以不要求 journal 覆盖每个步骤：内核模块加载失败会被跳过，不产生记录；
// 这类步骤由 Verify 的“无法观测即视为完成”兜底。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	journal := p.journal(c)
	if journal == nil || len(journal.Entries) == 0 {
		return false, nil
	}
	steps, err := p.steps(ctx, c)
	if err != nil {
		return false, err
	}
	applier := p.applier(nil)
	// 复用 Apply 的读/算逻辑；Check 不维护第二套容易漏项（文件/目录/
	// sysctl）的 switch。测试根演练（Root != ""）时跳过文件内容校验，
	// 因为路径已被重定位、真实主机文件不应参与比较。
	ok, err := hostinit.Verify(ctx, steps, applier, c.Paths.Root == "")
	if err != nil {
		return false, err
	}
	return ok, nil
}

// applier 返回要使用的 applier，需要时构建真实实现。
func (p *Phase) applier(report func(string, ...any)) hostinit.Applier {
	if p.Applier != nil {
		return p.Applier
	}
	real := NewApplier()
	if report != nil {
		real.Report = report
	}
	return real
}

// Run 构建并应用计划。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	report := c.Reporter()
	steps, err := p.steps(ctx, c)
	if err != nil {
		return err
	}
	if c.Paths.Root != "" && p.Applier == nil && !c.DryRun {
		return fmt.Errorf("host-init 会修改真实主机，不能用测试根（Paths.Root）重定向执行；请改用 --dry-run 预览，或在目标主机上直接运行")
	}
	if c.DryRun {
		report.Infof("将应用 %d 项主机变更（dry-run）：", len(steps))
		for _, st := range steps {
			report.Infof("  %s", st.Title)
		}
		return nil
	}
	applier := p.applier(report.Debugf)
	journal := p.journal(c)
	for i := range steps {
		report.Debugf("主机变更 %s", steps[i].Title)
	}
	if err := hostinit.ApplyWithOptions(ctx, steps, applier, journal, hostinit.ApplyOptions{
		SkipKernelErrors: true,
		Warnf:            report.Warnf,
	}); err != nil {
		return err
	}
	if err := journal.Save(); err != nil {
		return fmt.Errorf("保存主机初始化记录 %s: %w", journal.Path, err)
	}
	report.Infof("已记录 %d 项主机变更到 %s", len(journal.Entries), journal.Path)
	report.Result(true, "主机初始化完成（%d 项变更）", len(steps))
	return nil
}

// steps 返回计划（注入的或计算的）。
func (p *Phase) steps(ctx context.Context, c *phase.Context) ([]hostinit.Step, error) {
	if p.Steps != nil {
		return p.Steps, nil
	}
	if c.Cluster == nil {
		return nil, fmt.Errorf("host-init: 缺少集群配置")
	}
	host, ok := c.Self()
	if !ok {
		return nil, fmt.Errorf("host-init: 无法确定本机在集群配置中的条目")
	}
	steps, err := hostinit.Plan(c.Cluster, host)
	if err != nil {
		return nil, err
	}
	return relocate(c, steps), nil
}

// relocate 把计划里的绝对宿主路径改写到测试根或自定义 kubemono 数据目录下。
// 生产环境 Root 为空；--data-dir 仍需要把 Plan 里的默认 /usr/local/kubemono
// 及其子目录改到实际数据目录。没有这一步，自定义目录会去创建默认目录。
func relocate(c *phase.Context, steps []hostinit.Step) []hostinit.Step {
	if c == nil {
		return steps
	}
	out := make([]hostinit.Step, len(steps))
	copy(out, steps)
	for i := range out {
		out[i].Payload = relocatePayload(c.Paths, out[i].Payload)
	}
	return out
}

// relocatePayload 把 typed payload 里的绝对宿主路径改写到 Paths.Root 下。
func relocatePayload(paths constants.Paths, payload any) any {
	kubemonoPaths := map[string]string{
		constants.KubemonoDir:  paths.Kubemono(),
		constants.DesiredDir:   paths.Desired(),
		constants.StateDir:     paths.State(),
		constants.ArtifactsDir: paths.Artifacts(),
		constants.BackupDir:    paths.Backup(),
	}
	path := func(p string) string {
		if mapped, ok := kubemonoPaths[p]; ok {
			return mapped
		}
		if strings.HasPrefix(p, "/") {
			return paths.Resolve(p)
		}
		return p
	}
	switch p := payload.(type) {
	case hostinit.FilePayload:
		p.Path = path(p.Path)
		return p
	case hostinit.DirPayload:
		p.Path = path(p.Path)
		return p
	default:
		return payload
	}
}

// journal 返回变更记录（从磁盘加载；无上下文时用内存实现）。
func (p *Phase) journal(c *phase.Context) *hostinit.Journal {
	if c == nil {
		return hostinit.NewMemory()
	}
	path := c.Paths.HostInitRecord()
	j, err := hostinit.Load(path)
	if err == nil && j != nil {
		return j
	}
	if err != nil {
		// 损坏的记录不能当成"没有记录"再整份覆盖：那会永久丢掉 /etc/hosts、
		// sysctl、时区、服务状态等还原所需的 Before 值，之后 `kubemono reset`
		// 再也回不到原状。保留证据、明确告警，然后重建。
		broken := path + ".corrupt-" + fsutil.Timestamp(time.Now())
		if renameErr := os.Rename(path, broken); renameErr != nil {
			c.Reporter().Warnf("主机变更记录 %s 无法解析（%v），改名备份也失败（%v）；本次将重建记录，原始内容可能丢失", path, err, renameErr)
		} else {
			c.Reporter().Warnf("主机变更记录 %s 无法解析（%v）；已备份为 %s，本次将重建记录", path, err, broken)
		}
	}
	fresh := hostinit.NewMemory()
	fresh.Path = path
	return fresh
}
