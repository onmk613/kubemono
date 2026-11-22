// Package install 编排 `kubemono init` / `kubemono join`：识别身份 → 判定动作 →
// 按顺序执行 phase。
package install

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"kubemono/internal/bom"
	"kubemono/internal/bootstrap"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/hostinit"
	"kubemono/internal/identity"
	"kubemono/internal/network"
	"kubemono/internal/phase"
	renderpkg "kubemono/internal/render"
	"kubemono/internal/util/fsutil"
	"kubemono/internal/util/output"
)

// Action 是 install 在本节点上决定执行的动作。
type Action string

// Actions 列出全部动作取值。
const (
	// ActionInit 在初始化节点上引导一个全新集群。
	ActionInit Action = "init"
	// ActionJoin 加入已有集群。
	ActionJoin Action = "join"
	// ActionSync 修复已安装节点（幂等重跑）。
	ActionSync Action = "sync"
)

// Options 是一次 install 运行的输入。
type Options struct {
	// Cluster 是期望的集群配置。
	Cluster *clusterconfig.Cluster
	// ClusterID 是不可变的集群身份。全新的 init 运行中为空（由编排层生成）。
	ClusterID string
	// Release 是正在运行的 kubemono 版本。
	Release string
	// JoinSecret 是 join 凭证（加入节点时必填）。
	JoinSecret *clusterconfig.JoinSecret
	// PreferredEndpoint 是 `join <endpoint>` 显式给出的入口。
	// 非空时 joinEndpoints 把它排在最前面。
	PreferredEndpoint string
	// Node 强制指定节点身份。
	Node string
	// AllowAddressMismatch 接受非本机地址（NAT）并覆盖身份解析的拒绝判定。
	AllowAddressMismatch bool
	// ReplacePKI 允许 join 用集群证书包覆盖本机已有的、不同的 CA 私钥
	// （等价于重建本机 PKI，会让本机已签发的证书失效）。
	ReplacePKI bool
	// RebindIdentity 允许 --node 与本机 node.yaml 不一致时改写身份文件
	// （节点在 cluster.yaml 里改名后的迁移路径）。
	RebindIdentity bool
	// AllowReleaseMismatch 允许用不同 release 的 kubemono 修复已安装节点。
	// 默认拒绝：不同 release 的物料/渲染契约可能不同，半修比不修更危险。
	AllowReleaseMismatch bool
	// Diff 只打印受管文件与期望状态的差异，不做任何变更
	// （`kubemono node repair --diff`）。
	Diff bool
	// DiffExitCode 让 --diff 在发现差异时返回非 0（CLI 的 --exit-code）。
	DiffExitCode bool
	// Timeout 限制整轮动作的时长；0 表示不限制（phase 内部超时仍然生效）。
	Timeout time.Duration
	// ForceInit 允许在可能残留旧数据的节点上执行初始化。
	ForceInit bool
	// ForceAction 跳过自动 init/join 判定，直接执行指定的动作。`kubemono init` /
	// `kubemono join` 用它把"显式拆分"落实到同一条执行路径上，避免两条代码路径
	// 产生行为差异。
	ForceAction Action
	// Only 把动作限制在本机计划内的这些 phase（`kubemono node repair --only`）。
	// 只对 ActionSync 生效：init/join 的 phase 顺序是安装契约，少跑一步会留下
	// 半装节点。空表示按动作的全集执行。
	Only []string
	// DryRun 只报告计划而不执行。
	DryRun bool
	// Wait 限制加入节点上"等待集群出现"循环的时长。
	Wait time.Duration
	// Paths 解析所有约定路径。
	Paths constants.Paths
	// BOM 是内嵌的物料清单。
	BOM *bom.BOM
	// Report 接收进度消息。
	Report output.Reporter
	// Now 可在测试中注入。
	Now func() time.Time

	// Resolver 可注入；为 nil 时构造默认实现。
	Resolver *identity.Resolver
	// ClientFunc 惰性构造集群级 phase 使用的集群客户端。
	// 为什么需要它：install 是一条 phase 流水线，admin.conf 由 kubeconfig phase
	// 写出，因此在决定计划时构造客户端必然失败；ClientFunc 让 phase 在真正
	// 需要时（控制面起来之后）才读凭据。
	ClientFunc func() (kubernetes.Interface, error)
	// Registry 保存可用的 phase；为 nil 时使用 phase.Default。
	Registry *phase.Registry
	// Sleep 可在加入等待循环中注入。
	Sleep func(time.Duration)
	// ClientFactory 构造 join 交换使用的引导令牌客户端；为 nil 时构造一个使用
	// Insecure TLS 的客户端（此时尚未信任 CA）。
	ClientFactory func(token bootstrap.Token) (kubernetes.Interface, error)
}

