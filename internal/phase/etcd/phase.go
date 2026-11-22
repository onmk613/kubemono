// Package etcd 实现 `etcd` phase：init / join。
// 两种子行为（`Mode`）：
// - init：init master（仅本地 etcd）只做
// “数据目录就绪 + 启动 etcd.service + 健康等待”；单节点时没有任何成员操作。
// - join：在已运行的集群上 AddLearner（端点来自配置）→ 本机以
// initial-cluster-state=existing 启动 → 等待追平（比较 leader 的 RaftIndex
// 与本机 Status.RaftIndex）→ Promote。
// 为什么注册的是一个带 Mode 的 Phase，而不是两个 phase 名：
// install/join 的 phase 只接受一个位置参数（phase runner 的
// MaximumNArgs(1)），etcd phase 的子阶段无法传进来。因此这里注册
// ModeAuto 的 `etcd`（按节点身份自动选 init/join），测试可显式选择子行为。
// 所有动作幂等：已经是成员、已 promote 都不报错，只说明现状。
// 注入点：ProviderFactory、ClientFactory、Services、Local、Sleep、Now。
package etcd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	etcdapi "kubemono/internal/etcd"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	"kubemono/internal/util/fsutil"
)

// PhaseName 是 `etcd` phase 的名称。
const PhaseName = "etcd"

// restartMarkerName 是“etcd unit 已写、尚未成功重启”的标记名。
const restartMarkerName = "etcd"

// 默认等待参数：etcd 首次启动要建 WAL 与 snapshot；learner 追平取决于数据量。
const (
	// DefaultTimeout 是等待本机健康与 learner 追平的上限。
	DefaultTimeout = 5 * time.Minute
	// DefaultPollInterval 是两次探测之间的间隔。
	DefaultPollInterval = 2 * time.Second
)

// Mode 是 etcd phase 的子行为。
type Mode string

// 子行为。
const (
	// ModeAuto 按节点身份选择 init 或 join（注册给 `etcd` phase）。
	ModeAuto Mode = "auto"
	// ModeInit 初始化本机 etcd 成员。
	ModeInit Mode = "init"
	// ModeJoin 以 learner 身份加入并 promote。
	ModeJoin Mode = "join"
)

func init() { phase.MustRegister(&Phase{Mode: ModeAuto}) }

