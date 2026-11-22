// Package runtime 实现 runtime phase：安装运行时、启动服务、等待 CRI。
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"kubemono/internal/artifact"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/hostinit"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	kubemonoruntime "kubemono/internal/runtime"
	"kubemono/internal/util/fsutil"
	"kubemono/internal/util/output"
)

// PhaseName 是 `runtime` phase 的名称。
const PhaseName = "runtime"

// restartMarkerName 是“运行时配置已写、尚未成功重启”的标记名。
const restartMarkerName = "runtime"

// 默认等待参数：containerd 首次启动要建 snapshotter 与 sandbox 镜像。
const (
	// DefaultTimeout 是等待 CRI 应答的上限。
	DefaultTimeout = 60 * time.Second
	// DefaultPollInterval 是两次探测之间的间隔。
	DefaultPollInterval = 2 * time.Second
)

func init() { phase.MustRegister(&Phase{}) }

// Phase 安装并配置本节点上的容器运行时。
type Phase struct {
	// ProviderFactory 完全接管 provider 的构造（测试注入）。
	ProviderFactory func(c *phase.Context) (kubemonoruntime.Provider, error)
	// ProviderOptions 在默认构造 provider 时叠加（测试可注入 Current、InstallDir 等）。
	ProviderOptions kubemonoruntime.Options
	// Commands 执行外部命令；nil 使用当前平台的默认实现。
	Commands kubemonoruntime.CommandRunner
	// Services 执行 systemd 动作；nil 使用当前平台的默认实现。
	Services kubemonoruntime.ServiceManager
	// Spec 覆盖渲染结果（测试注入）。
	Spec *render.Spec
	// WaitKnobs 统一等待循环的超时/间隔/睡眠/时钟注入（见 phase.WaitKnobs）。
	// 默认超时 DefaultTimeout、间隔 DefaultPollInterval。
	phase.WaitKnobs

	// last 是最近一次 Run 的计划。Run 是单线程的（phase runner 串行执行），
	// 不需要锁；Run 串行执行，last 只供测试观察。
	last *kubemonoruntime.Plan
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 实现 phase.Phase：只有 worker 角色才需要容器运行时
// （纯控制面 master 不装 kubelet/containerd/CNI，不跑业务 Pod）。
func (p *Phase) Applies(c *phase.Context) bool {
	host, ok := c.Self()
	return ok && host.IsWorker()
}

// Check 报告运行时、CNI 插件和受管配置是否就绪；未就绪返回 false。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	if pending, err := phase.HasPendingRestart(c, restartMarkerName); err != nil {
		return false, err
	} else if pending {
		c.Reporter().Debugf("运行时配置已写、等待重启，需要重跑")
		return false, nil
	}
	spec, err := p.desiredSpec(c)
	if err != nil {
		return false, err
	}
	prov, err := p.provider(c)
	if err != nil {
		return false, err
	}
	if missing := kubemonoruntime.MissingBinaries(c.Paths, p.ProviderOptions.InstallDir, kubemonoruntime.Binaries(runtimeSpec(c), c.BOM)); len(missing) > 0 {
		c.Reporter().Debugf("运行时未就绪：缺少二进制 %v", missing)
		return false, nil
	}
	if missing := kubemonoruntime.MissingCNIPlugins(c.Paths, c.BOM); len(missing) > 0 {
		c.Reporter().Debugf("运行时未就绪：缺少 CNI 插件 %v", missing)
		return false, nil
	}
	st, err := prov.Status(ctx)
	if err != nil {
		c.Reporter().Debugf("运行时未就绪：%v", err)
		return false, nil
	}
	if !st.Running {
		return false, nil
	}
	// containerd.service 不在 render.Spec 里（它是 runtime 包自己渲染的），
	// 所以这里必须单独比较 unit 内容并确认已 enable；否则改 unit 或手工
	// disable 后 repair 会静默跳过。
	if !runtimeSpec(c).IsCustom() {
		unitPath := c.Paths.Resolve(kubemonoruntime.UnitPath)
		want, err := kubemonoruntime.RenderUnit()
		if err != nil {
			return false, err
		}
		got, err := os.ReadFile(unitPath)
		if err != nil || string(got) != string(want) {
			c.Reporter().Debugf("containerd.service 内容与期望不一致或缺失，需要重写")
			return false, nil
		}
		if ec, ok := p.services().(interface {
			IsEnabled(context.Context, string) (bool, error)
		}); ok {
			enabled, err := ec.IsEnabled(ctx, kubemonoruntime.DefaultServiceName)
			if err != nil {
				return false, err
			}
			if !enabled {
				c.Reporter().Debugf("containerd.service 未设置开机自启，需要重跑")
				return false, nil
			}
		}
	}
	if want := kubemonoruntime.DesiredConfigHash(spec); want != "" && st.ConfigHash != want {
		c.Reporter().Debugf("运行时配置与期望不一致（实际 %s，期望 %s）", st.ConfigHash, want)
		return false, nil
	}
	if !p.imagesImported(c) {
		c.Reporter().Debugf("物料镜像尚未导入运行时")
		return false, nil
	}
	return true, nil
}