// Plan 是 install 为本节点做出的判定。
type Plan struct {
	// Action 取 init、join 或 sync。
	Action Action
	// NodeName 是实际的 Kubernetes NodeName（kubelet/etcd/证书 CN）。
	NodeName string
	// ConfigName 是节点在 cluster.yaml 中的身份键（Host.Name）。
	ConfigName string
	// Address 是解析出的节点地址。
	Address string
	// Roles 是本节点的角色。
	Roles []clusterconfig.Role
	// IsMaster 表示本节点是否运行控制面。
	IsMaster bool
	// Phases 是要执行的 phase 名称有序列表。
	Phases []string
	// Reasons 解释判定理由（中文，直接可打印）。
	Reasons []string
	// Warnings 列出非致命发现。
	Warnings []string

	// resolved 是 Decide 已经完成的本机身份解析结果。Run 直接复用，
	// 不二次枚举网卡（二次解析既慢，又会把错误静默吞掉）。
	resolved *identity.Result
}

// ErrDiffFound 表示 --diff --exit-code 发现了差异。
var ErrDiffFound = errors.New("受管文件与期望状态存在差异")

// Result 是一次 install 运行的结果。
type Result struct {
	// Summary 是 phase runner 的摘要。
	Summary phase.Summary
	// ClusterID 是运行结束后生效的集群身份。
	ClusterID string
}

// Describe 渲染计划，用于 `kubemono init/join --dry-run` 一类输出。
func (p Plan) Describe() string {
	var b strings.Builder
	if p.ConfigName != "" && p.ConfigName != p.NodeName {
		fmt.Fprintf(&b, "节点: %s（配置身份 %s，%s）\n", p.NodeName, p.ConfigName, p.Address)
	} else {
		fmt.Fprintf(&b, "节点: %s（%s）\n", p.NodeName, p.Address)
	}
	fmt.Fprintf(&b, "角色: %s\n", rolesString(p.Roles))
	fmt.Fprintf(&b, "动作: %s\n", p.Action)
	fmt.Fprintf(&b, "执行顺序: %s\n", strings.Join(p.Phases, " → "))
	for _, r := range p.Reasons {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "  警告: %s\n", w)
	}
	return b.String()
}

// Decide 解析节点身份，并为调用方显式指定的动作（init/join/sync）构造计划。
// 不做"探测集群后自动判定"：那会让一次误跑变成对既有集群的写操作。
func Decide(ctx context.Context, opts *Options) (*Plan, error) {
	opts.defaults()
	if opts.Cluster == nil {
		return nil, fmt.Errorf("install: 缺少集群配置")
	}
	if opts.BOM == nil {
		return nil, fmt.Errorf("install: 缺少 BOM")
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = &identity.Resolver{NodeConfigPath: opts.Paths.NodeConfig()}
	}
	res, err := resolver.Resolve(opts.Cluster, opts.Node, opts.AllowAddressMismatch)
	if err != nil {
		return nil, err
	}
	host := res.Host
	if host == nil {
		return nil, fmt.Errorf("install: 无法确定本机在集群配置中的条目（用 --node 指定）")
	}

	plan := &Plan{
		NodeName:   res.NodeName(),
		ConfigName: host.Name,
		Address:    res.Address,
		Roles:      host.Roles,
		IsMaster:   host.IsMaster(),
		Warnings:   append([]string(nil), res.Warnings...),
		resolved:   res,
	}

	installedID := ""
	if res.NodeConfig != nil {
		installedID = res.NodeConfig.ClusterID
	}

	// 动作必须由调用方显式指定（init/join/node repair）。kubemono 不做
	// "探测集群后自动决定 init/join"：该路径没有任何 CLI 入口，且会把
	// 一次误跑变成对既有集群的写操作；需要时由运维显式选择命令。
	if opts.ForceAction == "" {
		return nil, fmt.Errorf("install: 缺少动作（init/join/sync）：调用方必须显式指定")
	}
	return forcedPlan(opts, plan, host, installedID)
}

