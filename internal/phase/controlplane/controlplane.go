// Package controlplane 实现 `control-plane` phase：把渲染结果写下去并
// 等待组件真正生效。
// 顺序与依赖：apiserver → controller-manager → scheduler。
// 每一步都用 controlplane.Runner 做原子替换 + 健康等待 + 失败回滚，因此
// 单节点和多 master 走的是同一条代码路径。etcd 由 etcd phase 以 etcd.service
// 管理，不属于本 phase。
package controlplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kubemono/internal/constants"
	controlplane "kubemono/internal/controlplane"
	"kubemono/internal/encryption"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	"kubemono/internal/systemd"
	"kubemono/internal/util/fsutil"
)

// PhaseName 是 `control-plane` phase 的名称。
const PhaseName = "control-plane"

// restartMarkerName 是“控制面文件已写、组件尚未全部重启”的标记名。
const restartMarkerName = "control-plane"

func init() { phase.MustRegister(&Phase{}) }

// Phase 应用本节点的控制面组件。
type Phase struct {
	// RunnerFactory 构造 runner。为 nil 时使用 systemd runner。
	RunnerFactory func(c *phase.Context) (controlplane.Runner, error)
	// Spec 覆盖渲染出的期望状态（测试注入）。
	Spec *render.Spec
	// InstallBinary 从离线物料安装宿主二进制；nil 使用 internal/kubelet 的
	// 通用安装器。测试注入假实现。
	InstallBinary func(ctx context.Context, c *phase.Context, component string) error
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 报告本节点是否运行控制面组件。
func (p *Phase) Applies(c *phase.Context) bool {
	return c.IsMaster()
}

// desiredSpec 渲染（或返回注入的）期望状态。
func (p *Phase) desiredSpec(c *phase.Context) (*render.Spec, error) {
	return phase.DesiredSpec(c, p.Spec)
}

// components 按启动顺序返回本节点必须运行的组件。
func (p *Phase) components(c *phase.Context) []controlplane.Component {
	host, ok := c.Self()
	if !ok {
		return nil
	}
	if !host.IsMaster() {
		return nil
	}
	return []controlplane.Component{
		controlplane.ComponentAPIServer,
		controlplane.ComponentControllerManager,
		controlplane.ComponentScheduler,
	}
}

// Check 报告每个组件是否已应用且健康。
// 判据必须同时覆盖“内容”和“运行态”：
//   - 渲染出的 unit 与附属文件（审计策略）必须与磁盘一致；
//   - unit 必须已 enable，组件必须健康；
//   - 上次写文件后如果没完成重启（restart marker），必须重跑。
//
// 只看 unit 存在 + 健康会让额外状态漂移（extraArgs、审计策略、服务被 disable）
// 被静默跳过；这是无守护进程模型下最危险的假阳性。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	if pending, err := phase.HasPendingRestart(c, restartMarkerName); err != nil {
		return false, err
	} else if pending {
		c.Reporter().Debugf("控制面有待完成的证书/配置重启，需要重跑")
		return false, nil
	}
	spec, err := p.desiredSpec(c)
	if err != nil {
		return false, err
	}
	managed := p.managedFiles(c, spec)
	changes := render.ComputeChanges(render.OverlayLive(nil, managed), managed)
	if len(changes) > 0 {
		for _, ch := range changes {
			c.Reporter().Debugf("控制面文件需要更新：%s（%s）", ch.File.Path, ch.Kind)
		}
		return false, nil
	}
	runner, err := p.runner(c)
	if err != nil {
		return false, err
	}
	if !p.kubectlInstalled(c) {
		return false, nil
	}
	// 静态加密配置不在 render.Spec 里（密钥生成是 phase 的职责），必须单独检查：
	// 文件丢失后 apiserver 下次重启会起不来，而 unit/健康检查都还是正常的。
	if c.IsMaster() {
		enc := c.Paths.EncryptionConfig() + "/" + constants.EncryptionConfigName
		if !fsutil.FileExists(enc) {
			c.Reporter().Debugf("静态加密配置缺失：%s", enc)
			return false, nil
		}
	}
	for _, component := range p.components(c) {
		unit := c.Paths.Resolve("/etc/systemd/system/" + string(component) + ".service")
		if !fsutil.FileExists(unit) {
			return false, nil
		}
		if ec, ok := runner.(interface {
			IsEnabled(context.Context, controlplane.Component) (bool, error)
		}); ok {
			enabled, err := ec.IsEnabled(ctx, component)
			if err != nil {
				return false, err
			}
			if !enabled {
				c.Reporter().Debugf("%s.service 未设置开机自启，需要重跑", component)
				return false, nil
			}
		}
		if err := runner.Health(ctx, component); err != nil {
			return false, nil
		}
	}
	return true, nil
}

