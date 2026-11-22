package reset

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kubemono/internal/execx"
	"kubemono/internal/util/output"
)

// ctrBinary 是 containerd 自带的客户端。
const ctrBinary = "ctr"

// k8sNamespace 是 CRI 在 containerd 中使用的 namespace。
const k8sNamespace = "k8s.io"

// podStopper 通过 ctr 停止并删除 k8s.io namespace 下的全部容器。
// kubelet 已经先停了，因此这里处理的是仍在运行的节点级静态 Pod（local-proxy）
// 以及 kubelet 停止前调度上来的 Pod。
type podStopper struct {
	// Runner 执行 ctr；为 nil 时返回错误。
	Runner execx.Runner
	// Report 接收进度消息。
	Report output.Reporter
	// DryRun 只报告，不执行 ctr。
	DryRun bool
}

func (p podStopper) reporter() output.Reporter {
	if p.Report == nil {
		return output.Discard()
	}
	return p.Report
}

// List 返回 k8s.io namespace 下全部容器 task ID。
func (p podStopper) List(ctx context.Context) ([]string, error) {
	if p.Runner == nil {
		return nil, fmt.Errorf("ctr 不可用：没有配置命令执行器")
	}
	out, err := p.Runner.Run(ctx, ctrBinary, "-n", k8sNamespace, "tasks", "list", "-q")
	if err != nil {
		return nil, fmt.Errorf("执行 %s -n %s tasks list -q 失败: %w。请确认 containerd 正在运行且 ctr 可用",
			ctrBinary, k8sNamespace, err)
	}
	var ids []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// StopAll 停止并删除全部容器，返回实际处理的容器数量。
// 单个容器失败不会中断其余容器：错误在最后汇总返回。
func (p podStopper) StopAll(ctx context.Context) (int, error) {
	if p.DryRun {
		p.reporter().Infof("将用 %s 停止 %s namespace 下的全部容器（dry-run）", ctrBinary, k8sNamespace)
		return 0, nil
	}
	ids, err := p.List(ctx)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		p.reporter().Infof("没有正在运行的容器（原本未运行）")
		return 0, nil
	}
	p.reporter().Infof("发现 %d 个正在运行的容器，开始停止", len(ids))
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if _, err := p.Runner.Run(ctx, ctrBinary, "-n", k8sNamespace, "tasks", "kill", "-s", "SIGKILL", id); err != nil {
			errs = append(errs, fmt.Errorf("停止容器 %s: %w", id, err))
			continue
		}
		if _, err := p.Runner.Run(ctx, ctrBinary, "-n", k8sNamespace, "tasks", "delete", "-f", id); err != nil {
			errs = append(errs, fmt.Errorf("删除容器 %s: %w", id, err))
			continue
		}
		p.reporter().Infof("已停止并删除容器 %s", id)
	}
	return len(ids), errors.Join(errs...)
}
