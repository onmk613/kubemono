// Package network 实现 `network` phase：应用网络插件与 Service 代理。
// 它是集群级与本机级的混合：
// - 集群级对象（kube-router/kube-proxy 的 DaemonSet、ConfigMap、
// ServiceAccount、RBAC）只能经过 apiserver 创建，因此需要一台 master 上的
// admin kubeconfig；client 为空时明确报错，而不是静默跳过；
// - 本机文件（CNI 配置、静态 Pod 清单、配置文件）用 render.ComputeChanges +
// render.Apply 原子落盘，与其它 phase 完全一致。
package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"kubemono/internal/clusterapply"
	"kubemono/internal/constants"
	"kubemono/internal/controlplane"
	"kubemono/internal/network"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	"kubemono/internal/systemd"
	"kubemono/internal/util/output"
)

// PhaseName 是 `network` phase 的名字。
const PhaseName = "network"

func init() { phase.MustRegister(&Phase{}) }

// Phase 应用本节点的网络层。
type Phase struct {

	// Spec 覆盖渲染出的期望状态（测试注入）。
	Spec *render.Spec
	// Objects 覆盖渲染出的集群对象（测试）。
	Objects *render.ObjectSet
	// Client 是集群级对象使用的 apiserver 客户端。
	Client kubernetes.Interface
	// Checker 覆盖网络探测实现（测试）。
	Checker NetworkChecker
	// RestartRuntime 让容器运行时重新加载 CNI 配置；nil 用 systemctl restart containerd。
	// 测试注入假实现（测试机上不能真去重启 systemd 服务）。
	RestartRuntime func(ctx context.Context) error
}

