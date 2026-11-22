// Package phase 是 install/join/node repair 共享的执行单元
// 一条命令 = 按顺序执行的一组 phase；每个 phase 自己判断“是否适用、是否已完成、
// 做什么”，需要回滚时在自身 Run 内完成（如 kubelet 的 render.Rollback），
// 因此同一条命令可以重复执行（幂等、可续跑）。
package phase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
	"kubemono/internal/util/output"
)

// ErrNoClusterClient 表示集群级 phase 拿不到 Kubernetes 客户端。
// 它是"还没到那一步"与"配置缺失"的统一错误：install 会惰性构造客户端，
// 因此在 admin.conf 尚未生成之前调用就会看到它。
var ErrNoClusterClient = errors.New("缺少 Kubernetes 客户端（需要 master 上的 admin kubeconfig）")

// Context 是一个 phase 可能需要的全部内容。节点身份通过 Node/Host 暴露。
type Context struct {
	// Cluster 是本节点所遵循的集群配置。
	Cluster *clusterconfig.Cluster
	// ClusterID 是集群不可变的标识。
	ClusterID string
	// Node 是本节点身份（/usr/local/kubemono/node.yaml）；节点注册之前可能为 nil，
	// 此时 phase 必须依赖 Host。
	Node *clusterconfig.NodeConfig
	// Host 是本节点在集群配置中的条目。
	Host *clusterconfig.Host
	// NodeNameValue 是本机实际的 Kubernetes NodeName；为空时按 Node/Host 推导。
	NodeNameValue string
	// IsInitNode 表示本节点本次是否执行显式的 kubemono init。
	IsInitNode bool
	// Client 是集群级操作（cluster-init、节点状态上报）使用的 Kubernetes 客户端。
	// 本机 phase 不应需要它：本机操作不依赖 apiserver，这也是"守护而非替代"的前提。
	Client kubernetes.Interface
	// ClientFunc 惰性解析集群客户端；见 KubeClient。
	// install 必须用它：同一个 phase 序列里，kubeconfig phase 才写出
	// admin.conf，cluster-init/mark-control-plane 在那之后才能读到凭据，
	// 因此在决定计划时无法提前构造客户端。
	ClientFunc func() (kubernetes.Interface, error)
	// JoinSecret 是 join 凭证（由 `kubemono token create` 打印的命令参数构造），
	// 仅在加入节点上运行 `install` 期间存在。worker 需要它的 bootstrap token
	// 来写 bootstrap-kubelet.conf；其他任何东西都不得依赖它。
	JoinSecret *clusterconfig.JoinSecret
	// BOM 是内嵌的物料清单。
	BOM *bom.BOM
	// Paths 解析所有约定路径（生产环境中 Root 为空）。
	Paths constants.Paths
	// Release 是正在运行的 kubemono 版本，例如 v1.31.4-km.1。
	Release string
	// HostMTU 是本机目标网卡 MTU；0 表示由渲染器使用 1500 兜底。
	HostMTU int
	// DryRun 让 phase 只报告将要做什么，而不改动主机。
	DryRun bool
	// Report 接收进度消息。
	Report output.Reporter
	// Now 返回当前时间（测试可注入）。
	Now func() time.Time
}

// Reporter 返回一个非 nil 的 reporter。
func (c *Context) Reporter() output.Reporter {
	if c.Report == nil {
		return output.Discard()
	}
	return c.Report
}

// KubeClient 返回集群级 phase 使用的客户端。
// 解析顺序（唯一实现，所有集群级 phase 都用它，避免各自判断）：
// 1. 显式注入的 Client（install/join 的 phase、`network install`、测试）；
// 2. ClientFunc 惰性构造（install 在控制面起来之后才读 admin.conf）；
// 3. 都没有时返回 ErrNoClusterClient。
func (c *Context) KubeClient() (kubernetes.Interface, error) {
	if c == nil {
		return nil, ErrNoClusterClient
	}
	if c.Client != nil {
		return c.Client, nil
	}
	if c.ClientFunc != nil {
		client, err := c.ClientFunc()
		if err != nil {
			return nil, err
		}
		if client != nil {
			// 缓存惰性解析结果：install 的多个 phase（以及收尾写 desired）
			// 共用同一个客户端，避免重复读 kubeconfig，也避免测试里出现
			// “ClientFunc 被调用两次”的不一致。
			c.Client = client
			return client, nil
		}
	}
	return nil, ErrNoClusterClient
}

