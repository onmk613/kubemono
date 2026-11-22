package leave

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/client-go/kubernetes"

	"kubemono/internal/constants"
	"kubemono/internal/etcd"
	"kubemono/internal/identity"
	kubemonoreset "kubemono/internal/reset"
	"kubemono/internal/util/output"
)

// ErrNoClient 表示缺少集群客户端（没有 admin 凭据）。
var ErrNoClient = errors.New("缺少集群客户端：需要 master 上的 admin kubeconfig")

// ResetFunc 执行本机重置（leave 的最后一步，）。
// 注入它有两个目的：单测不真的清理主机；CLI 可以带上 --keep-data 等开关。
type ResetFunc func(ctx context.Context, opts *Options) error

// Options 是 Leave 的全部依赖与开关。
type Options struct {
	// Paths 解析路径（node.yaml、reset 的目标）。
	Paths constants.Paths
	// Client 是集群侧凭据（master 上的 admin kubeconfig）；必需。
	Client kubernetes.Interface
	// NodeName 是目标节点的实际 Kubernetes/etcd 节点名（nodeName 策略的结果）。
	// 留空时 Leave 从 node.yaml 读取。
	NodeName string
	// Force 跳过 cordon 失败与 etcd 法定人数校验（节点已损坏时使用）。
	Force bool
	// RemoveEtcdMember 表示目标节点是 etcd 成员，需要移除。
	RemoveEtcdMember bool
	// RuntimeType 传给本机 reset。
	RuntimeType string
	// KeepData / KeepHostInit 原样传给本机 reset（CLI 开关）。
	KeepData     bool
	KeepHostInit bool
	// Purge 原样传给本机 reset（CLI：--purge）。
	Purge bool
	// ContainerdDataDir 是 --purge 时要删除的 containerd 数据目录。
	ContainerdDataDir string
	// RuntimeUnit 是 --purge 时要停止/删除的运行时 unit。
	RuntimeUnit string
	// Report 接收进度消息。
	Report output.Reporter
	// DryRun 只输出计划，不改集群与主机。
	DryRun bool

	// StopKubelet 在删除 Node 之前停掉本机 kubelet，避免
	// kubelet 在 Node 删除后立即重新注册。nil 时跳过（测试/特殊环境）。
	StopKubelet func(ctx context.Context) error
	// Etcd 管理 etcd 成员；RemoveEtcdMember 为 true 时必需。
	Etcd EtcdMembers
	// Reset 执行本机重置；nil 时调用 internal/reset。
	Reset ResetFunc
}

// Result 是 Leave 的执行结果。
type Result struct {
	// NodeName 是目标节点名。
	NodeName string `json:"nodeName"`
	// EtcdMemberRemoved 表示 etcd 成员已被移除。
	EtcdMemberRemoved bool `json:"etcdMemberRemoved,omitempty"`
	// EtcdMemberID 是被移除的成员 ID。
	EtcdMemberID uint64 `json:"etcdMemberID,omitempty"`
	// NodeDeleted 表示 Node 对象已删除。
	NodeDeleted bool `json:"nodeDeleted,omitempty"`
	// Reset 表示本机 reset 已执行（仅 Leave）。
	Reset bool `json:"reset,omitempty"`
	// Skipped 列出被跳过的动作及原因。
	Skipped []string `json:"skipped,omitempty"`
	// Warnings 是需要人工留意的提示。
	Warnings []string `json:"warnings,omitempty"`
}

// Leave 让本节点退出集群。
// 顺序：cordon → 删除 Node 对象 → etcd 成员移除（校验法定人数）
// → 提示从 cluster.yaml 删除节点条目 → 本机 reset。
func Leave(ctx context.Context, opts *Options) (*Result, error) {
	o, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	res := &Result{NodeName: o.NodeName}

	// 步骤 2：cordon；驱逐由运维在执行前用 kubectl drain 完成。
	if o.DryRun {
		o.Report.Infof("将把节点 %s 标记为不可调度（dry-run）", o.NodeName)
	} else if err := cordonNode(ctx, o.Client, o.NodeName); err != nil {
		if !o.Force {
			return res, fmt.Errorf("cordon 节点 %s 失败（--force-leave 可跳过）：%w", o.NodeName, err)
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("cordon 失败（--force-leave 继续）：%v", err))
		o.Report.Warnf("cordon 失败（--force-leave 继续）：%v", err)
	} else {
		o.Report.Infof("已把节点 %s 标记为不可调度", o.NodeName)
		warnPodsOnNode(ctx, o, o.NodeName)
	}

	// 先停 kubelet，避免删除 Node 后 kubelet 立刻重新注册；随后
	// removeFromCluster 依次删除 Node、写配置、移除 etcd 成员。
	if !o.DryRun && o.StopKubelet != nil {
		if err := o.StopKubelet(ctx); err != nil {
			return res, fmt.Errorf("停止 kubelet 失败，已中止退群: %w", err)
		}
		o.Report.Infof("已停止 kubelet（避免 Node 重新注册）")
	}

	// 步骤 3~5：Node 对象、etcd 成员、配置提示。
	if err := removeFromCluster(ctx, o, res); err != nil {
		return res, err
	}

	// 步骤 6：本机 reset。
	if err := o.Reset(ctx, o); err != nil {
		return res, err
	}
	res.Reset = true
	return res, nil
}

