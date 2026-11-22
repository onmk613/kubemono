package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kubemono/internal/artifact"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/render"
	"kubemono/internal/util/fsutil"
)

// containerdProvider 是 kubemono 托管的 containerd。
type containerdProvider struct {
	cfg  clusterconfig.RuntimeSpec
	opts Options
}

// Name 实现 Provider。
func (p *containerdProvider) Name() string { return ComponentContainerd }

// Install 安装运行时二进制、CNI 插件和 unit；幂等，dry-run 不检查物料。
func (p *containerdProvider) Install(_ context.Context) error {
	report := p.opts.reporter()
	if p.opts.DryRun {
		for _, b := range Binaries(p.cfg, p.opts.BOM) {
			report.Infof("将安装 %s（→ %s）", b.Name, LinkPath(p.opts.installDir(), b.Name))
		}
		if err := installCNIPlugins(p.opts); err != nil {
			return err
		}
		report.Infof("将写入 %s", p.opts.Paths.Resolve(UnitPath))
		return nil
	}

	for _, b := range Binaries(p.cfg, p.opts.BOM) {
		if b.Version == "" {
			if b.Required {
				return fmt.Errorf("物料缺少 %s 的 BOM 版本：确认 BOM 与物料匹配", b.Component)
			}
			continue
		}
		src, err := p.locate(b)
		if err != nil {
			if b.Required {
				return err
			}
			report.Debugf("跳过可选的 %s：%v", b.Name, err)
			continue
		}
		if err := installBinary(p.opts.installDir(), b, src); err != nil {
			return fmt.Errorf("安装 %s 失败：%w；确认物料完整（%s）", b.Name, err, src)
		}
	}

	if err := installCNIPlugins(p.opts); err != nil {
		return err
	}

	content, err := RenderUnit()
	if err != nil {
		return err
	}
	unitPath := p.opts.Paths.Resolve(UnitPath)
	changed, err := fsutil.WriteFileIfChanged(unitPath, content, 0o644)
	if err != nil {
		return fmt.Errorf("写入 %s 失败：%w；确认 /etc/systemd/system 可写", unitPath, err)
	}
	if changed {
		report.Infof("已写入 %s", unitPath)
	}
	return nil
}

// locate 在物料目录中定位一个可执行文件。
func (p *containerdProvider) locate(b Binary) (string, error) {
	return locateBinary(p.opts.artifactDir(), b)
}

// locateBinary 在物料目录中定位一个可执行文件。
func locateBinary(root string, b Binary) (string, error) {
	ref, err := artifact.LocateBinary(root, artifact.LoadMetaOptional(root), b.Component, b.Name, b.Category)
	if err != nil {
		return "", fmt.Errorf("%w（物料不完整，请重新执行 `kubemono bundle load <archive>` 导入 release %s 的完整物料）",
			err, orDefault(b.Version, "?"))
	}
	return ref.Path, nil
}

// orDefault 在值为空时返回回退值（仅用于错误消息）。
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// ApplyConfig 应用受管配置，返回重启/热加载判定。
func (p *containerdProvider) ApplyConfig(ctx context.Context, spec *render.Spec) (*Plan, error) {
	report := p.opts.reporter()
	desired := managedSpec(spec)

	var current *render.Spec
	if p.opts.Current != nil {
		current = managedSpec(p.opts.Current)
	} else {
		live, err := LiveSpec(p.opts.Paths)
		if err != nil {
			return nil, err
		}
		current = live
	}

	changes := render.ComputeChanges(current, desired)
	plan := &Plan{
		Changes:  changes,
		Decision: ClassifyChanges(configPath(p.opts.Paths), certsDir(p.opts.Paths), changes),
	}

	if len(changes) == 0 {
		report.Debugf("containerd 配置无变化")
		return plan, nil
	}
	if p.opts.DryRun {
		for _, ch := range changes {
			report.Infof("将写入 %s（%s，dry-run）", ch.File.Path, ch.Kind)
		}
		return plan, nil
	}

	ts := fsutil.Timestamp(p.opts.now())
	backupRoot := p.opts.Paths.Backup()
	plan.BackupDir = filepath.Join(backupRoot, ts)
	applied, err := render.Apply(changes, render.ApplyOptions{
		BackupDir: backupRoot,
		Timestamp: ts,
		Reporter:  p.opts.Report,
	})
	plan.Applied = applied
	if err != nil {
		return plan, fmt.Errorf("写入 containerd 配置失败：%w；确认 /etc/containerd 可写、/usr/local/kubemono/backup 所在分区有空间", err)
	}

	switch plan.Decision {
	case DecisionRestart:
		if err := p.opts.services().Action(ctx, DefaultServiceName, "restart"); err != nil {
			return plan, fmt.Errorf("重启 containerd 失败：%w；请手工执行 systemctl restart containerd 并查看 journalctl -u containerd", err)
		}
		plan.Restarted = true
		report.Infof("containerd 配置已更新，已重启 %s", DefaultServiceName)
	case DecisionReload:
		report.Infof("containerd 只有镜像仓库配置（certs.d）变化，热加载，不重启")
	}
	return plan, nil
}