// NetworkChecker 是 Check 使用的可注入探测实现。
type NetworkChecker interface {
	// CheckLocalFiles 比较 live 文件与期望 spec。
	CheckLocalFiles(spec *render.Spec) (bool, error)
	// CheckHealth 探测每个健康端点。
	CheckHealth(ctx context.Context, healths []network.Health) error
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 实现 phase.Phase：网络层覆盖所有节点。
func (p *Phase) Applies(*phase.Context) bool { return true }

// kubeClient 返回集群级对象使用的客户端。
// 注入的 Client 优先（测试与 `node repair --kubeconfig` 场景）；
// 否则走 phase.Context 的惰性解析（install 在控制面起来之后才读 admin.conf）。
func (p *Phase) kubeClient(c *phase.Context) (kubernetes.Interface, error) {
	if p.Client != nil {
		return p.Client, nil
	}
	return c.KubeClient()
}

// plan 渲染（或返回注入的）期望状态。
func (p *Phase) plan(ctx context.Context, c *phase.Context) (*network.Plan, error) {
	if p.Spec != nil {
		plan := &network.Plan{}
		plan.Files = append(plan.Files, p.Spec.Files...)
		if p.Objects != nil {
			plan.Objects = append(plan.Objects, p.Objects.Objects()...)
		}
		return plan, nil
	}
	cidr, err := p.nodeCIDR(ctx, c)
	if err != nil {
		return nil, err
	}
	plan, err := network.Build(c.RenderContext(), cidr)
	if err != nil {
		return nil, fmt.Errorf("渲染网络期望状态失败：%w", err)
	}
	if p.Objects != nil {
		plan.Objects = p.Objects.Objects()
	}
	return plan, nil
}

// desiredSpec 返回本节点网络层的期望状态。
// 本地代理（haproxy）由 local-proxy phase 独占：它自己写/删自己的文件，
// network 只负责 CNI 与集群对象，避免两份 spec 互相把对方的文件判成删除。
func (p *Phase) desiredSpec(plan *network.Plan) (*render.Spec, error) {
	return plan.Spec(), nil
}

// managedLiveFiles 返回本 phase 已知受管、但当前期望集合里不存在的本机文件。
// desired 快照只覆盖 render.DefaultBuilders 的四个 builder，network/local-proxy
// 的文件不在其中；因此切换 plugin 或删除时，旧文件不会出现在 previousSpec 里。
// 这里按“已知受管路径 + 磁盘存在”补齐，保证旧 CNI/haproxy 文件能被判定为删除。
func (p *Phase) managedLiveFiles(c *phase.Context, spec *render.Spec) []render.File {
	if p.Spec != nil {
		return nil
	}
	rc := c.RenderContext()
	paths := []struct {
		path      string
		component string
	}{
		{network.CNIConfigPath(rc), constants.ComponentKubeRouter},
	}
	out := make([]render.File, 0, len(paths))
	for _, item := range paths {
		if item.path == "" {
			continue
		}
		if _, exists := spec.Find(item.path); exists {
			continue
		}
		if info, err := os.Stat(item.path); err != nil || info.IsDir() {
			continue
		}
		out = append(out, render.File{
			Path:      item.path,
			Component: item.component,
			Kind:      render.KindOther,
		})
	}
	return out
}

// nodeCIDR 返回本节点的 Pod 子网。
//
// 权威值只有 Node.spec.podCIDR（kube-controller-manager 分配）。已安装节点
// （node.yaml 存在）与 join 节点读不到时必须失败：此时按 index=1 推导会覆盖
// 已分配的 podCIDR，导致多节点子网重叠，而且还会重启 containerd 让错误立即
// 生效。只有首次 init 允许返回空（由 network.Build 推导一个临时值，安装收尾
// 的 desired 快照会以真实分配为准，后续 repair 会纠正）。
func (p *Phase) nodeCIDR(ctx context.Context, c *phase.Context) (string, error) {
	host, ok := c.Self()
	if !ok || host == nil || !host.IsWorker() {
		return "", nil
	}
	strict := c.Node != nil || c.JoinSecret != nil
	client, err := p.kubeClient(c)
	if err != nil {
		if strict {
			return "", fmt.Errorf("读取本机 Node %s 的 podCIDR 失败：%w", c.NodeName(), err)
		}
		return "", nil
	}
	node, err := client.CoreV1().Nodes().Get(ctx, c.NodeName(), metav1.GetOptions{})
	if err != nil {
		if strict {
			return "", fmt.Errorf("读取本机 Node %s 的 podCIDR 失败（network phase 必须在节点注册后执行）：%w", c.NodeName(), err)
		}
		return "", nil
	}
	cidr := strings.TrimSpace(node.Spec.PodCIDR)
	if cidr == "" && strict {
		return "", fmt.Errorf("本机 Node %s 还没有分配 podCIDR：确认 kube-controller-manager 正常运行且 --allocate-node-cidrs=true", c.NodeName())
	}
	return cidr, nil
}

// Check 报告网络层是否已应用且健康。
// 判据：
// 1. plugin=kube-router 时 CNI 配置文件存在；
// 2. desired 文件与 live 一致；
// 3. kube-router（20244）与 kube-proxy（10256）的健康端点可达。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	plan, err := p.plan(ctx, c)
	if err != nil {
		return false, err
	}
	checker := p.checker()
	spec, err := p.desiredSpec(plan)
	if err != nil {
		return false, err
	}
	ok, err := checker.CheckLocalFiles(spec)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := checker.CheckHealth(ctx, plan.Healths); err != nil {
		c.Reporter().Debugf("网络组件未就绪：%v", err)
		return false, nil
	}
	// 旧一代的 CNI 文件（例如 plugin: kube-router -> none）不在当前 spec 里，
	// Drift/CheckLocalFiles 看不见；必须显式检查受管路径是否还有残留。
	if stale := p.managedLiveFiles(c, spec); len(stale) > 0 {
		for _, f := range stale {
			c.Reporter().Debugf("网络文件需要删除：%s", f.Path)
		}
		return false, nil
	}
	// 集群级对象（kube-router/kube-proxy DaemonSet/ConfigMap/RBAC）只有 master 维护；
	// Check 必须同时比较本机文件、健康、集群对象与已删除对象的残留。
	if host, ok := c.Self(); ok && host.IsMaster() {
		client, err := p.kubeClient(c)
		if err != nil {
			if len(plan.Objects) == 0 {
				// 没有对象要维护，也没有客户端可查残留；本机层已完成。
				return true, nil
			}
			c.Reporter().Debugf("网络对象 Check 缺少 apiserver 客户端：%v", err)
			return false, nil
		}
		stale, err := p.staleObjects(ctx, client, plan.Objects)
		if err != nil {
			return false, err
		}
		if len(stale) > 0 {
			for _, obj := range stale {
				c.Reporter().Debugf("网络对象需要删除：%s", obj.Ref())
			}
			return false, nil
		}
		for _, obj := range plan.Objects {
			matched, err := clusterapply.Matches(ctx, client, obj)
			if err != nil {
				return false, err
			}
			if !matched {
				c.Reporter().Debugf("网络对象需要更新：%s", obj.Ref())
				return false, nil
			}
		}
	}
	return true, nil
}