// imagesLayout 返回物料里的 OCI layout 目录；没有可导入的布局时返回空。
func imagesLayout(c *phase.Context) string {
	if c == nil || c.Release == "" || c.Paths.Kubemono() == "" {
		return ""
	}
	return filepath.Join(c.Paths.ArtifactDir(c.Release), "images")
}

// imagesState 记录“这个 release 的哪份归档、哪些镜像已经被导入”。
// 只按 release 记 sentinel 会在“同 release 重新出包/load”时静默跳过新镜像，
// 气隙节点直到 Pod 拉镜像失败才暴露。
type imagesState struct {
	ArchiveSHA256 string   `json:"archiveSHA256,omitempty"`
	References    []string `json:"references"`
}

// imagesSentinel 是镜像导入状态文件路径。
func imagesSentinel(c *phase.Context) string {
	return filepath.Join(c.Paths.State(), "images-"+c.Release+".json")
}

// currentImageState 返回当前物料归档摘要与镜像引用集合。
func currentImageState(c *phase.Context) (imagesState, error) {
	st := imagesState{}
	complete := filepath.Join(c.Paths.ArtifactDir(c.Release), artifact.CompleteName)
	if data, err := os.ReadFile(complete); err == nil {
		var loaded artifact.Complete
		if json.Unmarshal(data, &loaded) == nil {
			st.ArchiveSHA256 = loaded.SHA256
		}
	}
	dir := imagesLayout(c)
	if dir == "" {
		return st, nil
	}
	refs, err := artifact.LayoutReferences(dir)
	if err != nil {
		return imagesState{}, err
	}
	sort.Strings(refs)
	st.References = refs
	return st, nil
}

// imagesImported 报告物料镜像是否已经按当前归档/引用集合导入。
func (p *Phase) imagesImported(c *phase.Context) bool {
	dir := imagesLayout(c)
	if dir == "" || !fsutil.FileExists(filepath.Join(dir, artifact.OCILayoutFile)) {
		return true
	}
	data, err := os.ReadFile(imagesSentinel(c))
	if err != nil {
		return false
	}
	var recorded imagesState
	if json.Unmarshal(data, &recorded) != nil {
		return false
	}
	current, err := currentImageState(c)
	if err != nil {
		return false
	}
	if recorded.ArchiveSHA256 != current.ArchiveSHA256 {
		return false
	}
	if len(recorded.References) != len(current.References) {
		return false
	}
	for i := range recorded.References {
		if recorded.References[i] != current.References[i] {
			return false
		}
	}
	return true
}

// writeImagesState 记录本次成功导入的归档摘要与镜像引用。
func writeImagesState(c *phase.Context, refs []string) error {
	current, err := currentImageState(c)
	if err != nil {
		return err
	}
	current.References = append([]string(nil), refs...)
	sort.Strings(current.References)
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(imagesSentinel(c)), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(imagesSentinel(c), append(data, '\n'), 0o600)
}

// importBundleImages 在 containerd 就绪后导入物料里的镜像。
// 这是干净节点能离线 join 的关键：kubelet 在注册前需要 pause，worker 的
// 本地代理需要 haproxy，Service 代理需要 kube-proxy；这些镜像都在已导入的物料里，不能等 join 完成。
func (p *Phase) importBundleImages(ctx context.Context, c *phase.Context) error {
	dir := imagesLayout(c)
	if dir == "" || !fsutil.FileExists(filepath.Join(dir, artifact.OCILayoutFile)) {
		return nil
	}
	if runtimeSpec(c).IsCustom() {
		return fmt.Errorf("node.runtime.custom 下 kubemono 不会用 ctr 导入镜像；请把物料 %s 导入你自己的 CRI（例如 crictl/ctr --address），或改用 --images=false 的轻量包", dir)
	}
	if p.imagesImported(c) {
		c.Reporter().Debugf("物料镜像已按当前归档导入过（%s）", imagesSentinel(c))
		return nil
	}
	refs, err := artifact.LayoutReferences(dir)
	if err != nil {
		return fmt.Errorf("读取物料镜像清单失败：%w", err)
	}
	if len(refs) == 0 {
		c.Reporter().Debugf("物料 OCI layout 中没有镜像，跳过导入")
		return writeImagesState(c, nil)
	}
	c.Reporter().Infof("从物料导入 %d 个镜像（%s）", len(refs), dir)
	if err := artifact.LoadImages(ctx, dir, refs, "containerd", kubemonoruntime.CTRImageImporter{}); err != nil {
		return fmt.Errorf("导入物料镜像失败（kubelet/kube-proxy/haproxy/CoreDNS 将缺少镜像）：%w", err)
	}
	if err := writeImagesState(c, refs); err != nil {
		return fmt.Errorf("记录镜像导入状态失败：%w", err)
	}
	c.Reporter().Infof("已导入 %d 个离线镜像", len(refs))
	return nil
}