// Status 实现 Provider：systemctl is-active + socket/containerd API 探测。
func (p *containerdProvider) Status(ctx context.Context) (Status, error) {
	return probe(ctx, p.opts, DefaultServiceName, DefaultCRISocket, true)
}

// Restart 实现 Provider。
func (p *containerdProvider) Restart(ctx context.Context) error {
	if err := p.opts.services().Action(ctx, DefaultServiceName, "restart"); err != nil {
		return fmt.Errorf("重启 containerd 失败：%w；请手工执行 systemctl restart containerd 并查看 journalctl -u containerd", err)
	}
	return nil
}

// probe 是共用的状态探测；custom 不读 containerd 配置。
// 托管 containerd 时用 ctr 确认 API 可用；使用 containerd 自带的 ctr。
func probe(ctx context.Context, opts Options, service, socket string, managed bool) (Status, error) {
	st := Status{CRISocket: socket}
	active, err := opts.services().IsActive(ctx, service)
	if err != nil {
		return st, fmt.Errorf("查询 %s 状态失败：%w；确认 systemd 可用（systemctl is-active %s）", service, err, service)
	}
	st.Running = active
	if managed {
		live, err := LiveSpec(opts.Paths)
		if err != nil {
			return st, err
		}
		st.ConfigHash = ConfigHash(ManagedFiles(live))
	}
	if !st.Running {
		st.Message = fmt.Sprintf("%s 未运行", service)
		return st, nil
	}
	socketPath := opts.Paths.Resolve(strings.TrimPrefix(socket, "unix://"))
	if _, err := os.Stat(socketPath); err != nil {
		st.Running = false
		st.Message = fmt.Sprintf("%s 运行中，但 socket %s 不存在", service, socket)
		return st, nil
	}
	if managed {
		ctr := filepath.Join(opts.installDir(), "ctr")
		if _, err := opts.commands().Run(ctx, ctr, "--address", socketPath, "version"); err != nil {
			if errors.Is(err, ErrCommandNotFound) {
				st.Message = fmt.Sprintf("%s 运行中，socket %s 存在（未找到 ctr，跳过 API 探测）", service, socket)
				return st, nil
			}
			return st, fmt.Errorf("ctr 查询 containerd API 失败：%w；确认运行时监听该 socket（journalctl -u %s）", err, service)
		}
	}
	st.Message = fmt.Sprintf("%s 运行中，socket %s 正常应答", service, socket)
	return st, nil
}

// RenderUnit 渲染 containerd.service 的内容。
func RenderUnit() ([]byte, error) {
	unit := render.SystemdUnit{
		Name:        DefaultUnitName,
		Description: "kubemono managed containerd",
		ExecStart:   "/usr/local/bin/containerd",
		Wants:       []string{"network-online.target"},
		After:       []string{"network-online.target"},
		Restart:     "always",
		RestartSec:  "5",
		Extra: []string{
			// containerd 需要 overlay 模块；失败不阻塞（模块可能已内建）。
			"ExecStartPre=-/sbin/modprobe overlay",
			"Delegate=yes",
			"KillMode=process",
			"OOMScoreAdjust=-999",
		},
	}
	return []byte(unit.Render()), nil
}