// Run 先做判定，再执行计划。
func Run(ctx context.Context, opts *Options) (*Result, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	// Decide 会先补齐默认值；这里不重复调用，避免两次 defaults 的隐藏耦合。
	plan, err := Decide(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := verifyMaterials(opts); err != nil {
		return nil, err
	}
	result := &Result{ClusterID: opts.ClusterID}

	// init 节点在没有 clusterID 时生成一个：它是集群身份，之后不可变。
	if plan.Action == ActionInit && opts.ClusterID == "" {
		id, err := NewClusterID()
		if err != nil {
			return result, err
		}
		opts.ClusterID = id
		result.ClusterID = id
		opts.Report.Infof("生成集群身份 clusterID=%s", id)
	}

	pc := &phase.Context{
		Cluster:       opts.Cluster,
		ClusterID:     opts.ClusterID,
		Host:          hostOf(opts, plan),
		NodeNameValue: plan.NodeName,
		IsInitNode:    plan.Action == ActionInit,
		BOM:           opts.BOM,
		Paths:         opts.Paths,
		Release:       opts.Release,
		DryRun:        opts.DryRun,
		Report:        opts.Report,
		Now:           opts.Now,
		JoinSecret:    opts.JoinSecret,
		ClientFunc:    opts.ClientFunc,
	}
	if pc.Host != nil {
		pc.HostMTU = network.DetectHostMTU(pc.Host.AllAddresses()...)
	}
	if res := plan.resolved; res != nil && res.NodeConfig != nil {
		pc.Node = res.NodeConfig
		// 已安装节点的 clusterID 记在 node.yaml 里：调用方没传（sync / 幂等修复）
		// 时用它，否则 phase 会拿到空 ID，写回身份时还会把它清掉。
		if opts.ClusterID == "" {
			opts.ClusterID = res.NodeConfig.ClusterID
		}
		pc.ClusterID = opts.ClusterID
		result.ClusterID = opts.ClusterID
	}

	if opts.Node == "" {
		opts.Node = plan.ConfigName
	}
	// --diff 只看不写：在执行 phase、写快照之前返回。
	if opts.Diff {
		return result, runDiff(ctx, opts, pc, plan)
	}
	// --only 的 phase 必须真的适用于本机：external etcd 的 master 上
	// `--only etcd` 以前会被接受然后静默无操作。
	if len(opts.Only) > 0 {
		reg := opts.registry()
		for _, name := range plan.Phases {
			p, ok := reg.Get(name)
			if ok && !p.Applies(pc) {
				return result, fmt.Errorf("phase %q 不适用于本机（角色/etcd 形态不匹配）：请从 --only 中移除", name)
			}
		}
	}

	// 加入者先取得集群密钥材料/CA，再执行本机 phase。
	if plan.Action == ActionJoin {
		if opts.DryRun {
			opts.Report.Infof("将执行加入准备：%s", joinedPreparation(*plan))
		} else if err := opts.prepareJoin(ctx); err != nil {
			return result, err
		}
	}
	// 加入准备可能从 cluster-info/证书包拿到 clusterID；phase 上下文必须同步，
	// 否则 master join 的 cluster-init 会因 ClusterID 为空而跳过，desired 快照
	// 也会写成空身份。
	if opts.ClusterID != "" && pc.ClusterID == "" {
		pc.ClusterID = opts.ClusterID
		result.ClusterID = opts.ClusterID
	}

	// 未实现的 phase 必须显式失败，而不是被静默跳过。
	missing := missingPhases(opts.registry(), plan.Phases)
	if len(missing) > 0 {
		return result, fmt.Errorf("以下 phase 尚未注册，install 无法完成：%s（检查 main.go 的空导入是否包含对应 phase 包）",
			strings.Join(missing, "、"))
	}

	runner := phase.NewRunner(opts.registry(), phase.NewFileStore(opts.Paths))
	summary, err := runner.Run(ctx, pc, phase.Options{
		Only:  plan.Phases,
		Scope: "install",
	})
	result.Summary = summary
	if err != nil {
		return result, err
	}
	// 登记本机身份：install 成功后 node.yaml 才能作为下一次运行的判定依据。
	if !opts.DryRun {
		if err := saveNodeConfig(opts, plan); err != nil {
			return result, fmt.Errorf("写入本机身份 %s: %w", opts.Paths.NodeConfig(), err)
		}
		// 部分运行（--only）没有应用整机配置，不能把整份 desired 写成“已应用”；
		// 否则快照会宣称从未写过的文件已经就位，下一次 --diff 也会被它骗过。
		if len(opts.Only) > 0 {
			opts.Report.Infof("部分运行（--only）：不更新 desired 快照与 cluster.yaml 缓存")
		} else {
			// 写出 desired 快照（/usr/local/kubemono/desired）：之后的修复以它为基线。
			if err := writeDesired(ctx, opts, pc, plan); err != nil {
				return result, fmt.Errorf("写入 desired 快照: %w", err)
			}
			// /usr/local/kubemono/cluster.yaml 作为本机“已应用配置”的基线。
			// 首次同步没有这个文件时只能以 remote 为准；
			// 安装成功时把实际使用的配置写下来，后续 node repair 与 cert renew 可直接复用。
			if err := writeClusterConfig(opts.Paths.ClusterConfig(), pc.Cluster); err != nil {
				return result, fmt.Errorf("写入本机配置缓存: %w", err)
			}
		}
		// 只跑单个 phase 时不要重跑 postInstall 钩子：调用方要的是“只把
		// local-proxy 推下去”，钩子里的业务动作不该被一次局部修复触发。
		if len(opts.Only) == 0 {
			if err := hostinit.RunHooks(ctx, pc.Cluster.Cluster.Hooks.PostInstall, opts.Report.Infof); err != nil {
				return result, fmt.Errorf("postInstall 钩子失败: %w", err)
			}
		}
	}
	return result, nil
}

// buildDesiredSpec 渲染本机 desired 状态。
// --diff 与写快照必须走同一条路径，否则两者看到的文件集合会漂移。
func buildDesiredSpec(ctx context.Context, pc *phase.Context, plan *Plan) (*renderpkg.Spec, error) {
	nodeCIDR := installedNodePodCIDR(ctx, pc, plan.NodeName)
	return buildDesired(&renderpkg.Context{
		Cluster:   pc.Cluster,
		Host:      pc.Host,
		Node:      pc.Node,
		BOM:       pc.BOM,
		Paths:     pc.Paths,
		Release:   pc.Release,
		ClusterID: pc.ClusterID,
		HostMTU:   pc.HostMTU,
	}, nodeCIDR)
}

// writeDesired 渲染节点的期望状态并把它固化为基线。
// 内容必须与 node repair 渲染的完全相同（render.DefaultBuilders），否则一次
// 修复就会把"快照里有、渲染里没有"的文件当成需要删除。网络层文件也在
// 快照里，因此这里必须传入 Node.spec.podCIDR。
//
// 注意：这里刻意不用 pc.RenderContext()——两者的映射并不等价：快照渲染
// 不能携带 IsInitNode/NodeNameValue（它们会影响 etcd --initial-cluster-state
// 与 NodeName 的推导，而快照的契约是"以已写入磁盘的现状为基线"），也与
// phase 渲染传入的 EtcdInitialCluster 覆盖无关。改用 RenderContext 前必须
// 先确认这两组语义的差集为空。
func writeDesired(ctx context.Context, opts *Options, pc *phase.Context, plan *Plan) error {
	spec, err := buildDesiredSpec(ctx, pc, plan)
	if err != nil {
		return err
	}
	if err := renderpkg.NewStore(opts.Paths).Write(spec); err != nil {
		return err
	}
	opts.Report.Infof("已写入 desired 快照（%d 个文件，%s）", len(spec.Files), spec.Hash())
	return nil
}

// installedNodePodCIDR 在 install 结束时读取 Node.spec.podCIDR。
// 读不到不是致命错误：后续修复会再查一次并纠正 CNI；这里只是避免
// install 写下的 desired 快照从第一天起就是错子网。
func installedNodePodCIDR(ctx context.Context, pc *phase.Context, nodeName string) string {
	if pc == nil || nodeName == "" {
		return ""
	}
	client, err := pc.KubeClient()
	if err != nil || client == nil {
		pc.Reporter().Debugf("读取 Node %s 的 spec.podCIDR 失败（安装收尾不阻断）：%v", nodeName, err)
		return ""
	}
	n, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		pc.Reporter().Debugf("读取 Node %s 的 spec.podCIDR 失败（安装收尾不阻断）：%v", nodeName, err)
		return ""
	}
	return strings.TrimSpace(n.Spec.PodCIDR)
}