// Phase 执行本机 etcd 的 init/join。
type Phase struct {
	// Mode 是子行为；空值等同 ModeAuto。
	Mode Mode
	// ProviderFactory 覆盖形态判定（测试注入）。
	ProviderFactory func(c *phase.Context) (etcdapi.Provider, error)
	// ClientFactory 覆盖 etcd 客户端（测试注入 fake，不发真实网络请求）。
	ClientFactory func(c *phase.Context) (etcdapi.API, error)
	// Services 执行 systemd 动作；nil 使用当前平台的默认实现。
	Services etcdapi.ServiceManager
	// Local 执行数据目录/清单的文件操作；nil 使用当前平台的默认实现。
	Local etcdapi.LocalOps
	// InstallBinary 从离线物料安装宿主二进制（systemd unit 模式需要）；
	// nil 使用 internal/kubelet 的通用安装器。测试注入假实现。
	InstallBinary func(ctx context.Context, c *phase.Context, component string) error
	// Render 覆盖 desired 渲染（测试注入）。nil 使用 render.Render + 默认 builders。
	// 注入的实现必须像生产一样消费 rc.EtcdInitialCluster/EtcdInitialClusterState
	// ——这保证"覆盖结构化 flags"只有一条路径，没有文本改写。
	Render func(rc *render.Context) (*render.Spec, error)
	// WaitKnobs 统一等待循环的超时/间隔/睡眠/时钟注入（见 phase.WaitKnobs）。
	// 注入的测试时钟让轮询不真正阻塞。
	phase.WaitKnobs
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 报告本机在所选子行为下是否有事可做。
func (p *Phase) Applies(c *phase.Context) bool {
	prov, err := p.provider(c)
	if err != nil {
		return false
	}
	return prov.Local().Member
}

// Check 报告本 phase 是否已经应用。
// 判据（幂等）：
//
//	init/join：本机成员存在 + 本机 endpoint 健康 +（join）已 promote；
//
// 数据目录还没初始化时，额外要求渲染文件（含
// --initial-cluster-state）与期望一致——这是 etcd 首次启动的判据。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	prov, err := p.provider(c)
	if err != nil {
		return false, err
	}
	local := prov.Local()
	if !local.Member {
		return true, nil
	}
	if pending, err := phase.HasPendingRestart(c, restartMarkerName); err != nil {
		return false, err
	} else if pending {
		c.Reporter().Debugf("etcd 有待完成的重启，需要重跑")
		return false, nil
	}
	cli, err := p.client(c, prov)
	if err != nil {
		return false, err
	}
	members, err := cli.Members(ctx)
	if err != nil {
		c.Reporter().Debugf("etcd 还未就绪：%v", err)
		return false, nil
	}
	member, ok := etcdapi.FindMember(members, local.Name, local.PeerURLs)
	if !ok {
		return false, nil
	}
	if p.mode(c) == ModeJoin && member.IsLearner {
		c.Reporter().Debugf("本机还是 learner，等待追平后 promote")
		return false, nil
	}
	st, err := cli.Status(ctx, local.ClientURL)
	if err != nil || !st.Healthy {
		c.Reporter().Debugf("本机 etcd endpoint %s 还不健康：%v", local.ClientURL, err)
		return false, nil
	}
	initialized := p.dataInitialized(prov)
	if initialized {
		// 数据目录已初始化后 etcd 不再读取 --initial-cluster*：这两行由加入
		// 时的成员列表决定，不能因为集群扩容就判成漂移。但 unit 里的其它 flag
		// （quota、snapshot-count、listen URLs、extraArgs）仍然必须与期望一致，
		// 否则 node repair 会静默跳过配置变更。
		if disabled, err := p.unitEnabled(ctx, local.ServiceName); err != nil {
			return false, err
		} else if !disabled {
			c.Reporter().Debugf("%s 未设置开机自启，需要重跑", local.ServiceName)
			return false, nil
		}
	}
	_, expected, err := p.expectedArtifact(ctx, c, prov, cli)
	if err != nil {
		return false, err
	}
	live, err := os.ReadFile(p.localArtifactPath(c))
	if err != nil {
		return false, nil
	}
	if !etcdArtifactMatches(live, expected, initialized) {
		c.Reporter().Debugf("本机 etcd 文件与期望不一致（%s），需要重写", p.localArtifactPath(c))
		return false, nil
	}
	return true, nil
}

// unitEnabled 查询 etcd.service 是否开机自启；注入的 ServiceManager 不实现
// EnabledChecker 时返回 true（不把“无法查询”当成漂移）。
func (p *Phase) unitEnabled(ctx context.Context, unit string) (bool, error) {
	if ec, ok := p.services().(interface {
		IsEnabled(context.Context, string) (bool, error)
	}); ok {
		return ec.IsEnabled(ctx, unit)
	}
	return true, nil
}

// etcdArtifactMatches 比较 live 与期望的 etcd 文件。
// initialized=true 时忽略 --initial-cluster 与 --initial-cluster-state：
// etcd 在 WAL 存在后不再使用它们，而它们的值会随成员变化。
func etcdArtifactMatches(live, expected []byte, initialized bool) bool {
	normalize := func(data []byte) []string {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if initialized && (strings.HasPrefix(line, "--initial-cluster=") ||
				strings.HasPrefix(line, "--initial-cluster-state=")) {
				continue
			}
			out = append(out, line)
		}
		return out
	}
	a := normalize(live)
	b := normalize(expected)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Run 执行选定的子行为。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	prov, err := p.provider(c)
	if err != nil {
		return err
	}
	switch p.mode(c) {
	case ModeJoin:
		return p.runJoin(ctx, c, prov)
	default:
		return p.runInit(ctx, c, prov)
	}
}

