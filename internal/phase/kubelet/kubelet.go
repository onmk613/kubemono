// Package kubelet 实现 `kubelet` phase（、、、
// 、 第 10 步）。
// 只有 worker 角色（含 [master, worker] 混合节点）运行 kubelet；纯控制面 master 没有。phase 只做
// 编排，具体能力来自 internal/kubelet：
//
//	安装二进制 → 应用渲染文件 → 启用/启动 unit → 等待就绪 → 告警待审批 CSR
//
// "就绪"按角色区分（见 wait）：master 的 kubelet 先于 etcd/控制面启动，只等本机
// healthz；worker 等 TLS Bootstrap 完成并注册（CNI 尚未部署导致的 NotReady 不算失败）。
// 角色差异只有一处：master 的 kubelet.conf 由 kubeconfig 阶段直签，worker 的
// kubelet.conf 由 kubelet 用 bootstrap-kubeconfig 完成 TLS Bootstrap 后自己写出，
// 因此 worker 必须多等一个“kubelet.conf 出现”的条件。
// 幂等与安全：
// - Check 为真时 Runner 会直接跳过本 phase；
// - Run 只写入与期望不一致的文件，失败时回滚本 phase 写过的文件并尽力重启；
// - DryRun 不写文件、不动服务，只报告将要做的动作。
package kubelet

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"kubemono/internal/constants"
	controlplane "kubemono/internal/controlplane"
	"kubemono/internal/execx"
	kubeletpkg "kubemono/internal/kubelet"
	"kubemono/internal/loadbalancer"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	"kubemono/internal/systemd"
	"kubemono/internal/util/fsutil"
)

// PhaseName 是 `kubelet` phase 的名称。
const PhaseName = "kubelet"

// restartMarkerName 是“kubelet 文件已写、尚未成功重启”的标记名。
const restartMarkerName = "kubelet"

// DefaultLocalProxyReadyTimeout 是切换 kubelet 到本地代理前等待 haproxy 就绪的上限。
// haproxy 是 kubelet 管理的静态 Pod：kubelet 尚未切到本地代理时仍能直连 master
// 启动它；但切过去之后若 haproxy 没起来，kubelet 就再也连不上 apiserver。
const DefaultLocalProxyReadyTimeout = 2 * time.Minute

func init() { phase.MustRegister(&Phase{}) }

// Phase 安装并启动本节点的 kubelet。
type Phase struct {
	// Spec 覆盖渲染出的期望状态（测试注入）。
	Spec *render.Spec
	// ServiceManager 控制 kubelet.service；为 nil 时构造 systemd 实现。
	ServiceManager systemd.ServiceManager
	// NodeClient 上报 Node 就绪状态；为 nil 时用节点的 kubeconfig 构造 client-go
	// 实现。
	NodeClient kubeletpkg.NodeClient
	// Installer 安装 kubelet 二进制；为 nil 时使用默认安装器。
	Installer *kubeletpkg.BinaryInstaller
	// WaitKnobs 统一等待循环的超时/间隔/睡眠/时钟注入（见 phase.WaitKnobs）。
	phase.WaitKnobs
	// Health 探测 kubelet 健康端点，用于诊断。
	Health controlplane.HealthChecker

	// NewNodeClient 按上下文构造 Node 就绪客户端；测试使用。
	NewNodeClient func(*phase.Context) (kubeletpkg.NodeClient, error)

	mu      sync.Mutex
	applied []render.AppliedChange
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 表示本节点是否运行 kubelet：worker（含 master+worker）才运行；
// 纯控制面 master 不装 kubelet，也不注册 Node。
func (p *Phase) Applies(c *phase.Context) bool {
	host, ok := c.Self()
	return ok && host.IsWorker()
}

// desiredSpec 渲染（或返回注入的）期望状态。
func (p *Phase) desiredSpec(c *phase.Context) (*render.Spec, error) {
	if c.Cluster == nil || c.Host == nil {
		return nil, fmt.Errorf("缺少集群配置或节点身份，无法渲染 kubelet 的期望状态")
	}
	return phase.DesiredSpec(c, p.Spec)
}

// Check 表示 kubelet 是否已安装、运行并完成注册。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	if pending, err := phase.HasPendingRestart(c, restartMarkerName); err != nil {
		return false, err
	} else if pending {
		c.Reporter().Debugf("kubelet 有待完成的重启，需要重跑")
		return false, nil
	}
	spec, err := p.desiredSpec(c)
	if err != nil {
		return false, err
	}
	if len(kubeletpkg.DesiredFiles(spec)) == 0 {
		return false, fmt.Errorf("渲染结果里没有 kubelet 的文件：请检查 internal/render 的 kubelet builder 是否适用于本节点")
	}
	if _, ok := c.Self(); !ok {
		return false, fmt.Errorf("无法确定本机在集群配置中的条目，kubelet 需要节点身份")
	}
	installer, err := p.installer(c)
	if err != nil {
		return false, err
	}
	sm, err := p.serviceManager(c)
	if err != nil {
		c.Reporter().Debugf("构造 ServiceManager 失败：%v", err)
		sm = nil
	}
	nc, err := p.nodeClient(c)
	if err != nil {
		// worker 在 TLS Bootstrap 完成前没有可用的 kubeconfig，这属于“还没完成”。
		c.Reporter().Debugf("尚未取得 Kubernetes 客户端（%v），按未完成处理", err)
		return false, nil
	}
	done, err := kubeletpkg.Check(ctx, kubeletpkg.CheckOptions{
		Paths:          c.Paths,
		Spec:           spec,
		Node:           c.NodeName(),
		Installer:      installer,
		ServiceManager: sm,
		NodeClient:     nc,
		Report:         func(format string, args ...any) { c.Reporter().Debugf(format, args...) },
	})
	if err != nil {
		return false, err
	}
	if done && p.needLocalProxySwitch(c) {
		c.Reporter().Debugf("kubelet 仍在用引导入口，未切换到本地代理；下次 repair 会重试")
		return false, nil
	}
	return done, nil
}