// writeClusterConfig 原子写入本机配置缓存。
func writeClusterConfig(path string, cfg *clusterconfig.Cluster) error {
	if cfg == nil {
		return nil
	}
	data, err := clusterconfig.MarshalCluster(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data, 0o600)
}

// hasRegularFiles 报告目录（递归）里是否存在非隐藏的普通文件。
// 读目录失败必须向上返回：把“无权限/IO 错误”当成“没有 etcd 数据”会绕过
// --force-init 保护。
func hasRegularFiles(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		found = true
		return fs.SkipAll
	})
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("检查残留数据目录 %s 失败: %w", dir, err)
	}
	return found, nil
}

// forcedPlan 为显式请求的动作构造计划。
func forcedPlan(opts *Options, plan *Plan, host *clusterconfig.Host, installedID string) (*Plan, error) {
	if len(opts.Only) > 0 && opts.ForceAction != ActionSync {
		return nil, fmt.Errorf("--only 只支持已安装节点的 kubemono node repair")
	}
	switch opts.ForceAction {
	case ActionInit:
		if !host.IsMaster() {
			return nil, fmt.Errorf("init 只能在 master 上执行：本机 %s 的角色是 %s", host.Name, host.RolesString())
		}
		residual, err := hasRegularFiles(opts.Paths.EtcdData())
		if err != nil {
			return nil, err
		}
		if residual && !opts.ForceInit {
			return nil, fmt.Errorf("本机存在 etcd 数据（%s）；确认要重建时使用 --force-init（仅用于灾难恢复）", opts.Paths.EtcdData())
		}
		plan.Action = ActionInit
		plan.Reasons = append(plan.Reasons, "按 --action init 显式初始化集群")
		plan.Phases = phasesFor(ActionInit, host)
		return plan, nil
	case ActionJoin:
		if opts.JoinSecret == nil {
			return nil, fmt.Errorf("join 需要 --token；如果这是第一台 master，请改用 kubemono init")
		}
		plan.Action = ActionJoin
		plan.Reasons = append(plan.Reasons, "按 --action join 显式加入集群（不等待集群探测）")
		plan.Phases = phasesFor(ActionJoin, host)
		return plan, nil
	case ActionSync:
		if installedID == "" {
			return nil, fmt.Errorf("本机尚未安装（缺少 %s）：请先执行 kubemono init 或 kubemono join", opts.Paths.NodeConfig())
		}
		installed, err := (&identity.Store{Dir: opts.Paths.Kubemono()}).Load()
		if err != nil {
			return nil, err
		}
		if installed != nil {
			if opts.Node != "" && !opts.RebindIdentity {
				current := installed.HostName
				if current != "" && current != opts.Node {
					return nil, fmt.Errorf("本机已安装为节点 %q，--node 指定了 %q：这会改写本机身份文件；"+
						"确认是改名迁移时加 --rebind-identity", current, opts.Node)
				}
			}
			if installed.NodeName != "" && installed.NodeName != plan.NodeName && !opts.RebindIdentity {
				return nil, fmt.Errorf("本机安装时的 NodeName 是 %q，按当前配置/主机名解析为 %q："+
					"节点改名会留下旧 Node 与旧证书；确认是改名迁移时加 --rebind-identity", installed.NodeName, plan.NodeName)
			}
			if installed.Release != "" && opts.Release != "" && installed.Release != opts.Release && !opts.AllowReleaseMismatch {
				return nil, fmt.Errorf("本机安装于 release %s，当前 kubemono 是 %s：跨 release 修复可能半改节点；"+
					"确认要这样做时加 --allow-release-mismatch", installed.Release, opts.Release)
			}
		}
		if opts.ClusterID != "" && opts.ClusterID != installedID {
			return nil, fmt.Errorf("本机已加入集群 %s，与期望的 %s 不一致：拒绝修复", installedID, opts.ClusterID)
		}
		plan.Action = ActionSync
		plan.Reasons = append(plan.Reasons, "按 kubemono node repair 对已安装节点执行幂等修复")
		plan.Phases = phasesFor(ActionSync, host)
		if len(opts.Only) > 0 {
			phases, err := selectPhases(plan.Phases, opts.Only)
			if err != nil {
				return nil, err
			}
			plan.Phases = phases
			plan.Reasons = append(plan.Reasons, "按 --only 只执行："+strings.Join(phases, "、"))
		}
		return plan, nil
	default:
		return nil, fmt.Errorf("未知的强制动作 %q（可选 init、join）", opts.ForceAction)
	}
}