// runInit 启动本机 etcd 并等待健康；init 节点不做任何成员变更。
func (p *Phase) runInit(ctx context.Context, c *phase.Context, prov etcdapi.Provider) error {
	local := prov.Local()
	if !local.Member {
		return fmt.Errorf("本机 %q 不运行 etcd 成员（%s）：本地 etcd 需要 master 角色；外部 etcd 由用户管理，请用 etcdctl", local.Name, prov.Form())
	}
	if c.DryRun {
		c.Reporter().Infof("dry-run：将准备数据目录 %s、写入 %s 并启动 etcd（%s）", local.DataDir, p.localArtifactPath(c), local.ServiceName+"（systemd unit）")
		return nil
	}
	file, content, err := p.expectedArtifact(ctx, c, prov, nil)
	if err != nil {
		return err
	}
	if err := p.startLocal(ctx, c, prov, file, content); err != nil {
		return err
	}
	cli, err := p.client(c, prov)
	if err != nil {
		return err
	}
	if err := p.waitHealthyLocal(ctx, c, prov, cli); err != nil {
		return err
	}
	if err := phase.ClearRestartMarker(c, restartMarkerName); err != nil {
		return err
	}
	members, err := cli.Members(ctx)
	if err != nil {
		c.Reporter().Warnf("读取成员列表失败（%v）：etcd 已就绪，成员变更由各节点的 join 完成", err)
		return nil
	}
	switch {
	case len(members) <= 1:
		c.Reporter().Infof("单节点 etcd（成员 %s）：无需成员操作", local.Name)
	default:
		c.Reporter().Infof("集群已有 %d 个成员：init 只负责启动本机成员，其他成员由各自节点的 etcd join 加入", len(members))
	}
	return nil
}

// ---- 注入点 ----
// mode 解析要执行的子行为。
func (p *Phase) mode(c *phase.Context) Mode {
	if p.Mode != "" && p.Mode != ModeAuto {
		return p.Mode
	}
	// auto：负责初始化 etcd 集群的主机做 init（本地 etcd 是 init master，
	// 本地 etcd 只有 init master），其余节点做 join。
	if c != nil && c.IsInitNode {
		return ModeInit
	}
	return ModeJoin
}

// provider 返回本机 etcd 的形态。
func (p *Phase) provider(c *phase.Context) (etcdapi.Provider, error) {
	if p.ProviderFactory != nil {
		return p.ProviderFactory(c)
	}
	if c == nil || c.Cluster == nil {
		return nil, errors.New("etcd phase 需要集群配置（cluster.yaml）")
	}
	host, _ := c.Self()
	return etcdapi.NewProvider(c.Cluster, host, c.Paths, c.NodeName())
}

// client 返回用于集群级操作的 etcd 客户端。
func (p *Phase) client(c *phase.Context, prov etcdapi.Provider) (etcdapi.API, error) {
	if p.ClientFactory != nil {
		return p.ClientFactory(c)
	}
	cfg, err := prov.MemberConfig()
	if err != nil {
		return nil, err
	}
	cfg.Report = c.Reporter()
	cfg.Now = p.ResolveClock(c)
	return etcdapi.New(cfg)
}

func (p *Phase) services() etcdapi.ServiceManager {
	if p.Services != nil {
		return p.Services
	}
	return etcdapi.DefaultServiceManager()
}

func (p *Phase) local() etcdapi.LocalOps {
	if p.Local != nil {
		return p.Local
	}
	return etcdapi.DefaultLocalOps()
}

// pollOptions 把注入的等待参数解析成 Poll 选项（默认值在此收敛）。
func (p *Phase) pollOptions(c *phase.Context) phase.PollOptions {
	return p.PollOptions(c, DefaultTimeout, DefaultPollInterval)
}

// now 返回生效时钟的当前时间（journal 时间戳与渲染用）。
func (p *Phase) now(c *phase.Context) time.Time {
	return p.ResolveClock(c)()
}

// spec 渲染（或返回注入的）desired 状态。
func (p *Phase) spec(c *phase.Context) (*render.Spec, error) {
	return p.render(c.RenderContext())
}

// render 用注入的（或默认的）渲染函数产出 desired。
func (p *Phase) render(rc *render.Context) (*render.Spec, error) {
	if p.Render != nil {
		return p.Render(rc)
	}
	spec, err := render.Render(rc, render.DefaultBuilders()...)
	if err != nil {
		return nil, fmt.Errorf("渲染本机 etcd desired 失败：%w", err)
	}
	return spec, nil
}

// ---- 本机文件 ----
// localArtifactPath 返回本机 etcd 的 systemd unit 路径。
func (p *Phase) localArtifactPath(c *phase.Context) string {
	return c.Paths.Resolve("/etc/systemd/system/" + etcdapi.ServiceName)
}