// Run 安装二进制、应用期望文件、（重新）启动 unit，并等到节点 Ready。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	report := c.Reporter()
	spec, err := p.desiredSpec(c)
	if err != nil {
		return err
	}
	if len(kubeletpkg.DesiredFiles(spec)) == 0 {
		return fmt.Errorf("渲染结果里没有 kubelet 的文件：请检查 internal/render 的 kubelet builder 是否适用于本节点")
	}
	host, ok := c.Self()
	if !ok {
		return fmt.Errorf("无法确定本机在集群配置中的条目，kubelet 需要节点身份")
	}
	installer, err := p.installer(c)
	if err != nil {
		return err
	}
	sm, err := p.serviceManager(c)
	if err != nil {
		return err
	}

	// 1. 二进制：kubelet 由 systemd 运行，必须安装。
	if c.DryRun {
		report.Infof("将安装 kubelet 二进制（release %s）到 %s", c.Release, kubeletpkg.BinaryInstallPath)
	} else {
		dir, err := installer.Install(ctx, constants.ComponentKubelet)
		if err != nil {
			return err
		}
		report.Infof("kubelet 二进制已就绪：%s", dir)
	}

	// 2. 期望文件：只写本 phase 拥有的 kubelet 文件，改动前先备份。
	// 其他组件（containerd、控制面……）的文件由各自的 phase 负责。
	kSpec := kubeletSpec(spec)
	changes, err := p.changes(c, kSpec)
	if err != nil {
		return err
	}
	pendingRestart, err := phase.HasPendingRestart(c, restartMarkerName)
	if err != nil {
		return err
	}
	if !c.DryRun && (len(changes) > 0 || pendingRestart) {
		// 先落标记再写文件：kubelet 只在启动时读 kubeconfig/unit，
		// 写完未重启的窗口必须让下一次 Check 看见。
		if err := phase.WriteRestartMarker(c, restartMarkerName, []string{kubeletpkg.UnitName},
			"kubelet 文件已更新，等待 kubelet 重启"); err != nil {
			return err
		}
	}
	if c.DryRun {
		if len(changes) == 0 {
			report.Infof("kubelet 文件与期望一致，无需变更（dry-run）")
		}
		for _, ch := range changes {
			report.Infof("将写入 %s（%04o，%s，dry-run）", ch.File.Path, ch.File.Mode, ch.File.OnChange)
		}
		return nil
	}
	if len(changes) == 0 {
		report.Infof("kubelet 文件与期望一致，无需变更")
	} else {
		applied, err := render.ApplyChanges(changes, c.Paths, c.Clock(), report)
		p.remember(applied)
		if err != nil {
			rbErr := p.rollbackFiles(c, sm)
			if rbErr != nil {
				return fmt.Errorf("写入 kubelet 文件失败: %w；回滚也失败: %v", err, rbErr)
			}
			return fmt.Errorf("写入 kubelet 文件失败（已回滚）: %w", err)
		}
	}

	if err := p.ensureService(ctx, c, sm, spec, len(changes) > 0 || pendingRestart); err != nil {
		return err
	}

	// 4. 等待就绪：master 只等本机 healthz，worker 等 TLS Bootstrap 与注册。
	if err := p.wait(ctx, c, c.NodeName(), host.IsMaster()); err != nil {
		return err
	}

	// 5. worker 的 kubelet 在引导阶段必须直连 master（本地代理还没起来），
	// 但就绪后应改走 127.0.0.1:6443，否则 master 故障时 worker 失去 HA 能力。
	// kubelet 只在启动时读 kubeconfig 的 server，改文件后必须重启才生效
	// （真机实测：只改文件时 kubelet 仍连 10.8.2.101，m1 故障后 w1 NotReady）。
	if p.needLocalProxySwitch(c) {
		// 关键顺序：先确认 haproxy 已经监听 127.0.0.1:6443，再改 kubelet.conf。
		// 反过来的话，kubelet 重启后会通过尚未就绪的本地代理连 apiserver，
		// 既无法上报 Ready，也无法再拉起 haproxy 静态 Pod（死锁）。
		if err := p.waitLocalProxyReady(ctx, c); err != nil {
			c.Reporter().Warnf("本地代理未就绪，kubelet 保持直连 master，未切换；修复后重跑 node repair: %v", err)
		} else if p.switchKubeletToLocalProxy(c) {
			if err := sm.Restart(ctx, kubeletpkg.UnitName); err != nil {
				return fmt.Errorf("切换 kubelet 到本地代理后重启失败: %w", err)
			}
			c.Reporter().Infof("kubelet 已重启以使用本地代理入口")
			if err := p.wait(ctx, c, c.NodeName(), host.IsMaster()); err != nil {
				return fmt.Errorf("切换本地代理后等待 kubelet 就绪失败: %w", err)
			}
		}
	}

	// 6. worker 完成 TLS Bootstrap 后 bootstrap-kubelet.conf 不再需要；
	// 它含明文 join token，2h 有效期内落到备份/镜像里就是可用凭证。
	if err := p.cleanupBootstrapKubeconfig(c); err != nil {
		report.Warnf("清理 bootstrap-kubelet.conf 失败：%v", err)
	}
	if !c.DryRun {
		if err := phase.ClearRestartMarker(c, restartMarkerName); err != nil {
			return err
		}
	}

	// 7. serving CSR 由 `kubectl certificate approve` 审批，
	// 这里只告警。
	p.warnPendingCSRs(ctx, c)
	return nil
}

