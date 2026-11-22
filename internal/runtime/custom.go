package runtime

import (
	"context"
	"fmt"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/render"
)

// customProvider 是用户托管的 CRI；kubemono 不写它的配置文件。
type customProvider struct {
	cfg  clusterconfig.RuntimeSpec
	opts Options
}

// Name 实现 Provider。
func (p *customProvider) Name() string { return clusterconfig.RuntimeCustom }

// Install 只安装 CNI 插件；运行时本身由用户预装，kubemono 不执行安装脚本。
func (p *customProvider) Install(context.Context) error {
	// CNI 插件与运行时无关，照常安装。
	if err := installCNIPlugins(p.opts); err != nil {
		return err
	}
	return nil
}

// ApplyConfig 记录“按规定不修改配置”，返回 DecisionNone 的空计划。
func (p *customProvider) ApplyConfig(_ context.Context, spec *render.Spec) (*Plan, error) {
	if n := len(ManagedFiles(spec)); n > 0 {
		p.opts.reporter().Warnf("node.runtime.custom：忽略 %d 个 containerd 渲染文件，kubemono 不修改用户托管的运行时配置", n)
	}
	return &Plan{Decision: DecisionNone}, nil
}

// Status 实现 Provider：只看配置的 socket 与 serviceName。
func (p *customProvider) Status(ctx context.Context) (Status, error) {
	return probe(ctx, p.opts, p.cfg.Custom.ServiceName, p.cfg.Custom.CRISocket, false)
}

// Restart 实现 Provider：重启用户配置的 serviceName。
func (p *customProvider) Restart(ctx context.Context) error {
	name := p.cfg.Custom.ServiceName
	if err := p.opts.services().Action(ctx, name, "restart"); err != nil {
		return fmt.Errorf("重启 %s 失败：%w；确认 node.runtime.custom.serviceName 正确（systemctl status %s）", name, err, name)
	}
	return nil
}