// localArtifact 返回本机 etcd 的渲染文件。
// 优先取渲染结果里的 etcd.service（etcd-member builder 渲染），没有时用
// profile 的 flags 模板现场拼 unit。
func (p *Phase) localArtifact(c *phase.Context) (render.File, error) {
	spec, err := p.spec(c)
	if err != nil {
		return render.File{}, err
	}
	path := p.localArtifactPath(c)
	f, ok := spec.Find(path)
	if !ok {
		return render.File{}, fmt.Errorf("渲染结果里没有 %s：etcd-member builder 应为本节点渲染 etcd.service", path)
	}
	return f, nil
}

// expectedArtifact 返回本机 etcd 文件应有的内容。
// join 模式会覆盖 --initial-cluster 与 --initial-cluster-state：
// etcd 在 existing 状态下会比对 --initial-cluster 的条目数与集群实际成员数
// 因此必须写入“此刻真实的成员列表”，而不是离线渲染时的全量列表。
func (p *Phase) expectedArtifact(ctx context.Context, c *phase.Context, prov etcdapi.Provider, cli etcdapi.API) (render.File, []byte, error) {
	if p.mode(c) != ModeJoin || p.dataInitialized(prov) {
		// 非 join，或数据目录已初始化（WAL 存在）时 etcd 不校验
		// --initial-cluster*：保留渲染结果原样，避免与 control-plane phase
		// 争抢文件内容。
		file, err := p.localArtifact(c)
		if err != nil {
			return render.File{}, nil, err
		}
		return file, file.Content, nil
	}
	if cli == nil {
		var err error
		cli, err = p.client(c, prov)
		if err != nil {
			return render.File{}, nil, err
		}
	}
	initialCluster, err := p.initialCluster(ctx, cli, prov)
	if err != nil {
		return render.File{}, nil, err
	}
	if len(initialCluster) == 0 {
		return render.File{}, nil, errors.New("etcd: --initial-cluster 列表为空：先确认集群成员（`etcdctl member list`）")
	}
	// 走同一套 builders，只覆盖结构化 flags；这样 unit 的描述、ExtraArgs
	// 与 control-plane 渲染完全一致，也不会出现第二份 unit 渲染逻辑。
	// 测试注入 Render hook 时同样只消费这两个结构化字段——没有"先渲染、
	// 再改写文本"的第二条路径。
	rc := c.RenderContext()
	rc.EtcdInitialCluster = initialCluster
	rc.EtcdInitialClusterState = "existing"
	spec, err := p.render(rc)
	if err != nil {
		return render.File{}, nil, err
	}
	file, ok := spec.Find(p.localArtifactPath(c))
	if !ok {
		return render.File{}, nil, fmt.Errorf("渲染结果里没有 %s", p.localArtifactPath(c))
	}
	if !strings.Contains(string(file.Content), "--initial-cluster="+strings.Join(initialCluster, ",")) {
		return render.File{}, nil, fmt.Errorf("渲染结果里没有期望的 --initial-cluster：确认 profile 的 etcd.flags 模板")
	}
	if !strings.Contains(string(file.Content), "--initial-cluster-state=existing") {
		return render.File{}, nil, fmt.Errorf("渲染结果里没有 --initial-cluster-state=existing：确认 profile 的 etcd.flags 模板")
	}
	return file, file.Content, nil
}

// initialCluster 从集群实际成员生成 --initial-cluster。
func (p *Phase) initialCluster(ctx context.Context, cli etcdapi.API, prov etcdapi.Provider) ([]string, error) {
	local := prov.Local()
	members, err := cli.Members(ctx)
	if err != nil {
		return nil, err
	}
	peerURL := ""
	if len(local.PeerURLs) > 0 {
		peerURL = local.PeerURLs[0]
	}
	return etcdapi.InitialCluster(members, local.Name, peerURL)
}

// applyLocal 写入本机 etcd 文件（原子替换 + 备份 + dry-run）。
// render.Apply 通过 fsutil.WriteFileAtomic 写盘：临时文件与目标同目录、以 "."
// 开头，kubelet 会忽略 manifests 目录里以 "." 开头的文件（ 的"清单目录里
// 不能出现备份文件"），因此这里可以安全地复用同一个原子写实现。
func (p *Phase) applyLocal(c *phase.Context, file render.File, content []byte) error {
	out := file
	out.Content = content
	_, err := render.Apply([]render.FileChange{{File: out, Kind: render.ChangeAdded}}, render.ApplyOptions{
		BackupDir: c.Paths.Backup(),
		Timestamp: fsutil.Timestamp(p.now(c)),
		DryRun:    c.DryRun,
		Reporter:  c.Reporter(),
	})
	if err != nil {
		return fmt.Errorf("写入 %s 失败：%w", out.Path, err)
	}
	return nil
}