// cleanupBootstrapKubeconfig 删除 worker 的引导 kubeconfig。
// 只在 kubelet.conf 已经以 system:node:<节点名> 身份存在时删除；否则保留，
// 让下一次 repair 还能继续 TLS Bootstrap。
func (p *Phase) cleanupBootstrapKubeconfig(c *phase.Context) error {
	host, ok := c.Self()
	if !ok || host.IsMaster() {
		return nil
	}
	bootstrap := c.Paths.KubeconfigPath(constants.BootstrapKubeletKubeconfigName)
	if !fsutil.FileExists(bootstrap) {
		return nil
	}
	kubeletConf := c.Paths.KubeconfigPath(constants.KubeletKubeconfigName)
	cfg, err := clientcmd.LoadFromFile(kubeletConf)
	if err != nil {
		return nil
	}
	current := cfg.Contexts[cfg.CurrentContext]
	if current == nil || current.AuthInfo != "system:node:"+c.NodeName() {
		return nil
	}
	if err := os.Remove(bootstrap); err != nil && !os.IsNotExist(err) {
		return err
	}
	c.Reporter().Infof("已删除 %s（TLS Bootstrap 已完成，避免残留 join token）", bootstrap)
	return nil
}

func (p *Phase) ensureService(ctx context.Context, c *phase.Context, sm systemd.ServiceManager, spec *render.Spec, restartNeeded bool) error {
	report := c.Reporter()
	unitContent, hasUnit := kubeletpkg.UnitFile(spec, c.Paths)
	if hasUnit {
		if err := sm.Enable(ctx, kubeletpkg.UnitName); err != nil {
			return p.failKubelet(c, sm, fmt.Errorf("启用 %s 失败: %w", kubeletpkg.UnitName, err))
		}
		// 文件没变且服务在运行时不重启：install 的幂等重跑（ActionSync）不应
		// 无谓地打断 kubelet。
		active, activeErr := sm.IsActive(ctx, kubeletpkg.UnitName)
		if restartNeeded || activeErr != nil || !active {
			if err := sm.Restart(ctx, kubeletpkg.UnitName); err != nil {
				return p.failKubelet(c, sm, fmt.Errorf("启动 %s 失败: %w", kubeletpkg.UnitName, err))
			}
			report.Infof("%s 已启动（%d 字节的 unit 来自渲染结果）", kubeletpkg.UnitName, len(unitContent))
		} else {
			report.Debugf("%s 已在运行且文件未变化，不重启", kubeletpkg.UnitName)
		}
		return nil
	}
	if !restartNeeded {
		return nil
	}
	if active, err := sm.IsActive(ctx, kubeletpkg.UnitName); err == nil && active {
		if err := sm.Restart(ctx, kubeletpkg.UnitName); err != nil {
			report.Warnf("配置已更新，但重启 %s 失败：%v", kubeletpkg.UnitName, err)
		} else {
			report.Infof("配置已更新，已重启 %s", kubeletpkg.UnitName)
		}
	} else if err != nil {
		report.Debugf("无法查询 %s 状态（%v）", kubeletpkg.UnitName, err)
	}
	return nil
}