// selectPhases 把调用方指定的 phase 收敛为本机计划内的子集，输出保持计划顺序。
// 顺序取计划而不是命令行：phase 之间有依赖（local-proxy 必须先于 kubelet），
// 让用户随意排列会得到一个看起来成功、实际错序的运行。
func selectPhases(planned, requested []string) ([]string, error) {
	allowed := make(map[string]bool, len(planned))
	for _, name := range planned {
		allowed[name] = true
	}
	want := make(map[string]bool, len(requested))
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !allowed[name] {
			return nil, fmt.Errorf("phase %q 不在本机修复计划中（本机可用：%s）",
				name, strings.Join(planned, "、"))
		}
		want[name] = true
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("--only 至少需要一个 phase 名称（本机可用：%s）", strings.Join(planned, "、"))
	}
	out := make([]string, 0, len(want))
	for _, name := range planned {
		if want[name] {
			out = append(out, name)
		}
	}
	return out, nil
}

// joinedPreparation 渲染加入准备的 dry-run 描述。
func joinedPreparation(plan Plan) string {
	if plan.IsMaster {
		return "用 certificateKey 解密集群证书包（kube-system/kubemono-certs，需要 --certificate-key）"
	}
	return "从 cluster-info 取得并校验集群 CA（随后由 kubelet 完成 TLS Bootstrap）"
}