// Run 安装、配置并启动本节点的运行时。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	report := c.Reporter()
	spec, err := p.desiredSpec(c)
	if err != nil {
		return err
	}
	prov, err := p.provider(c)
	if err != nil {
		return err
	}

	finish := report.Step("安装运行时二进制")
	installErr := prov.Install(ctx)
	finish(installErr)
	if installErr != nil {
		return installErr
	}

	service := kubemonoruntime.ServiceName(runtimeSpec(c))
	pendingBefore := false
	if !c.DryRun {
		pendingBefore, err = phase.HasPendingRestart(c, restartMarkerName)
		if err != nil {
			return err
		}
	}
	if !c.DryRun {
		// 先落标记再写配置：ApplyConfig 内部是“写文件 → 重启”，进程若在
		// 两者之间被杀，下一次 Check 必须知道运行时还没加载新配置。
		changed, err := p.runtimeConfigChanged(c, spec)
		if err != nil {
			return err
		}
		if changed {
			if err := phase.WriteRestartMarker(c, restartMarkerName, []string{service},
				"运行时配置已更新，等待 containerd 重启"); err != nil {
				return err
			}
		}
	}
	plan, err := p.configure(ctx, prov, spec)
	if err != nil {
		return err
	}
	if plan != nil {
		p.last = plan
		reportDecision(report, plan.Decision)
		// 只改 certs.d 时 ApplyConfig 只做热加载、不重启；标记可以立即清掉。
		// 上一次“写配置后重启失败”留下的标记不能被 Reload/None 清掉：
		// 本次没有配置变化时 ApplyConfig 不会重启，必须由下面显式补一次 restart。
		if !pendingBefore && (plan.Decision == kubemonoruntime.DecisionReload || plan.Decision == kubemonoruntime.DecisionNone) {
			if err := phase.ClearRestartMarker(c, restartMarkerName); err != nil {
				return err
			}
		}
	}

	if c.DryRun {
		report.Infof("dry-run：跳过服务启动与 CRI 等待")
		return nil
	}

	services := p.services()
	// unit 文件刚写下去（或本次没变），必须先让 systemd 重新读取，否则
	// `systemctl start` 可能报 "Unit not found"。
	if err := services.DaemonReload(ctx); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败：%w；确认 /etc/systemd/system 下的 unit 语法正确", err)
	}
	if err := services.Action(ctx, service, "enable"); err != nil {
		return fmt.Errorf("设置 %s 开机自启失败：%w；确认 systemd 可用（systemctl enable %s）", service, err, service)
	}
	action := "start"
	if pendingBefore && (plan == nil || plan.Decision != kubemonoruntime.DecisionRestart) {
		// 配置已经写好但上一次重启失败：直接补一次 restart，start 对 active
		// unit 是 no-op，旧进程会一直用旧配置。
		action = "restart"
	}
	if err := services.Action(ctx, service, action); err != nil {
		return fmt.Errorf("%s %s 失败：%w；查看 journalctl -u %s 与 /var/lib/containerd 所在分区剩余空间", action, service, err, service)
	}
	if err := p.waitForRuntime(ctx, prov, c); err != nil {
		return err
	}
	if err := p.importBundleImages(ctx, c); err != nil {
		return err
	}
	if !c.DryRun {
		if err := phase.ClearRestartMarker(c, restartMarkerName); err != nil {
			return err
		}
	}
	report.Infof("运行时 %s 已就绪（socket %s）", service, kubemonoruntime.CRISocket(runtimeSpec(c)))
	if c.Cluster != nil {
		if err := hostinit.RunHooks(ctx, c.Cluster.Cluster.Hooks.PostRuntime, report.Infof); err != nil {
			return fmt.Errorf("postRuntime 钩子失败: %w", err)
		}
	}
	return nil
}