// Run 按顺序应用本节点的每个组件。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	spec, err := p.desiredSpec(c)
	if err != nil {
		return err
	}
	runner, err := p.runner(c)
	if err != nil {
		return err
	}
	report := c.Reporter()
	// 先落“待重启”标记，再写任何文件：如果进程在写文件与重启之间被杀，
	// 下一次 Check 必须知道还有组件没加载新配置。
	if !c.DryRun {
		if err := phase.WriteRestartMarker(c, restartMarkerName, p.componentNames(c),
			"控制面文件已更新，等待组件重启"); err != nil {
			return err
		}
	}
	// 附属文件（审计策略等）必须先于 apiserver 就位：apiserver 启动时立刻读
	// --audit-policy-file，缺文件会直接 crash-loop（真实环境踩过：
	// /etc/kubernetes/audit/policy.yaml 漏写，apiserver 反复 Exited）。
	// 控制面是宿主二进制 + systemd unit：必须先从离线物料安装。真机实测：缺了
	// 这一步，unit 启动即 203/EXEC（找不到 /usr/local/bin/kube-apiserver）。
	if !c.DryRun {
		// kubectl 是控制面的管理入口：跟控制面二进制一起安装到 /usr/local/bin，
		// 不单独维护命令行工具阶段。
		if err := phase.InstallComponent(ctx, c, p.InstallBinary, constants.ComponentKubectl); err != nil {
			return err
		}
		for _, component := range p.components(c) {
			if err := phase.InstallComponent(ctx, c, p.InstallBinary, string(component)); err != nil {
				return err
			}
		}
	} else if host, ok := c.Self(); ok && host.IsMaster() {
		report.Infof("将安装 kubectl")
	}
	if err := p.applySideFiles(c, spec); err != nil {
		return err
	}
	// 静态加密配置必须在 apiserver 启动前存在：密钥只在 master 上生成，
	// 且必须复用已有密钥（重新生成会让已有 Secret 无法解密）。
	if c.IsMaster() {
		path := c.Paths.EncryptionConfig() + "/" + constants.EncryptionConfigName
		if c.DryRun {
			report.Infof("将确保静态加密配置存在：%s", path)
		} else if created, err := encryption.EnsureConfig(path, c.IsInitNode); err != nil {
			return err
		} else if created {
			report.Infof("已生成静态加密配置 %s（密钥只保存在本机）", path)
		} else {
			report.Debugf("静态加密配置已存在，复用原密钥：%s", path)
		}
	}
	for _, component := range p.components(c) {
		unitPath := c.Paths.Resolve("/etc/systemd/system/" + string(component) + ".service")
		file, ok := spec.Find(unitPath)
		if !ok {
			continue
		}
		if c.DryRun {
			report.Infof("将应用 %s（%s）", component, file.Path)
			continue
		}
		rs := &controlplane.RenderedSpec{
			Component: component,
			Unit:      file.Content,
		}
		if err := runner.Apply(ctx, rs); err != nil {
			return fmt.Errorf("应用 %s: %w", component, err)
		}
		report.Infof("%s 已生效", component)
	}
	if !c.DryRun {
		if err := phase.ClearRestartMarker(c, restartMarkerName); err != nil {
			return err
		}
	}
	return nil
}