// Run 应用本节点的网络层。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	plan, err := p.plan(ctx, c)
	if err != nil {
		return err
	}
	report := c.Reporter()

	// 1) 本机文件：与其它 phase 一样先比较再落盘。
	// 快照是整机共享的：必须先按"本 phase 负责的组件"过滤，否则会把控制面清单、
	// kubelet 配置等别的组件的文件判成"应删除"并真的删掉（真机实测：刚装好的
	// 单节点集群在 network phase 结束时被删掉 manifests/*，etcd 立即被 kubelet 杀掉）。
	spec, err := p.desiredSpec(plan)
	if err != nil {
		return err
	}
	old := render.SelectComponents(c.PreviousSpec(), spec)
	if old == nil {
		old = &render.Spec{Node: c.NodeName()}
	}
	old.Files = append(old.Files, p.managedLiveFiles(c, spec)...)
	old.Sort()
	changes := render.ComputeChanges(render.OverlayLive(old, spec), spec)
	if c.DryRun {
		// dry-run 仍然列出集群对象（reconcileObjects 内部只打印，不写 apiserver）。
		if err := p.reconcileObjects(ctx, c, plan.Objects); err != nil {
			return err
		}
		report.Infof("将应用 %d 个本机文件变更（dry-run）", len(changes))
		for _, ch := range changes {
			report.Infof("  %s %s", ch.Kind, ch.File.Path)
		}
		p.reportRestart(report, changes)
		return nil
	}
	_, err = render.ApplyChanges(changes, c.Paths, c.Clock(), report)
	if err != nil {
		return fmt.Errorf("应用网络配置失败：%w", err)
	}
	for _, ch := range changes {
		if ch.Kind == render.ChangeRemoved {
			report.Infof("已删除网络文件 %s", ch.File.Path)
		}
	}
	if len(changes) > 0 {
		report.Infof("网络配置已更新：%d 个文件", len(changes))
	}
	// CNI 配置落盘后必须让 containerd 重新初始化 CNI：它只在启动时读配置，
	// 启动时没有配置就会一直报 "cni plugin not initialized"，节点永远 NotReady。
	if cniConfigChanged(changes) {
		if err := p.reloadRuntime(ctx); err != nil {
			// 不阻断安装：配置已经落盘，最坏情况是运维手工重启一次 containerd。
			report.Warnf("重启容器运行时以加载 CNI 配置失败：%v（可手工执行 systemctl restart containerd）", err)
		} else {
			report.Infof("已重启容器运行时（containerd）以加载新的 CNI 配置")
		}
	}
	// 应用 CNI（kube-router）与 Service 代理（kube-proxy）对象，并删除
	// 上一代配置留下、当前不再需要的对象。
	if err := p.reconcileObjects(ctx, c, plan.Objects); err != nil {
		return err
	}
	p.reportRestart(report, changes)
	return nil
}