// runtimeConfigChanged 报告运行时受管配置是否与磁盘不同。
// 与 ApplyConfig 使用同一份 LiveSpec/ManagedFiles/ComputeChanges，避免
// “标记判断”和“实际应用”看到不同的差异集合。
func (p *Phase) runtimeConfigChanged(c *phase.Context, spec *render.Spec) (bool, error) {
	if runtimeSpec(c).IsCustom() {
		return false, nil
	}
	current, err := kubemonoruntime.LiveSpec(c.Paths)
	if err != nil {
		return false, err
	}
	desired := &render.Spec{Files: kubemonoruntime.ManagedFiles(spec)}
	return len(render.ComputeChanges(current, desired)) > 0, nil
}

// configure 应用运行时配置并拿到重启/热加载判定。
func (p *Phase) configure(ctx context.Context, prov kubemonoruntime.Provider, spec *render.Spec) (*kubemonoruntime.Plan, error) {
	return prov.ApplyConfig(ctx, spec)
}

// waitForRuntime 有界轮询运行时：unit active + socket/API 可用。
func (p *Phase) waitForRuntime(ctx context.Context, prov kubemonoruntime.Provider, c *phase.Context) error {
	opts := p.PollOptions(c, DefaultTimeout, DefaultPollInterval)
	timeout := opts.Timeout
	var lastErr error
	err := phase.Poll(ctx, opts, func() (bool, error) {
		st, err := prov.Status(ctx)
		if err != nil {
			lastErr = err
			return false, err
		}
		if st.Running {
			c.Reporter().Debugf("运行时就绪：%s", st.Message)
			return true, nil
		}
		lastErr = nil
		return false, nil
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}

	socket := kubemonoruntime.CRISocket(runtimeSpec(c))
	service := kubemonoruntime.ServiceName(runtimeSpec(c))
	checks := fmt.Sprintf("确认 %s 正在监听该 socket（journalctl -u %s）、node.runtime 的 socket 与运行时一致", service, service)
	if !runtimeSpec(c).IsCustom() {
		checks += "、/var/lib/containerd 所在分区有足够空间"
	}
	msg := fmt.Sprintf("%s 未就绪：socket %s 在 %s 内没有应答；%s", service, socket, timeout, checks)
	if lastErr != nil {
		return fmt.Errorf("%s；最后一次错误：%v", msg, lastErr)
	}
	return errors.New(msg)
}

// desiredSpec 渲染（或返回注入的）期望状态。
func (p *Phase) desiredSpec(c *phase.Context) (*render.Spec, error) {
	spec, err := phase.DesiredSpec(c, p.Spec)
	if err != nil {
		return nil, fmt.Errorf("渲染本机 desired 失败：%w", err)
	}
	return spec, nil
}

// provider 返回本节点的运行时 provider。
func (p *Phase) provider(c *phase.Context) (kubemonoruntime.Provider, error) {
	if p.ProviderFactory != nil {
		return p.ProviderFactory(c)
	}
	opts := p.ProviderOptions
	opts.Paths = c.Paths
	if c.BOM != nil {
		opts.BOM = c.BOM
	}
	opts.Release = c.Release
	opts.DryRun = c.DryRun
	if opts.Report == nil {
		opts.Report = c.Reporter()
	}
	if opts.Commands == nil {
		opts.Commands = p.Commands
	}
	if opts.Services == nil {
		opts.Services = p.Services
	}
	if opts.Now == nil {
		opts.Now = p.ResolveClock(c)
	}
	return kubemonoruntime.NewProvider(runtimeSpec(c), opts)
}

// runtimeSpec 返回本节点的运行时配置。
func runtimeSpec(c *phase.Context) clusterconfig.RuntimeSpec {
	if c == nil || c.Cluster == nil {
		return clusterconfig.RuntimeSpec{}
	}
	return c.Cluster.Node.Runtime
}

func (p *Phase) services() kubemonoruntime.ServiceManager {
	if p.Services != nil {
		return p.Services
	}
	return kubemonoruntime.DefaultServiceManager()
}

// reportDecision 把重启/热加载判定写进日志，便于操作者核对。
func reportDecision(r output.Reporter, d kubemonoruntime.ConfigDecision) {
	switch d {
	case kubemonoruntime.DecisionRestart:
		r.Infof("运行时配置已更新：config.toml 变化，containerd 需要重启")
	case kubemonoruntime.DecisionReload:
		r.Infof("运行时配置已更新：仅 certs.d 变化，containerd 热加载，不重启")
	case kubemonoruntime.DecisionNone:
	}
}