// componentNames 返回本节点控制面组件名，用于 restart marker。
func (p *Phase) componentNames(c *phase.Context) []string {
	components := p.components(c)
	out := make([]string, 0, len(components))
	for _, component := range components {
		out = append(out, string(component))
	}
	return out
}

// kubectlInstalled 报告控制面的 kubectl 是否已经安装到 /usr/local/bin。
// 只看真实文件，不依赖物料（Check 可能在物料还没拉取的规划阶段运行）。
func (p *Phase) kubectlInstalled(c *phase.Context) bool {
	host, ok := c.Self()
	if !ok || !host.IsMaster() {
		return true
	}
	if c.BOM == nil || c.BOM.ComponentVersion(constants.ComponentKubectl) == "" {
		return false
	}
	target := c.Paths.Resolve(filepath.Join(constants.DefaultLocalBinDir, constants.ComponentKubectl))
	info, err := os.Lstat(target)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// managedFiles 返回本节点控制面组件拥有的全部受管文件（unit + 审计策略等）。
// Check 用它做内容/权限比较：unit 由 controlplane.Runner 应用，附属文件由
// 本 phase 自己写；两者漏掉任何一个都会让配置漂移被静默跳过。
func (p *Phase) managedFiles(c *phase.Context, spec *render.Spec) *render.Spec {
	out := &render.Spec{
		Node:      spec.Node,
		ClusterID: spec.ClusterID,
		Release:   spec.Release,
	}
	for _, component := range p.components(c) {
		for _, f := range spec.FilesOf(string(component)) {
			if f.Kind == render.KindManifest {
				continue
			}
			out.Add(f)
		}
	}
	out.Sort()
	return out
}

// sideFiles 返回 unit 之外的附属文件（目前是审计策略）。
// unit 由 controlplane.Runner 负责（原子替换 + 健康等待 + 回滚），这里不再
// 重复写一遍；否则 Run 会在 daemon-reload 之前先落一次 unit，语义重复。
func (p *Phase) sideFiles(c *phase.Context, spec *render.Spec) *render.Spec {
	out := &render.Spec{
		Node:      spec.Node,
		ClusterID: spec.ClusterID,
		Release:   spec.Release,
	}
	for _, component := range p.components(c) {
		for _, f := range spec.FilesOf(string(component)) {
			if f.Kind == render.KindManifest || f.Kind == render.KindUnit {
				continue
			}
			out.Add(f)
		}
	}
	out.Sort()
	return out
}

// applySideFiles 幂等地写出附属文件：内容与磁盘一致时不重写（避免无谓的重启）。
func (p *Phase) applySideFiles(c *phase.Context, spec *render.Spec) error {
	side := p.sideFiles(c, spec)
	if len(side.Files) == 0 {
		return nil
	}
	changes := render.ComputeChanges(render.OverlayLive(nil, side), side)
	if len(changes) == 0 {
		return nil
	}
	report := c.Reporter()
	if c.DryRun {
		for _, ch := range changes {
			report.Infof("将写入 %s（%04o，dry-run）", ch.File.Path, ch.File.Mode)
		}
		return nil
	}
	if _, err := render.ApplyChanges(changes, c.Paths, c.Clock(), report); err != nil {
		return fmt.Errorf("写入控制面附属文件失败：%w", err)
	}
	for _, ch := range changes {
		report.Infof("已写入 %s", ch.File.Path)
	}
	return nil
}

// runner 返回 systemd runner。
func (p *Phase) runner(c *phase.Context) (controlplane.Runner, error) {
	if p.RunnerFactory != nil {
		return p.RunnerFactory(c)
	}
	opts := controlplane.Options{
		Paths:  c.Paths,
		Health: controlplane.NewHTTPChecker(),
		Report: func(format string, args ...any) { c.Reporter().Debugf(format, args...) },
	}
	return controlplane.NewRunner(opts, systemd.Manager{Timeout: 30 * time.Second})
}