// failKubelet 在服务动作失败时回滚已写文件，并把回滚结果拼进错误。
func (p *Phase) failKubelet(c *phase.Context, sm systemd.ServiceManager, err error) error {
	if rbErr := p.rollbackFiles(c, sm); rbErr != nil {
		return fmt.Errorf("%w；回滚也失败: %v", err, rbErr)
	}
	return fmt.Errorf("%w（已回滚）", err)
}

// needLocalProxySwitch 报告 worker 的 kubelet.conf 是否还没指向本地代理。
// 读不到 kubelet.conf（TLS Bootstrap 尚未完成）时返回 false，由正常等待流程处理。
func (p *Phase) needLocalProxySwitch(c *phase.Context) bool {
	if c == nil || c.Cluster == nil || !c.Cluster.ControlPlane.LocalProxyEnabled() {
		return false
	}
	host, ok := c.Self()
	if !ok || host.IsMaster() {
		return false
	}
	path := c.Paths.KubeconfigPath(constants.KubeletKubeconfigName)
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil || cfg.CurrentContext == "" || cfg.Contexts[cfg.CurrentContext] == nil {
		return false
	}
	cl, ok := cfg.Clusters[cfg.Contexts[cfg.CurrentContext].Cluster]
	if !ok || cl == nil {
		return false
	}
	want := fmt.Sprintf("https://127.0.0.1:%d", constants.DefaultLocalProxyPort)
	return cl.Server != want
}

// waitLocalProxyReady 等 haproxy 的统计端点可达。haproxy 是 kubelet 管理的
// 静态 Pod，必须先由直连 master 的 kubelet 拉起；它监听 127.0.0.1:6443 后
// 才能切换 kubelet.conf，否则重启后的 kubelet 会失去 apiserver 连接。
func (p *Phase) waitLocalProxyReady(ctx context.Context, c *phase.Context) error {
	address := fmt.Sprintf("127.0.0.1:%d", loadbalancer.LocalProxyStatsPort)
	err := phase.Poll(ctx, p.PollOptions(c, DefaultLocalProxyReadyTimeout, 2*time.Second), func() (bool, error) {
		checkErr := p.health().Check(ctx, controlplane.HealthCheck{
			Scheme: "http", Address: address, Path: "/stats",
		})
		return checkErr == nil, checkErr
	})
	if err != nil {
		return fmt.Errorf("本地代理 %s/stats 未就绪: %w", address, err)
	}
	return nil
}