// removeFromCluster 执行集群侧清理：Node 对象 → 配置提示 → etcd 成员。
//
// 顺序是刻意的：先删 Node，最后移除 etcd 成员。移除成员后 apiserver 可能
// 立即失去存储，因此它是最后一步；所有步骤都对 NotFound/已移除幂等。
func removeFromCluster(ctx context.Context, o *Options, res *Result) error {
	// Node 对象。
	if o.DryRun {
		o.Report.Infof("将删除 Node 对象 %s（dry-run）", o.NodeName)
	} else if err := deleteNodeObject(ctx, o.Client, o.NodeName); err != nil {
		return err
	} else {
		res.NodeDeleted = true
		o.Report.Infof("已删除 Node 对象 %s", o.NodeName)
	}

	// 提示运维从 cluster.yaml 删除节点条目：配置不是集群内副本，
	// cordon/Node/etcd 仍是 leave 的职责，文件编辑留给持有配置文件的人。
	res.Skipped = append(res.Skipped,
		fmt.Sprintf("集群配置是本地文件：请从 cluster.yaml 删除主机 %s 的条目", o.NodeName))
	o.Report.Infof("请从 cluster.yaml 删除主机 %s 的条目；kubemono 不维护集群内配置副本", o.NodeName)

	// etcd 成员（法定人数校验）。
	if o.RemoveEtcdMember {
		if err := removeEtcdMember(ctx, o, res); err != nil {
			return err
		}
	} else {
		res.Skipped = append(res.Skipped, "本节点不是 etcd 成员，跳过 etcd 成员移除")
	}
	return nil
}

// removeEtcdMember 移除本机（或目标节点）的 etcd 成员，并校验法定人数。
func removeEtcdMember(ctx context.Context, o *Options, res *Result) error {
	if o.Etcd == nil {
		return errors.New("需要 etcd 成员客户端才能移除 etcd 成员：请确认本机是 etcd 成员且 PKI 可用")
	}
	members, err := o.Etcd.List(ctx)
	if err != nil {
		return err
	}
	// 复用 internal/etcd 的匹配语义（先名字、再 peer URL 集合）。
	// 这里只按节点名查找：kubemono 用节点名作 etcd --name。不做子串地址匹配
	// （本机 10.0.0.1 会误匹配另一台 10.0.0.11）；生产路径不传地址，名字匹配足够。
	member, found := etcd.FindMember(members, o.NodeName, nil)
	if !found {
		res.Skipped = append(res.Skipped, fmt.Sprintf("etcd 成员列表中没有 %s，跳过", o.NodeName))
		o.Report.Infof("etcd 成员列表中没有 %s，跳过成员移除", o.NodeName)
		return nil
	}
	// 单成员 etcd 无法通过 MemberRemove 移除自己（etcd 会拒绝
	// "not enough started members"）。--force-leave 下把“清空本机”作为目标，
	// 跳过成员删除并继续配置清理 + reset；否则单节点集群永远 reset 不掉。
	if o.Force && len(members) == 1 {
		res.Skipped = append(res.Skipped, fmt.Sprintf("etcd 只有 1 个成员 %s，--force-leave 跳过成员移除", member.Name))
		o.Report.Warnf("etcd 只有 1 个成员 %s，--force-leave 跳过成员移除，继续清理本机", member.Name)
		return nil
	}
	if !o.Force {
		if err := o.Etcd.CanRemove(ctx, member.ID); err != nil {
			return err
		}
	}
	if o.DryRun {
		o.Report.Infof("将移除 etcd 成员 %s（ID=%d，dry-run）", member.Name, member.ID)
		return nil
	}
	if err := o.Etcd.Remove(ctx, member.ID); err != nil {
		return err
	}
	res.EtcdMemberRemoved = true
	res.EtcdMemberID = member.ID
	o.Report.Infof("已移除 etcd 成员 %s（ID=%d）", member.Name, member.ID)
	return nil
}

// withDefaults 校验必需项并补齐默认值。
// 集群凭据在最前面校验：leave 是集群级操作，
// 没有 admin kubeconfig 就必须在改动任何东西之前失败。
func (o *Options) withDefaults() (*Options, error) {
	if o == nil {
		return nil, errors.New("leave: Options 为空")
	}
	if o.Client == nil {
		return nil, ErrNoClient
	}
	cp := *o
	if cp.NodeName == "" {
		name, err := localNodeName(cp.Paths)
		if err != nil {
			return nil, err
		}
		cp.NodeName = name
	}
	if cp.Report == nil {
		cp.Report = output.Discard()
	}
	if cp.Reset == nil {
		cp.Reset = defaultReset
	}
	return &cp, nil
}

// localNodeName 从本机身份文件读取节点名。
func localNodeName(paths constants.Paths) (string, error) {
	node, err := identity.NewStore(paths.Kubemono()).Load()
	if err != nil {
		return "", fmt.Errorf("读取本机身份 %s 失败: %w", paths.NodeConfig(), err)
	}
	if node == nil || node.NodeName == "" {
		return "", fmt.Errorf("无法确定本机节点名：%s 不存在或不含 nodeName；请显式指定节点名", paths.NodeConfig())
	}
	return node.NodeName, nil
}

// defaultReset 用 internal/reset 执行本机重置。
func defaultReset(ctx context.Context, o *Options) error {
	_, err := kubemonoreset.Run(ctx, &kubemonoreset.Options{
		Paths:             o.Paths,
		RuntimeType:       o.RuntimeType,
		KeepData:          o.KeepData,
		KeepHostInit:      o.KeepHostInit,
		Purge:             o.Purge,
		ContainerdDataDir: o.ContainerdDataDir,
		RuntimeUnit:       o.RuntimeUnit,
		DryRun:            o.DryRun,
		Report:            o.Report,
	})
	return err
}
