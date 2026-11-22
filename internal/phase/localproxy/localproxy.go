// Package localproxy 实现 `local-proxy` 阶段：worker 上的本地代理（haproxy）。
// 一个容易搞错的地方：master **不需要**本地代理。master 上的
// kubelet/kcm/scheduler 访问 127.0.0.1:6443，那就是本机 apiserver；只有 worker
// 才需要 haproxy 把 127.0.0.1:6443 转发到某台 master。因此本 phase 的 Applies
// 按**配置里的角色**判断，并在报告里说明为什么跳过。
package localproxy

import (
	"context"
	"fmt"
	"os"
	"slices"

	"kubemono/internal/constants"
	"kubemono/internal/controlplane"
	"kubemono/internal/loadbalancer"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	"kubemono/internal/util/fsutil"
)

// PhaseName 是 `local-proxy` phase 的名称。
const PhaseName = "local-proxy"

func init() { phase.MustRegister(&Phase{}) }

// Phase 应用本节点的 worker 本地代理。
type Phase struct {

	// Spec 覆盖渲染出的期望状态（测试用）。
	Spec *render.Spec
	// HTTP 探测本地代理健康端点。
	HTTP loadbalancer.HTTPGetter
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 实现 phase.Phase。
// ：master 直连本机 apiserver，因此本地代理只在 worker 上需要。
func (p *Phase) Applies(c *phase.Context) bool {
	if c == nil {
		return false
	}
	if c.Cluster == nil {
		return false
	}
	host, ok := c.Self()
	if !ok {
		// 身份还没建立时（install 早期）不阻断流程：由 Run 里的渲染判断兜底。
		return true
	}
	return !host.IsMaster()
}

// proxy 返回本节点的本地代理渲染器。
func (p *Phase) proxy(c *phase.Context) (*loadbalancer.LocalProxy, error) {
	return loadbalancer.NewLocalProxy(c.RenderContext())
}

// desired 返回本节点渲染出的期望状态。
func (p *Phase) desired(c *phase.Context) (*render.Spec, []loadbalancer.Backend, error) {
	if p.Spec != nil {
		return p.Spec, nil, nil
	}
	lp, err := p.proxy(c)
	if err != nil {
		return nil, nil, err
	}
	rc := c.RenderContext()
	if !lp.Applies(rc) {
		return &render.Spec{}, nil, nil
	}
	spec := &render.Spec{Node: c.NodeName()}
	if err := lp.Render(rc, spec); err != nil {
		return nil, nil, fmt.Errorf("渲染本地代理失败：%w", err)
	}
	spec.Sort()
	return spec, lp.Backends(rc), nil
}

func localProxyEnabled(c *phase.Context) bool {
	return c != nil && c.Cluster != nil && c.Cluster.ControlPlane.LocalProxyEnabled()
}

// managedPaths 是本 phase 拥有的文件。即使缺少上一次的期望状态快照，
// 也用它们做禁用后的清理。
func (p *Phase) managedPaths(c *phase.Context) []string {
	return []string{
		loadbalancer.LocalProxyConfigPath(c.RenderContext()),
		c.Paths.ManifestPath(constants.ComponentLocalProxy),
	}
}

func (p *Phase) hasManagedFiles(c *phase.Context) bool {
	return slices.ContainsFunc(p.managedPaths(c), fsutil.FileExists)
}

// remove 在集群已有稳定外部入口（externalEndpoints 非空）时
// 删除 worker 上遗留的 haproxy 文件。
func (p *Phase) remove(c *phase.Context) error {
	report := c.Reporter()
	old := &render.Spec{Node: c.NodeName()}
	if prev := c.PreviousSpec(); prev != nil {
		old = render.SelectComponent(prev, constants.ComponentLocalProxy)
	}
	for _, path := range p.managedPaths(c) {
		if !fsutil.FileExists(path) {
			continue
		}
		if _, ok := old.Find(path); !ok {
			old.Files = append(old.Files, render.File{
				Path: path, Component: constants.ComponentLocalProxy, Kind: render.KindOther,
			})
		}
	}
	changes := render.ComputeChanges(old, &render.Spec{Node: c.NodeName()})
	if len(changes) == 0 {
		report.Infof("本地代理已禁用，且没有遗留文件")
		return nil
	}
	if c.DryRun {
		report.Infof("将删除 %d 个本地代理遗留文件（dry-run）", len(changes))
		for _, ch := range changes {
			report.Infof("  %s %s", ch.Kind, ch.File.Path)
		}
		return nil
	}
	_, err := render.ApplyChanges(changes, c.Paths, c.Clock(), report)
	if err != nil {
		return fmt.Errorf("删除本地代理遗留文件失败：%w", err)
	}
	report.Infof("本地代理已禁用：已删除 %d 个遗留文件", len(changes))
	return nil
}

// Check 报告本地代理是否已经应用且健康。
// 判据：
// 1. live 配置文件里的后端列表与 desired 一致；
// 2. desired 文件与 live 一致；
// 3. 127.0.0.1:<port> 的统计端点可访问。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	host, ok := c.Self()
	if ok && host.IsMaster() {
		// master 上本地代理不适用，视为"已完成"（Applies 已经会跳过它）。
		return true, nil
	}
	if !localProxyEnabled(c) {
		return !p.hasManagedFiles(c), nil
	}
	spec, desiredBackends, err := p.desired(c)
	if err != nil {
		return false, err
	}
	if spec == nil || len(spec.Files) == 0 {
		return true, nil
	}
	if err := p.checkBackends(c, desiredBackends); err != nil {
		c.Reporter().Debugf("本地代理后端列表与期望不一致：%v", err)
		return false, nil
	}
	drift, err := render.Drift(spec)
	if err != nil {
		return false, err
	}
	if len(drift) > 0 {
		return false, nil
	}
	if err := p.checkHealth(); err != nil {
		c.Reporter().Debugf("本地代理未就绪：%v", err)
		return false, nil
	}
	return true, nil
}