// switchKubeletToLocalProxy 在引导完成后把 worker 的 kubelet kubeconfig 改指
// 本地代理。文件发生变化时返回 true；调用方必须重启 kubelet，因为 server URL
// 只在启动时读取。
func (p *Phase) switchKubeletToLocalProxy(c *phase.Context) bool {
	if c == nil || c.Cluster == nil || !c.Cluster.ControlPlane.LocalProxyEnabled() {
		return false
	}
	host, ok := c.Self()
	if !ok || host.IsMaster() {
		return false
	}
	path := c.Paths.KubeconfigPath(constants.KubeletKubeconfigName)
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		c.Reporter().Warnf("读取 %s 失败，保持引导入口：%v", path, err)
		return false
	}
	if cfg.CurrentContext == "" || cfg.Contexts[cfg.CurrentContext] == nil {
		return false
	}
	clusterName := cfg.Contexts[cfg.CurrentContext].Cluster
	cl, ok := cfg.Clusters[clusterName]
	if !ok || cl == nil {
		return false
	}
	want := fmt.Sprintf("https://127.0.0.1:%d", constants.DefaultLocalProxyPort)
	if cl.Server == want {
		return false
	}
	cl.Server = want
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		c.Reporter().Warnf("改写 %s 指向本地代理失败：%v", path, err)
		return false
	}
	c.Reporter().Infof("worker kubelet.conf 已改指本地代理 %s", want)
	return true
}

// kubeletSpec 把节点渲染出的 spec 投影到 kubelet 组件所拥有的文件上。
func kubeletSpec(spec *render.Spec) *render.Spec {
	return &render.Spec{
		Node:      spec.Node,
		ClusterID: spec.ClusterID,
		Release:   spec.Release,
		Files:     kubeletpkg.DesiredFiles(spec),
	}
}

// changes 把实际状态与期望的 kubelet 文件做比较。
// old 由两部分合成：本机 desired 快照里的 kubelet 文件（用于发现“这一代不
// 需要、必须删除的文件”，快照由 install/repair 维护）+ 磁盘上的真实内容
// （用于发现漂移）。两者都有的文件以真实内容为准，这样漂移的文件也会被重写，
// 而不是因为“快照没变”被跳过。
func (p *Phase) changes(c *phase.Context, spec *render.Spec) ([]render.FileChange, error) {
	base, err := render.NewStore(c.Paths).Restore()
	if err != nil {
		c.Reporter().Debugf("读取 desired 快照失败（按首次安装处理）：%v", err)
		base = nil
	}
	if base != nil {
		base = kubeletSpec(base)
	}
	// 与 live 合并是唯一实现（internal/render.OverlayLive），network phase 共用它：
	// 快照没变但磁盘被改过时也必须重写。
	return render.ComputeChanges(render.OverlayLive(base, spec), spec), nil
}

// wait 阻塞到 kubelet 可用为止。
// master：kubelet 先于 etcd/控制面启动，此时没有 apiserver，只等本机
// healthz；注册由 mark-control-plane 确认。worker：集群已存在，等 TLS Bootstrap
// 完成并注册（只因 CNI 未就绪的 NotReady 视为完成，见 AllowNetworkNotReady）。
func (p *Phase) wait(ctx context.Context, c *phase.Context, node string, master bool) error {
	report := c.Reporter()
	knobs := p.WaitKnobs
	opts := kubeletpkg.WaitOptions{
		Node:      node,
		Master:    master,
		LocalOnly: master,
		Paths:     c.Paths,
		// Timeout 的默认在 phase 侧收敛；PollInterval 的默认（2s）由
		// kubeletpkg.WaitForBootstrap 自己兜底，这里原样透传。
		Timeout:      knobs.TimeoutOrDefault(kubeletpkg.DefaultBootstrapTimeout),
		PollInterval: knobs.Interval,
		Sleep:        knobs.ResolveSleep(),
		Now:          knobs.ResolveClock(c),
		Health:       p.health(),
		Report:       func(format string, args ...any) { report.Infof(format, args...) },
		NodeClient: func(context.Context) (kubeletpkg.NodeClient, error) {
			return p.nodeClient(c)
		},
	}
	if err := kubeletpkg.WaitForBootstrap(ctx, opts); err != nil {
		return fmt.Errorf("等待 kubelet 就绪失败: %w", err)
	}
	return nil
}

