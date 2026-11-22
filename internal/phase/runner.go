package phase

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Options 控制一次 phase 运行的行为。
type Options struct {
	// Only 把执行限制在指定的 phase（空表示所有适用的）。
	Only []string
	// Scope 指定用于续跑的状态文件名，例如 "install"。
	Scope string
}

// Runner 执行 phase，按顺序运行、记录续跑状态；不重试、不做跨 phase 回滚。
type Runner struct {
	Registry *Registry
	State    Store
}

// NewRunner 返回由给定注册表和状态存储支撑的 runner。
func NewRunner(reg *Registry, store Store) *Runner {
	return &Runner{Registry: reg, State: store}
}

// Run 执行选中的 phase。
// 语义：
//   - Applies=false 的 phase 直接跳过（例如 worker 上的 cluster-init）；
//   - Check=true 的 phase 视为已完成，更新状态后跳过（幂等、可续跑）；
//   - 任一 phase 失败即返回（不重试、不做跨 phase 回滚）：每个 phase 自己保证
//     幂等，失败后用同一条命令续跑即可；需要回滚的 phase 在自身 Run 内完成
//     （例如 kubelet phase 的 render.Rollback）；
//   - DryRun 只执行 Check 并报告将要做的动作。
func (r *Runner) Run(ctx context.Context, c *Context, opts Options) (Summary, error) {
	var summary Summary
	if r.Registry == nil {
		return summary, errors.New("phase runner has no registry")
	}
	phases, err := r.Registry.Select(opts.Only)
	if err != nil {
		return summary, err
	}
	report := c.Reporter()

	for _, p := range phases {
		res := Result{Name: p.Name()}
		start := time.Now()
		// emit 统一完成“计时 + 追加结果 + 按需落状态”：各分支不重复
		// 维护 summary.Results，也不会出现“先 append 再回填最后一个元素”。
		emit := func(record bool) {
			if res.Duration == 0 {
				res.Duration = time.Since(start)
			}
			summary.Results = append(summary.Results, res)
			if !record {
				return
			}
			if err := r.record(c, opts.Scope, res); err != nil {
				report.Warnf("记录 phase 状态失败：%v", err)
			}
		}

		if !p.Applies(c) {
			emit(false)
			report.Debugf("phase %s: 不适用于本节点，跳过", p.Name())
			continue
		}
		res.Applies = true

		done, checkErr := p.Check(ctx, c)
		if checkErr != nil {
			res.Err = fmt.Errorf("check: %w", checkErr)
			summary.Failed = p.Name()
			emit(false)
			return summary, summary.Error()
		}
		if done {
			res.Done = true
			emit(true)
			report.Debugf("phase %s: 已完成，跳过", p.Name())
			continue
		}
		if c.DryRun {
			emit(false)
			report.Infof("phase %s: 需要执行（dry-run，不实际变更）", p.Name())
			continue
		}

		finish := report.Step(fmt.Sprintf("phase %s", p.Name()))
		res.Pending, res.Err = r.attempt(ctx, p, c)
		finish(res.Err)
		// 在这里固定时长，避免把随后的落状态/日志时间算进 phase。
		res.Duration = time.Since(start)

		if res.Err != nil {
			summary.Failed = p.Name()
			emit(true)
			return summary, summary.Error()
		}
		if res.Pending {
			summary.Pending = append(summary.Pending, p.Name())
		}
		emit(true)
	}
	return summary, nil
}

// attempt 执行一个 phase，并在成功后确认变更真的生效。
// 返回 pending=true 表示 Run 已成功，但事后 Check 仍认为未收敛或检查出错。
func (r *Runner) attempt(ctx context.Context, p Phase, c *Context) (pending bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := p.Run(ctx, c); err != nil {
		return false, err
	}
	// 有些 phase（preflight）的 Check 故意恒为 false：它是每次都要重新执行的
	// 闸门，不是“未收敛”。这类 phase 不产生 Pending 警告。
	if ap, ok := p.(interface{ AlwaysPending() bool }); ok && ap.AlwaysPending() {
		return false, nil
	}
	// 执行成功后确认真的生效；Check 失败不影响成功判定，但必须显式记录。
	done, checkErr := p.Check(ctx, c)
	if checkErr != nil {
		c.Reporter().Warnf("phase %s 执行后检查失败：%v", p.Name(), checkErr)
		return true, nil
	}
	if !done {
		c.Reporter().Warnf("phase %s 执行后仍未收敛；下次 repair 会继续", p.Name())
		return true, nil
	}
	return false, nil
}

func (r *Runner) record(c *Context, scope string, res Result) error {
	if r.State == nil || scope == "" {
		return nil
	}
	return r.State.Record(scope, PhaseRecord{
		Name:        res.Name,
		Done:        res.Err == nil && res.Applies,
		AlreadyDone: res.Done,
		Error:       errorString(res.Err),
		AppliedAt:   c.Clock(),
		Release:     c.Release,
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