// phasesFor 返回某个动作的有序 phase 列表。
// 顺序即依赖：
//
//	主机 → 运行时 → 证书 → 凭据 → [本地代理] → kubelet → etcd → 控制面 →
//	集群初始化 → 标记 → 网络 → 附加组件 → 工具
//
// 网络层在每个节点都要执行：CNI 配置是本机文件；集群级对象只由 master
// 写入（worker 上 network phase 会跳过它们）。
// 单独列 phase；这里只列出"可能需要"的全集，
// Runner 会把不适用的标记为 skipped。
// phasePlanKey 是 phase 列表的选择条件：动作 + 节点角色。
type phasePlanKey struct {
	action Action
	master bool
	worker bool
}

// phasePlans 是所有动作/角色组合的唯一 phase “集合”表。
// 顺序不在这里表达：phasesFor 统一按 phase.CanonicalOrder 排序，避免展示顺序
// 与实际执行顺序出现两套真值。新增 phase 或调整角色规则只改这一张表。
var phasePlans = map[phasePlanKey][]string{
	{ActionInit, true, false}: {
		"preflight", "host-init",
		"certs", "kubeconfig", "etcd", "etcd-health", "control-plane",
		"cluster-init",
		"network", "addon",
	},
	{ActionInit, true, true}: {
		"preflight", "host-init", "runtime",
		"certs", "kubeconfig", "kubelet", "etcd", "etcd-health", "control-plane",
		"cluster-init", "mark-control-plane",
		"network", "addon",
	},
	{ActionJoin, true, false}: {
		"preflight", "host-init",
		"certs", "kubeconfig", "etcd", "etcd-health", "control-plane",
		"network", "addon",
	},
	{ActionJoin, true, true}: {
		"preflight", "host-init", "runtime",
		"certs", "kubeconfig", "kubelet", "etcd", "etcd-health", "control-plane",
		"mark-control-plane",
		"network", "addon",
	},
	// worker 的 TLS Bootstrap 走 127.0.0.1:6443，因此本地代理必须先于 kubelet。
	{ActionJoin, false, true}: {
		"preflight", "host-init", "runtime",
		"kubeconfig", "local-proxy", "kubelet", "mark-control-plane",
		"network",
	},
	{ActionSync, true, false}: {
		"host-init", "certs", "kubeconfig", "etcd", "etcd-health", "control-plane", "cluster-init",
		"network", "addon",
	},
	// etcd 与 cluster-init 必须在 master+worker 的 sync 里：
	// “本机已安装”会把加入流程判成 sync，缺 etcd 时成员永远不会加入集群；
	// 中途失败的安装会留下“节点已装、集群级资源缺失”，sync 不补就卡在 RBAC。
	{ActionSync, true, true}: {
		"host-init", "runtime", "certs", "kubeconfig", "kubelet", "etcd", "etcd-health", "control-plane", "cluster-init", "mark-control-plane",
		"network", "addon",
	},
	{ActionSync, false, true}:  {"host-init", "runtime", "kubeconfig", "local-proxy", "kubelet", "mark-control-plane", "network"},
	{ActionSync, false, false}: {"host-init", "runtime", "kubeconfig", "local-proxy", "kubelet", "mark-control-plane", "network"},
}