// warnPendingCSRs 报告尚未审批的 kubelet serving CSR；它从不使 phase 失败，
// 因为审批属于集群运维（`kubectl certificate approve`）。
func (p *Phase) warnPendingCSRs(ctx context.Context, c *phase.Context) {
	nc, err := p.nodeClient(c)
	if err != nil || nc == nil {
		return
	}
	n, err := nc.PendingServingCSRs(ctx)
	if err != nil {
		c.Reporter().Debugf("查询待审批 serving CSR 失败：%v", err)
		return
	}
	if n > 0 {
		c.Reporter().Warnf("有 %d 个 kubelet serving CSR 等待审批；由 `kubectl certificate approve` 处理", n)
	}
}

// rollbackFiles 恢复 Run 写过的文件并重启 unit。
func (p *Phase) rollbackFiles(c *phase.Context, sm systemd.ServiceManager) error {
	applied := p.takeApplied()
	if len(applied) == 0 {
		return nil
	}
	if err := render.Rollback(applied); err != nil {
		return fmt.Errorf("回滚 kubelet 文件失败: %w", err)
	}
	c.Reporter().Warnf("已把 kubelet 文件回滚到本阶段执行前的状态")
	if sm != nil {
		// 回滚可能发生在 ctx 被取消之后，这里用独立上下文尽力恢复服务。
		if err := sm.Restart(context.Background(), kubeletpkg.UnitName); err != nil {
			c.Reporter().Warnf("回滚后重启 %s 失败：%v", kubeletpkg.UnitName, err)
		}
	}
	return nil
}

func (p *Phase) remember(applied []render.AppliedChange) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applied = append(p.applied, applied...)
}

func (p *Phase) takeApplied() []render.AppliedChange {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.applied
	p.applied = nil
	return out
}

// installer 返回本 phase 的二进制安装器。
func (p *Phase) installer(c *phase.Context) (*kubeletpkg.BinaryInstaller, error) {
	if p.Installer != nil {
		return p.Installer, nil
	}
	return &kubeletpkg.BinaryInstaller{
		Paths:   c.Paths,
		Release: c.Release,
		BOM:     c.BOM,
		Report:  func(format string, args ...any) { c.Reporter().Debugf(format, args...) },
	}, nil
}

// serviceManager 返回本 phase 的服务管理器。
func (p *Phase) serviceManager(c *phase.Context) (systemd.ServiceManager, error) {
	if p.ServiceManager != nil {
		return p.ServiceManager, nil
	}
	return systemd.UnitController{Runner: execx.Default(), Report: c.Reporter(), Timeout: 30 * time.Second}, nil
}

// nodeClient 返回本 phase 的 Node 就绪客户端。
func (p *Phase) nodeClient(c *phase.Context) (kubeletpkg.NodeClient, error) {
	if p.NodeClient != nil {
		return p.NodeClient, nil
	}
	if p.NewNodeClient != nil {
		return p.NewNodeClient(c)
	}
	host, ok := c.Self()
	if !ok {
		return nil, fmt.Errorf("无法确定本机节点身份")
	}
	// master 用 admin.conf（kubeconfig 阶段已直签）；worker 用 kubelet.conf
	// （TLS Bootstrap 的产物）。两者都不能用 bootstrap 令牌身份读取 Node。
	name := constants.AdminKubeconfigName
	if !host.IsMaster() {
		name = constants.KubeletKubeconfigName
	}
	conf := c.Paths.KubeconfigPath(name)
	if !fsutil.FileExists(conf) {
		if host.IsMaster() {
			return nil, fmt.Errorf("缺少 %s：请先执行 `kubeconfig` phase", conf)
		}
		return nil, fmt.Errorf("缺少 %s：worker 需要先完成 TLS Bootstrap（确认引导令牌已 approved，kubectl get csr）", conf)
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", conf)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", conf, err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("创建 Kubernetes 客户端失败: %w", err)
	}
	nc := kubeletpkg.NewNodeClient(client, kubeletVersion(c))
	nc.Node = c.NodeName()
	// kubelet phase 先于 network phase：只因 CNI 未就绪的 NotReady 视为
	// kubelet 已完成，网络就绪由 network phase 与最终验收负责。
	nc.AllowNetworkNotReady = true
	return nc, nil
}

// kubeletVersion 返回 BOM 中的 kubelet 版本。
func kubeletVersion(c *phase.Context) string {
	if c.BOM == nil {
		return ""
	}
	return c.BOM.ComponentVersion(constants.ComponentKubelet)
}

func (p *Phase) health() controlplane.HealthChecker {
	if p.Health != nil {
		return p.Health
	}
	return controlplane.NewHTTPChecker()
}
