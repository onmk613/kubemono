// 本文件提供 reset --leave 的集群侧辅助函数（原先在 internal/cli/leave，
// 只有一个调用方，合并回 reset 包，避免为 3 个函数多一个包）。
package reset

import (
	"context"
	"fmt"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/execx"
	"kubemono/internal/identity"
	leavepkg "kubemono/internal/leave"
	"kubemono/internal/render"
	"kubemono/internal/systemd"
)

// newLeaveOptions 构造 leave 的集群侧参数。
// 三件事只在 CLI 层决定：
// - 本机节点名从 node.yaml 读取；
// - 本机是否是 etcd 成员（决定是否需要移除 etcd 成员与法定人数校验）；
// - 退群后的本机 reset 是否保留数据/主机改动（keepData/keepHostInit 原样透传）。
func newLeaveOptions(opts *options.GlobalOptions, keepData, keepHostInit, purge, force bool) (*leavepkg.Options, error) {
	client, err := utils.NewClusterClient(opts)
	if err != nil {
		return nil, fmt.Errorf("%w；如果本机没有 admin 凭据（例如 worker），请在 master 上执行 kubectl delete node <本机节点名> 并手动移除 etcd 成员，再回到本机执行 kubemono reset", err)
	}
	// 配置以本机 cluster.yaml（或 -f）为准：kubemono 不维护集群内配置副本。
	// 显式 -f 读不到时必须报错：否则会按默认运行时/数据目录退群+清理。
	var cluster *clusterconfig.Cluster
	var loadErr error
	if opts.ConfigPath != "" {
		cluster, loadErr = opts.LoadCluster()
		if loadErr != nil {
			return nil, loadErr
		}
	} else {
		cluster, _ = opts.LoadCluster()
	}
	paths := opts.Paths()
	if cluster != nil {
		paths = cluster.WithDataDirs(paths)
	}
	lo := &leavepkg.Options{
		Paths:             paths,
		Client:            client,
		Force:             force,
		DryRun:            opts.DryRun,
		Report:            opts.Reporter,
		KeepData:          keepData,
		KeepHostInit:      keepHostInit,
		Purge:             purge,
		ContainerdDataDir: containerdDataDirFor(cluster),
		RuntimeUnit:       runtimeUnitFor(cluster),
		StopKubelet: func(ctx context.Context) error {
			units := systemd.UnitController{Runner: execx.Default(), Report: opts.ReporterOrDiscard()}
			return units.Stop(ctx, "kubelet.service")
		},
	}
	if cluster != nil {
		lo.RuntimeType = cluster.Node.Runtime.Mode()
	}
	node, err := (&identity.Store{Dir: opts.Paths().Kubemono()}).Load()
	if err != nil {
		return nil, err
	}
	if node != nil {
		lo.NodeName = node.NodeName
	}
	if cluster == nil {
		opts.Reporter.Warnf("无法读取集群配置：跳过 etcd 成员检测与配置 generation 更新所需的角色判断；如本机是 etcd 成员，请在 master 上执行 kubectl delete node <name> 并手动移除 etcd 成员")
		return lo, nil
	}
	if lo.NodeName == "" {
		return lo, nil
	}
	configName := lo.NodeName
	if host, ok := cluster.Cluster.Hosts.FindHostByNodeName(lo.NodeName); ok {
		configName = host.Name
	}
	if host, ok := cluster.Cluster.Hosts.FindHost(configName); ok && render.RunsEtcd(&render.Context{Cluster: cluster, Host: host}) {
		lo.RemoveEtcdMember = true
		endpoints, ca, cert, key := leavepkg.EtcdEndpointsAndTLS(opts.Paths(), cluster)
		lo.Etcd = leavepkg.NewMembersClient(endpoints, ca, cert, key)
	}
	return lo, nil
}

// runtimeUnitFor 返回 purge 时要停止的运行时 unit。
func runtimeUnitFor(cluster *clusterconfig.Cluster) string {
	if cluster != nil && cluster.Node.Runtime.IsCustom() && cluster.Node.Runtime.Custom.ServiceName != "" {
		return cluster.Node.Runtime.Custom.ServiceName
	}
	return "containerd.service"
}

// containerdDataDirFor 返回 purge 时要删除的 containerd 数据目录。
func containerdDataDirFor(cluster *clusterconfig.Cluster) string {
	if cluster != nil && cluster.Node.Runtime.IsCustom() {
		// 自定义运行时的数据目录未知，不能猜一个路径去删。
		return ""
	}
	if cluster != nil && cluster.Node.Runtime.Containerd.Root != "" {
		return cluster.Node.Runtime.Containerd.Root
	}
	return "/var/lib/containerd"
}

// PrintResult 输出 leave 的摘要。
func printLeaveResult(opts *options.GlobalOptions, res *leavepkg.Result, message string) {
	if res == nil {
		return
	}
	report := opts.Reporter
	if res.EtcdMemberRemoved {
		report.Infof("已移除 etcd 成员 %s（ID=%d）", res.NodeName, res.EtcdMemberID)
	}
	if res.NodeDeleted {
		report.Infof("已删除 Node 对象 %s", res.NodeName)
	}
	if res.Reset {
		report.Infof("本机 reset 已完成")
	}
	for _, item := range res.Skipped {
		report.Infof("已跳过：%s", item)
	}
	for _, item := range res.Warnings {
		report.Warnf("%s", item)
	}
	if message != "" {
		report.Result(true, "%s", message)
	}
}
