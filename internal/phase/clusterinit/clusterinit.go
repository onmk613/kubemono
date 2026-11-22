// Package clusterinit 实现 `cluster-init` phase：创建/收敛集群级资源。
// 它在两类节点上运行：
//   - 初始化节点（kubemono init）：创建全部资源；
//   - 已安装的 master（kubemono node repair）：补回 RBAC/cluster-info/
//     kubemono-cluster 状态。
//
// 它只做三件事：
// 1. 创建引导、节点 RBAC（全部落在 kube-system）；
// 2. 写入 kube-system/kubemono-cluster 状态 ConfigMap；
// 3. 发布加入方的信任起点 kube-public/cluster-info。
// 引导令牌与加密证书包由 `kubemono token create` 在需要时签发，不在这里注册。
package clusterinit

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"kubemono/internal/clusterinit"
	"kubemono/internal/constants"
	"kubemono/internal/phase"
	"kubemono/internal/util/fsutil"
)

// PhaseName 是 `cluster-init` phase 的名称。
const PhaseName = "cluster-init"

func init() { phase.MustRegister(&Phase{}) }

// Phase 在初始化节点上创建集群级资源。
type Phase struct {
	// Client 是用于集群级写入的管理客户端。install 会从
	// /etc/kubernetes/admin.conf（紧急情况下用 super-admin.conf）设置它。
	Client kubernetes.Interface
	// Resources 可选地覆盖渲染出的资源集合（测试用）。
	Resources *clusterinit.ResourceSet
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 报告本节点是否需要收敛集群级资源。
//
// 除了初始化节点，已安装的 master 也要跑：中途失败的安装会留下
// “节点已装、集群级资源缺失”，`node repair` 必须能补回 RBAC、cluster-info
// 与 kubemono-cluster 状态。
//
// 要求 clusterID 非空：它来自 node.yaml，是集群身份的唯一依据；空值会把
// kubemono-cluster ConfigMap 写成空身份，比不修更糟。worker 不跑（由 master 维护）。
func (p *Phase) Applies(c *phase.Context) bool {
	if c == nil {
		return false
	}
	if c.IsInitNode {
		return true
	}
	return c.IsMaster() && c.ClusterID != ""
}

// client 返回注入的客户端，未注入时回退到 phase 上下文的客户端。
// 引导写入必须优先用 super-admin.conf（O=system:masters）：kubemono-admin 能用的
// `kubemono:cluster-admins → cluster-admin` 绑定正是**本 phase 创建**的，用 admin.conf
// 会在绑定生效前就 403（真机实测：kubemono-admin 此时还没有任何权限）。
// super-admin.conf 不存在时（测试注入、或外部 CA 场景）退回 phase 上下文的客户端。
func (p *Phase) client(c *phase.Context) (kubernetes.Interface, error) {
	if p.Client != nil {
		return p.Client, nil
	}
	if cli, err := superAdminClient(c); err != nil {
		return nil, err
	} else if cli != nil {
		return cli, nil
	}
	client, err := c.KubeClient()
	if err != nil {
		return nil, fmt.Errorf("cluster-init: %w", err)
	}
	return client, nil
}

// superAdminClient 用 super-admin.conf 构造 system:masters 客户端；
// 文件不存在时返回 (nil, nil)，由调用方回退。
func superAdminClient(c *phase.Context) (kubernetes.Interface, error) {
	if c == nil {
		return nil, nil
	}
	path := c.Paths.KubeconfigPath(constants.SuperAdminKubeconfigName)
	if !fsutil.FileExists(path) {
		return nil, nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("cluster-init: 读取 %s 失败: %w", path, err)
	}
	cli, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster-init: 用 %s 构造客户端失败: %w", path, err)
	}
	return cli, nil
}

// resources 渲染（或返回注入的）资源集合。
func (p *Phase) resources(c *phase.Context) (*clusterinit.ResourceSet, error) {
	if p.Resources != nil {
		return p.Resources, nil
	}
	if c.Cluster == nil {
		return nil, fmt.Errorf("cluster-init: 缺少集群配置")
	}
	return clusterinit.Build(clusterinit.Options{
		Cluster:          c.Cluster,
		ClusterID:        c.ClusterID,
		Release:          c.Release,
		PublishClusterID: c.IsInitNode,
		Paths:            c.Paths,
	})
}

// hasClusterInfo 报告资源集合是否发布 cluster-info。
func hasClusterInfo(rs *clusterinit.ResourceSet) bool {
	for _, cm := range rs.ConfigMaps {
		if cm.Name == constants.ClusterInfoName {
			return true
		}
	}
	return false
}

// Check 报告集群级资源是否已存在。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	rs, err := p.resources(c)
	if err != nil {
		return false, err
	}
	client, err := p.client(c)
	if err != nil {
		return false, err
	}
	return rs.Check(ctx, client)
}

// Run 应用集群级资源并写入第一代配置。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	rs, err := p.resources(c)
	if err != nil {
		return err
	}
	// cluster-info 是加入方校验 CA 的唯一依据：本机有 CA（即真的在装 init 节点）
	// 就必须发布它，不能"默默不发布"。测试用的临时 root 没有 CA，走注入路径。
	if p.Resources == nil && fsutil.FileExists(c.Paths.CertPath(constants.CACertName)) && !hasClusterInfo(rs) {
		return fmt.Errorf("缺少 cluster-info 资源：本机已有集群 CA，但 cluster-info 没有被渲染")
	}
	client, err := p.client(c)
	if err != nil {
		return err
	}
	report := c.Reporter()
	if c.DryRun {
		report.Infof("将创建 %d 个集群级对象（%s）", len(rs.Names()), rs.Summary())
		for _, name := range rs.Names() {
			report.Debugf("  %s", name)
		}
		return nil
	}
	return rs.Apply(ctx, client, report.Infof)
}
