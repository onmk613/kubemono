package etcd

import (
	"context"
	"errors"
	"fmt"
	"time"

	etcdapi "kubemono/internal/etcd"
	"kubemono/internal/phase"
)

// runJoin 把本机作为 learner 加入已有集群，追平后 promote。
// 如果 AddLearner 之后任一步失败，会尽力删除刚加入的 learner：etcd 同一时刻
// 只允许一个 learner，残留会让下一次 master join 直接失败。
func (p *Phase) runJoin(ctx context.Context, c *phase.Context, prov etcdapi.Provider) (err error) {
	local := prov.Local()
	if !local.Member {
		return fmt.Errorf("本机 %q 在配置里不是 etcd 成员（%s）：本地 etcd 需要 master 角色", local.Name, prov.Form())
	}
	if !configuredMember(prov, local.Name) &&
		(local.Name != c.NodeName() || !configuredMember(prov, c.ConfigName())) {
		return fmt.Errorf("本机 %q 不在集群配置声明的 etcd 成员里（%v）：先在 cluster.yaml 的 cluster.hosts 里加入该节点并下发新配置，再执行 etcd join",
			local.Name, prov.ConfiguredNames())
	}
	cli, err := p.client(c, prov)
	if err != nil {
		return err
	}
	members, err := cli.Members(ctx)
	if err != nil {
		return fmt.Errorf("%w；加入前必须能从已有成员读取集群状态（先在任一 master 上用 etcdctl（带集群证书）执行 endpoint health）", err)
	}
	member, isMember := etcdapi.FindMember(members, local.Name, local.PeerURLs)
	if c.DryRun {
		// dry-run 必须在 AddLearner 之前返回：加入成员是集群级写操作，
		// 预览不能改变成员列表。
		if isMember {
			c.Reporter().Infof("dry-run：成员 %s（id %x）已存在，将写入 %s（initial-cluster-state=existing）",
				local.Name, member.ID, p.localArtifactPath(c))
		} else {
			c.Reporter().Infof("dry-run：将添加 learner %s，写入 %s（initial-cluster-state=existing）并等待追平后 promote",
				local.Name, p.localArtifactPath(c))
		}
		return nil
	}
	var id uint64
	addedLearner := false
	switch {
	case isMember:
		id = member.ID
		if member.IsLearner {
			c.Reporter().Infof("learner %s（id %x）已存在，继续等待追平并 promote", local.Name, id)
		} else {
			c.Reporter().Infof("成员 %s（id %x）已是投票成员，只做启动与健康检查", local.Name, id)
		}
	default:
		id, err = cli.AddLearner(ctx, local.Name, local.PeerURLs)
		if err != nil {
			return err
		}
		addedLearner = true
	}
	if addedLearner {
		defer func() {
			if err == nil {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if cleanupErr := cli.RemoveMember(cleanupCtx, id); cleanupErr != nil {
				c.Reporter().Warnf("清理未完成的 learner %x 失败：%v；请手工执行 etcdctl member remove %x 后再重试", id, cleanupErr, id)
				return
			}
			c.Reporter().Infof("已清理未完成的 learner %x", id)
		}()
	}
	// 重新读取成员列表：learner 已经出现在集群里，--initial-cluster 必须包含它。
	file, content, err := p.expectedArtifact(ctx, c, prov, cli)
	if err != nil {
		return err
	}
	if err := p.startLocal(ctx, c, prov, file, content); err != nil {
		return err
	}
	if err := p.waitHealthyLocal(ctx, c, prov, cli); err != nil {
		return err
	}
	if err := phase.ClearRestartMarker(c, restartMarkerName); err != nil {
		return err
	}
	if isMember && !member.IsLearner {
		c.Reporter().Infof("成员 %s 已经是投票成员，无需 promote", local.Name)
		return nil
	}
	return p.waitAndPromote(ctx, c, prov, cli, id)
}

// waitAndPromote 等待 learner 追平后 promote。
// 追平判据：本机 Status.RaftIndex ≥ leader 的 RaftIndex（leader 不可达时用
// 可达成员里最大的 RaftIndex）。轮询是有界的：Timeout/PollInterval 决定最大轮数，
// Sleep 可注入，因此测试不会真正等待。
func (p *Phase) waitAndPromote(ctx context.Context, c *phase.Context, prov etcdapi.Provider, cli etcdapi.API, id uint64) error {
	local := prov.Local()
	opts := p.pollOptions(c)
	timeout := opts.Timeout
	err := phase.Poll(ctx, opts, func() (bool, error) {
		if members, err := cli.Members(ctx); err == nil {
			if m, ok := etcdapi.MemberByID(members, id); ok && !m.IsLearner {
				c.Reporter().Infof("成员 %x 已是投票成员", id)
				return true, nil
			}
		}
		target, targetErr := p.leaderRaftIndex(ctx, cli, prov)
		if targetErr != nil {
			return false, targetErr
		}
		st, err := cli.Status(ctx, local.ClientURL)
		if err != nil {
			return false, err
		}
		if st.RaftIndex < target {
			c.Reporter().Debugf("等待追平：本机 raftIndex=%d，leader=%d", st.RaftIndex, target)
			return false, fmt.Errorf("本机 raftIndex %d 落后 leader %d", st.RaftIndex, target)
		}
		if err := cli.Promote(ctx, id); err != nil {
			return false, err
		}
		c.Reporter().Infof("成员 %x 已 promote 为投票成员（raftIndex=%d）", id, st.RaftIndex)
		return true, nil
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("等待 learner 追平并 promote 超时（%s）：%w；用 etcdctl member list / endpoint status 查看 RaftIndex 差距，或执行 etcdctl endpoint health 后重试（join 幂等）",
		timeout, err)
}

// leaderRaftIndex 返回 leader 当前的 RaftIndex。
// 直接问 leader（Status 的 MemberID == Leader）最准确；leader 不可达时退化为
// “可达成员的最大 RaftIndex”，promote 时若还没追平会在下一轮重试。
func (p *Phase) leaderRaftIndex(ctx context.Context, cli etcdapi.API, prov etcdapi.Provider) (uint64, error) {
	cfg, err := prov.ClientConfig()
	if err != nil {
		return 0, err
	}
	var best uint64
	var lastErr error
	for _, endpoint := range cfg.Endpoints {
		st, err := cli.Status(ctx, endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		if st.Leader != 0 && st.MemberID == st.Leader {
			return st.RaftIndex, nil
		}
		if st.RaftIndex > best {
			best = st.RaftIndex
		}
	}
	if best > 0 {
		return best, nil
	}
	if lastErr == nil {
		lastErr = errors.New("所有 endpoint 都没有报告 leader")
	}
	return 0, fmt.Errorf("无法读取 leader 的 raftIndex：%w", lastErr)
}