// cniConfigChanged 报告本次运行是否写入了 CNI 配置文件。
func cniConfigChanged(changes []render.FileChange) bool {
	for _, ch := range changes {
		if ch.Kind == render.ChangeRemoved {
			continue
		}
		if filepath.Base(ch.File.Path) == network.CNIConfigFileName {
			return true
		}
	}
	return false
}

// reloadRuntime 重启 containerd，让它加载 CNI 配置。
// 没有 systemd，或 containerd 没在运行时，保持空操作。
func (p *Phase) reloadRuntime(ctx context.Context) error {
	if p.RestartRuntime != nil {
		return p.RestartRuntime(ctx)
	}
	mgr := systemd.Manager{Timeout: 30 * time.Second}
	active, err := mgr.IsActive(ctx, "containerd")
	if err != nil || !active {
		return nil
	}
	return mgr.Action(ctx, "containerd", "restart")
}

// listManaged 按归属标签列出 kubemono 管理的网络对象。
// 不再维护本地对象账本：对象自己带标签，prune 的依据在集群里，
// 节点重装/状态文件丢失都不会留下“记得删但不知道删什么”的死角。
func (p *Phase) listManaged(ctx context.Context, client kubernetes.Interface) ([]render.Object, error) {
	opts := metav1.ListOptions{LabelSelector: network.ManagedSelector()}
	var out []render.Object

	sas, err := client.CoreV1().ServiceAccounts(constants.KubeSystemNamespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for i := range sas.Items {
		out = append(out, render.NewClusterObject("ServiceAccount", constants.KubeSystemNamespace, sas.Items[i].Name, &sas.Items[i]))
	}
	cms, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for i := range cms.Items {
		out = append(out, render.NewClusterObject("ConfigMap", constants.KubeSystemNamespace, cms.Items[i].Name, &cms.Items[i]))
	}
	dss, err := client.AppsV1().DaemonSets(constants.KubeSystemNamespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for i := range dss.Items {
		out = append(out, render.NewClusterObject("DaemonSet", constants.KubeSystemNamespace, dss.Items[i].Name, &dss.Items[i]))
	}
	crs, err := client.RbacV1().ClusterRoles().List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for i := range crs.Items {
		out = append(out, render.NewClusterObject("ClusterRole", "", crs.Items[i].Name, &crs.Items[i]))
	}
	crbs, err := client.RbacV1().ClusterRoleBindings().List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for i := range crbs.Items {
		out = append(out, render.NewClusterObject("ClusterRoleBinding", "", crbs.Items[i].Name, &crbs.Items[i]))
	}
	return out, nil
}

// staleObjects 返回集群里带归属标签、但当前 plan 不再需要的对象。
func (p *Phase) staleObjects(ctx context.Context, client kubernetes.Interface, current []render.Object) ([]render.Object, error) {
	managed, err := p.listManaged(ctx, client)
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(current))
	for _, o := range current {
		keep[o.Ref()] = true
	}
	var out []render.Object
	for _, o := range managed {
		if !keep[o.Ref()] {
			out = append(out, o)
		}
	}
	return out, nil
}

// reconcileObjects 应用当前对象，并按标签删除上一代残留。
func (p *Phase) reconcileObjects(ctx context.Context, c *phase.Context, objects []render.Object) error {
	report := c.Reporter()
	if p.Client == nil && !c.IsMaster() {
		// 集群级对象由 master 维护；worker 上只有节点身份凭据，写 DaemonSet/RBAC
		// 必然被拒绝，因此明确跳过并说明原因。本机文件照常落盘。
		if len(objects) > 0 {
			report.Infof("本节点不是 master：跳过 %d 个集群级对象（%s），由 master 维护", len(objects), clusterapply.JoinRefs(objects))
		}
		return nil
	}
	client, err := p.kubeClient(c)
	if err != nil {
		if len(objects) == 0 {
			// plugin=none 且 kubeProxy 关闭时没有对象要写；没有客户端也无法
			// 查询残留，交给有 admin 凭据的 master 处理。
			report.Debugf("没有集群级对象需要应用，跳过网络对象清理：%v", err)
			return nil
		}
		return fmt.Errorf(
			"网络层需要集群级对象（%s），但当前没有可用的 apiserver 客户端（%v）；请在一台 master 上使用 admin kubeconfig 执行（例如 kubemono node repair --kubeconfig /etc/kubernetes/admin.conf）",
			clusterapply.JoinRefs(objects), err)
	}
	stale, err := p.staleObjects(ctx, client, objects)
	if err != nil {
		return fmt.Errorf("列出 kubemono 管理的网络对象失败：%w", err)
	}
	if c.DryRun {
		for _, o := range objects {
			report.Infof("将应用集群对象 %s（dry-run）", o.Ref())
		}
		for _, o := range stale {
			report.Infof("将删除集群对象 %s（dry-run）", o.Ref())
		}
		return nil
	}
	for _, o := range objects {
		changed, err := clusterapply.Apply(ctx, client, o)
		if err != nil {
			return fmt.Errorf("应用集群对象 %s 失败：%w", o.Ref(), err)
		}
		if changed {
			report.Infof("已应用集群对象 %s", o.Ref())
		} else {
			report.Debugf("集群对象 %s 已是最新", o.Ref())
		}
	}
	// 删除顺序与创建相反：先删工作负载，再删 RBAC。
	for i := len(stale) - 1; i >= 0; i-- {
		if err := clusterapply.Delete(ctx, client, stale[i]); err != nil {
			return fmt.Errorf("删除遗留集群对象 %s 失败：%w", stale[i].Ref(), err)
		}
		report.Infof("已删除遗留集群对象 %s", stale[i].Ref())
	}
	return nil
}

// reportRestart 把重启/重载决定写入日志。
func (p *Phase) reportRestart(report output.Reporter, changes []render.FileChange) {
	actions := render.PendingActions(changes)
	for component, action := range actions {
		switch action {
		case render.ActionRecreate:
			report.Infof("%s 配置变化：清单已替换，将由容器运行时重建", component)
		case render.ActionRestart:
			report.Infof("%s 配置变化：需要重启组件", component)
		case render.ActionReload:
			report.Infof("%s 配置变化：热加载即可", component)
		}
	}
}

// checker 返回可注入的探测实现。
func (p *Phase) checker() NetworkChecker {
	if p.Checker != nil {
		return p.Checker
	}
	return &defaultChecker{}
}

// defaultChecker 是生产环境的探测实现。
type defaultChecker struct{}

// CheckLocalFiles 实现 NetworkChecker。
func (d *defaultChecker) CheckLocalFiles(spec *render.Spec) (bool, error) {
	drift, err := render.Drift(spec)
	if err != nil {
		return false, err
	}
	if len(drift) > 0 {
		return false, nil
	}
	// plugin=kube-router 时 CNI 配置必须存在；plugin=none 时它不出现在
	// desired 里，ComputeChanges 会把它标成 Removed。
	for _, f := range spec.Files {
		if filepath.Base(f.Path) != network.CNIConfigFileName {
			continue
		}
		if _, err := os.Stat(f.Path); err != nil {
			return false, nil
		}
	}
	return true, nil
}

// CheckHealth 实现 NetworkChecker。
func (d *defaultChecker) CheckHealth(ctx context.Context, healths []network.Health) error {
	if len(healths) == 0 {
		return nil
	}
	checker := controlplane.NewHTTPChecker()
	for _, h := range healths {
		err := checker.Check(ctx, controlplane.HealthCheck{
			Scheme:  h.Scheme,
			Address: h.Address,
			Path:    h.Path,
		})
		if err != nil {
			return err
		}
	}
	return nil
}