// checkBackends 比较 live 后端列表与期望后端列表。
func (p *Phase) checkBackends(c *phase.Context, desired []loadbalancer.Backend) error {
	if p.Spec != nil {
		// 注入的 Spec 没有后端语义，只比对文件。
		return nil
	}
	configPath := loadbalancer.LocalProxyConfigPath(c.RenderContext())
	content, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%w", configPath, err)
	}
	live := loadbalancer.LocalProxyBackendsFromConfig(string(content))
	if !loadbalancer.BackendsEqual(desired, live) {
		return fmt.Errorf("live=%v desired=%v", live, desired)
	}
	return nil
}

// checkHealth 探测本地代理的统计端点。
// 统计端点在独立的 stats 端口上；127.0.0.1:6443 是转发给 apiserver 的
// TCP 入口，对它发 HTTP 请求只会被转给 TLS 的 apiserver（返回 400）。
func (p *Phase) checkHealth() error {
	port := loadbalancer.LocalProxyStatsPort
	if p.HTTP != nil {
		status, err := p.HTTP.Get(fmt.Sprintf("http://127.0.0.1:%d/stats", port))
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("http://127.0.0.1:%d/stats 返回 HTTP %d", port, status)
		}
		return nil
	}
	return controlplane.NewHTTPChecker().Check(context.Background(), controlplane.HealthCheck{
		Scheme:  "http",
		Address: fmt.Sprintf("127.0.0.1:%d", port),
		Path:    "/stats",
	})
}

// Run 应用本节点的本地代理配置。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	report := c.Reporter()
	host, _ := c.Self()
	if host != nil && host.IsMaster() {
		report.Infof("本节点是 master：kubelet/kcm/scheduler 直连 127.0.0.1:6443（本机 apiserver），不需要本地代理")
		return nil
	}
	if !localProxyEnabled(c) {
		return p.remove(c)
	}
	spec, _, err := p.desired(c)
	if err != nil {
		return err
	}
	if spec == nil || len(spec.Files) == 0 {
		report.Infof("没有需要应用的本地代理文件")
		return nil
	}
	// 先看 live 是否已经等于 desired：相等就不写、不 reload。
	// 这条快速路径让 phase 在"上一次已经应用过"时是幂等的。
	if drift, err := render.Drift(spec); err != nil {
		return err
	} else if len(drift) == 0 {
		report.Infof("本地代理配置已是最新，无需 reload")
		return nil
	}
	// 快照是整机共享的：先按本 phase 负责的组件过滤，否则会把别的组件的文件
	// （控制面清单、kubelet 配置……）判成"应删除"并真的删掉。
	changes := render.ComputeChanges(render.OverlayLive(render.SelectComponents(c.PreviousSpec(), spec), spec), spec)
	if c.DryRun {
		report.Infof("将应用 %d 个本地代理文件变更（dry-run）", len(changes))
		for _, ch := range changes {
			report.Infof("  %s %s", ch.Kind, ch.File.Path)
		}
		return nil
	}
	_, err = render.ApplyChanges(changes, c.Paths, c.Clock(), report)
	if err != nil {
		return fmt.Errorf("应用本地代理配置失败：%w", err)
	}
	if len(changes) == 0 {
		report.Infof("本地代理配置已是最新")
		return nil
	}
	report.Infof("本地代理配置已更新：清单已替换，kubelet 会重建 haproxy pod")
	return nil
}