// Clock 返回一个非 nil 的时钟。
func (c *Context) Clock() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// Self 返回本节点在集群配置中的条目（如果存在）。
func (c *Context) Self() (*clusterconfig.Host, bool) {
	if c.Host != nil {
		return c.Host, true
	}
	if c.Cluster == nil {
		return nil, false
	}
	name := c.ConfigName()
	if name == "" {
		return nil, false
	}
	h, ok := c.Cluster.Cluster.Hosts.FindHost(name)
	return h, ok
}

// NodeName 返回本机实际的 Kubernetes NodeName（kubelet/etcd/证书 CN）。
func (c *Context) NodeName() string {
	return clusterconfig.NodeNameOf(c.NodeNameValue, c.Node, c.Host)
}

// RenderContext 把 phase 上下文转换为 render 上下文。
// 所有 phase 共用这一份映射：render.Context 新增字段时只改这里。
func (c *Context) RenderContext() *render.Context {
	if c == nil {
		return nil
	}
	return &render.Context{
		Cluster:       c.Cluster,
		Host:          c.Host,
		Node:          c.Node,
		IsInitNode:    c.IsInitNode,
		NodeNameValue: c.NodeName(),
		BOM:           c.BOM,
		Paths:         c.Paths,
		Release:       c.Release,
		ClusterID:     c.ClusterID,
		HostMTU:       c.HostMTU,
	}
}

// PreviousSpec 读取上一次 install 写入的 desired 快照。
// 读不到或快照损坏时返回 nil（按首次安装处理），不把错误变成阻断：
// 续跑的主路径是各 phase 的 Check，而不是这份快照。
func (c *Context) PreviousSpec() *render.Spec {
	if c == nil {
		return nil
	}
	spec, err := render.NewStore(c.Paths).Restore()
	if err != nil {
		return nil
	}
	return spec
}

// ConfigName 返回本机在 cluster.yaml 中的身份键。
func (c *Context) ConfigName() string {
	if c.Host != nil {
		return c.Host.Name
	}
	if c.Node != nil {
		if c.Node.HostName != "" {
			return c.Node.HostName
		}
		return c.Node.NodeName
	}
	return ""
}

// IsMaster 表示本节点是否运行控制面。
func (c *Context) IsMaster() bool {
	if h, ok := c.Self(); ok {
		return h.IsMaster()
	}
	return false
}

// DesiredSpec 返回注入的期望状态，未注入时按默认 builders 渲染。
// 多个 render 型 phase 共用这一份“注入或渲染”的接线，避免各写一份。
func DesiredSpec(c *Context, injected *render.Spec) (*render.Spec, error) {
	if injected != nil {
		return injected, nil
	}
	return render.Render(c.RenderContext(), render.DefaultBuilders()...)
}

// Phase 是一个可独立执行的步骤。
// 与早期的接口相比，这里把 `n *clusterconfig.NodeConfig` 收进 *Context：
// 渲染一个节点需要 cluster.yaml、BOM、路径和生成号，只传 node.yaml 不够。
type Phase interface {
	// Name 是 phase 的名称。
	Name() string
	// Applies 表示该 phase 是否与本节点相关。
	Applies(c *Context) bool
	// Check 表示该 phase 是否已完成（幂等）。
	Check(ctx context.Context, c *Context) (bool, error)
	// Run 执行该 phase。
	Run(ctx context.Context, c *Context) error
}

// Result 是一次 phase 执行的结果。
type Result struct {
	Name    string
	Applies bool // 不适用时为 false；此时 Done/Err 均为零值
	Done    bool // Check 报告该 phase 已完成
	Err     error
	// Pending 表示 Run 成功、但事后 Check 仍认为未收敛（例如 Deployment 还在滚动）。
	// 它不是失败，但必须让操作者看到；下一次 repair 会继续。
	Pending  bool
	Duration time.Duration
}

// Summary 汇总一次 phase 运行的结果。
type Summary struct {
	Results []Result
	// Failed 是第一个失败的 phase（如果有）。
	Failed string
	// Pending 是 Run 成功但尚未收敛的 phase。
	Pending []string
}

// OK 表示是否所有 phase 都成功。
func (s Summary) OK() bool { return s.Failed == "" }

// Error 返回描述失败 phase 的错误。
func (s Summary) Error() error {
	if s.OK() {
		return nil
	}
	for _, r := range s.Results {
		if r.Name == s.Failed && r.Err != nil {
			return fmt.Errorf("phase %s failed: %w", r.Name, r.Err)
		}
	}
	return fmt.Errorf("phase %s failed", s.Failed)
}

// Applied 返回真正执行了工作的 phase 名。
func (s Summary) Applied() []string {
	var out []string
	for _, r := range s.Results {
		if r.Applies && !r.Done && r.Err == nil {
			out = append(out, r.Name)
		}
	}
	return out
}