func phasesFor(action Action, host *clusterconfig.Host) []string {
	master := host.IsMaster()
	worker := host.IsWorker()
	if phases, ok := phasePlans[phasePlanKey{action: action, master: master, worker: worker}]; ok {
		return phase.SortCanonical(phases)
	}
	// init/join 的异常角色组合沿用“完整节点/worker”的 phase 列表。
	switch action {
	case ActionInit:
		return phase.SortCanonical(phasePlans[phasePlanKey{ActionInit, true, true}])
	case ActionJoin:
		return phase.SortCanonical(phasePlans[phasePlanKey{ActionJoin, false, true}])
	default:
		return phase.SortCanonical(phasePlans[phasePlanKey{ActionSync, false, false}])
	}
}

// missingPhases 报告哪些必需的 phase 尚未注册。
func missingPhases(reg *phase.Registry, required []string) []string {
	var out []string
	for _, name := range required {
		if !reg.Has(name) {
			out = append(out, name)
		}
	}
	return out
}

// saveNodeConfig 持久化本机节点身份。
func saveNodeConfig(opts *Options, plan *Plan) error {
	store := &identity.Store{Dir: opts.Paths.Kubemono()}
	existing, err := store.Load()
	if err != nil {
		return err
	}
	if existing == nil {
		existing = &clusterconfig.NodeConfig{}
	}
	// clusterID 是集群身份：opts.ClusterID 为空时（例如 sync / 幂等修复的调用方
	// 没有传）**不能覆盖**已有值。真机实测：在已装好的节点上跑一次
	// 分步安装就把 node.yaml 的 clusterID 清空，之后所有
	// install 都被当成"没有安装记录"，只能 --force-init 恢复。
	if opts.ClusterID != "" {
		existing.ClusterID = opts.ClusterID
	}
	existing.HostName = plan.ConfigName
	existing.NodeName = plan.NodeName
	existing.Address = plan.Address
	existing.Roles = plan.Roles
	existing.Release = opts.Release
	if existing.InstalledAt.IsZero() {
		existing.InstalledAt = opts.Now()
	}
	existing.UpdatedAt = opts.Now()
	return store.Save(existing)
}

// hostOf 返回计划对应的 host 条目。
func hostOf(opts *Options, plan *Plan) *clusterconfig.Host {
	if opts.Cluster == nil {
		return nil
	}
	host, _ := opts.Cluster.Cluster.Hosts.FindHost(plan.ConfigName)
	return host
}

// registry 返回实际生效的 phase 注册表。
func (o *Options) registry() *phase.Registry {
	if o.Registry != nil {
		return o.Registry
	}
	return phase.Default()
}

// defaults 填充未设置的选项。
func (o *Options) defaults() {
	if o.Paths == (constants.Paths{}) {
		o.Paths = constants.DefaultPaths()
	}
	if o.Cluster != nil {
		o.Paths = o.Cluster.WithDataDirs(o.Paths)
	}
	if o.BOM == nil {
		o.BOM = bom.MustLoad()
	}
	if o.Release == "" && o.BOM != nil {
		o.Release = o.BOM.Release
	}
	if o.Report == nil {
		o.Report = output.Discard()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Wait <= 0 {
		o.Wait = constants.DefaultInstallWait
	}
	if o.Resolver == nil {
		o.Resolver = &identity.Resolver{NodeConfigPath: o.Paths.NodeConfig()}
	}
}

// NewClusterID 生成集群身份。
// 用 128 位随机数而不是 UUID 库：无需引入依赖，且格式对运维人员友好。
func NewClusterID() (string, error) {
	id, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("生成 clusterID 失败: %w", err)
	}
	return "km-" + id, nil
}

// randomHex 返回 n 个随机字节的十六进制编码。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func rolesString(roles []clusterconfig.Role) string {
	parts := make([]string, 0, len(roles))
	for _, r := range []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker} {
		for _, have := range roles {
			if have == r {
				parts = append(parts, string(r))
			}
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ",")
}