// startLocal 准备数据目录、写入 etcd 文件并启动本机 etcd。
func (p *Phase) startLocal(ctx context.Context, c *phase.Context, prov etcdapi.Provider, file render.File, content []byte) error {
	local := prov.Local()
	if c.DryRun {
		c.Reporter().Infof("dry-run：将准备数据目录 %s、写入 %s 并启动 etcd（%s）",
			local.DataDir, file.Path, local.ServiceName+"（systemd unit）")
		return nil
	}
	// etcd 是宿主二进制，必须先从离线物料安装到 /usr/local/bin。
	// 真机实测：漏了这一步，unit 启动即 203/EXEC（Unable to locate executable
	// '/usr/local/bin/etcd'），etcd phase 等满 5 分钟后失败。
	if err := phase.InstallComponent(ctx, c, p.InstallBinary, "etcd"); err != nil {
		return err
	}
	if err := p.local().PrepareDir(local.DataDir, 0o700); err != nil {
		return fmt.Errorf("准备数据目录 %s 失败：%w；确认 /var/lib 可写且分区有足够空间", local.DataDir, err)
	}
	live, hadLive := render.ReadLive(file)
	changed := !hadLive || string(live.Content) != string(content)
	pendingBefore, pendingErr := phase.HasPendingRestart(c, restartMarkerName)
	if pendingErr != nil {
		return pendingErr
	}
	services := p.services()
	active, activeErr := services.IsActive(ctx, local.ServiceName)
	if err := phase.WriteRestartMarker(c, restartMarkerName, []string{local.ServiceName},
		"etcd 文件已更新，等待 etcd 重启"); err != nil {
		return err
	}
	if err := p.applyLocal(c, file, content); err != nil {
		return err
	}
	if err := services.DaemonReload(ctx); err != nil {
		return fmt.Errorf("%w；确认 %s 语法正确", err, file.Path)
	}
	if err := services.Action(ctx, local.ServiceName, "enable"); err != nil {
		return fmt.Errorf("设置 %s 开机自启失败：%w", local.ServiceName, err)
	}
	// unit 内容变化且 etcd 已在运行时必须 restart：start 对 active unit 是
	// no-op，会让新 flag 永远不生效（P1-1 的 etcd 分支）。
	action := "start"
	if (changed || pendingBefore) && activeErr == nil && active {
		// pendingBefore：上一次写 unit 后重启失败；即使本次没有内容变化，
		// start 对 active unit 也是 no-op，必须 restart。
		action = "restart"
	}
	if err := services.Action(ctx, local.ServiceName, action); err != nil {
		return fmt.Errorf("%s %s 失败：%w；查看 journalctl -u %s", action, local.ServiceName, err, local.ServiceName)
	}
	return nil
}

// waitHealthyLocal 有界轮询本机 endpoint，直到健康或超时。
func (p *Phase) waitHealthyLocal(ctx context.Context, c *phase.Context, prov etcdapi.Provider, cli etcdapi.API) error {
	local := prov.Local()
	opts := p.pollOptions(c)
	timeout := opts.Timeout
	err := phase.Poll(ctx, opts, func() (bool, error) {
		st, err := cli.Status(ctx, local.ClientURL)
		switch {
		case err == nil && st.Healthy:
			c.Reporter().Infof("本机 etcd 已就绪（%s，raftIndex=%d）", local.ClientURL, st.RaftIndex)
			return true, nil
		case err != nil:
			return false, err
		default:
			return false, fmt.Errorf("自报错误：%s", strings.Join(st.Errors, "; "))
		}
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("本机 etcd 在 %s 内没有就绪（%s）：%w；用 journalctl -u %s 查看 etcd 日志，确认数据目录与证书可读",
		timeout, local.ClientURL, err, local.ServiceName)
}

// dataInitialized 报告本机 etcd 是否已经初始化过（有 WAL/backend）。
func (p *Phase) dataInitialized(prov etcdapi.Provider) bool {
	return p.local().Exists(filepath.Join(prov.Local().DataDir, "member", "snap", "db"))
}

// configuredMember 报告本机是否在集群配置声明的 etcd 成员里。
func configuredMember(prov etcdapi.Provider, name string) bool {
	return slices.Contains(prov.ConfiguredNames(), name)
}
