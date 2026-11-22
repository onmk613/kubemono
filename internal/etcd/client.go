package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"kubemono/internal/constants"
	"kubemono/internal/util/output"
)

// 默认超时：etcd 单次请求（含每个 endpoint 的 dial）不应该无限阻塞，
// 否则调用方的 etcd 巡检会卡住。
const (
	// DefaultDialTimeout 是建立连接的默认超时。
	DefaultDialTimeout = 5 * time.Second
	// DefaultRequestTimeout 是单次 RPC 的默认超时。
	DefaultRequestTimeout = 10 * time.Second
)

// Config 是构造 Client 的参数。
// 在线部分（Endpoints + TLS + 可选用户名口令）来自 的形态。
type Config struct {
	// Endpoints 是 etcd 的 client URL 列表。
	Endpoints []string
	// TLS 是访问 etcd 的 CA 与客户端证书。
	TLS TLSFiles
	// DialTimeout / RequestTimeout 覆盖默认超时。
	DialTimeout    time.Duration
	RequestTimeout time.Duration
	// Report 接收进度与警告（例如“成员数为偶数”）。
	Report output.Reporter
	// Now 返回当前时间（健康探测的延迟计时）。
	Now func() time.Time
}

// API 是 kubemono 需要的 etcd 能力集合。
// phase 依赖接口而不是 *Client，因此测试可以注入 fake；
// *Client 是唯一的真实实现（薄封装 clientv3）。
type API interface {
	// Health 探测每个 endpoint 并返回健康结论。
	Health(ctx context.Context) ([]MemberHealth, error)
	// Members 列出成员（含 learner 标记）。
	Members(ctx context.Context) ([]Member, error)
	// AddLearner 以 learner 身份加入成员；已存在时返回其 ID（幂等）。
	AddLearner(ctx context.Context, name string, peerURLs []string) (uint64, error)
	// Promote 把 learner 提升为投票成员；已是投票成员时返回 nil（幂等）。
	Promote(ctx context.Context, id uint64) error
	// RemoveMember 移除成员；已移除时返回 nil（幂等）。
	RemoveMember(ctx context.Context, id uint64) error
	// Status 返回一个 endpoint 的状态。
	Status(ctx context.Context, endpoint string) (Status, error)
	// CanRemoveMember 校验移除指定成员是否安全（learner 或已移除时总是安全）。
	CanRemoveMember(ctx context.Context, id uint64) error
	// Close 释放连接。
	Close() error
}

// rpc 是 Client 用到的 clientv3 子集。
// 把 RPC 面收窄成一个接口，单测就能用 fake 覆盖成员解析、幂等与法定人数校验，
// 不需要真实 etcd；生产实现永远是 *clientv3.Client。
type rpc interface {
	// MemberList 带 OpOption：etcd 3.7 的 clientv3 在这里新增了可变参数
	// （过滤/超时选项），kubemono 只调用不带选项的形式。
	MemberList(ctx context.Context, opts ...clientv3.OpOption) (*clientv3.MemberListResponse, error)
	MemberAddAsLearner(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error)
	MemberPromote(ctx context.Context, id uint64) (*clientv3.MemberPromoteResponse, error)
	MemberRemove(ctx context.Context, id uint64) (*clientv3.MemberRemoveResponse, error)
	Status(ctx context.Context, endpoint string) (*clientv3.StatusResponse, error)
	Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error)
	Close() error
}

// Client 是 clientv3 的薄封装。
type Client struct {
	cfg Config
	cli rpc
}

// New 构造 etcd 客户端。
func New(cfg Config) (*Client, error) {
	cfg.applyDefaults()
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("etcd: endpoint 列表为空：stacked 需要 spec.hosts 里的 master，external 需要 controlPlane.etcd.external.endpoints")
	}
	tlsCfg, err := tlsConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Endpoints,
		DialTimeout: cfg.DialTimeout,
		TLS:         tlsCfg,
	})
	if err != nil {
		return nil, fmt.Errorf("构造 etcd 客户端失败（endpoints %v）：%w；确认 etcd 已启动、证书与端口 2379 可达", cfg.Endpoints, err)
	}
	return &Client{cfg: cfg, cli: cli}, nil
}

// applyDefaults 填齐可选字段。
func (c *Config) applyDefaults() {
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.Report == nil {
		c.Report = output.Discard()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Close 实现 API。
func (c *Client) Close() error {
	if c.cli == nil {
		return nil
	}
	return c.cli.Close()
}

// requestCtx 为单次 RPC 设置超时上界。
func (c *Client) requestCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || c.cfg.RequestTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.cfg.RequestTimeout)
}

// Members 实现 API。
func (c *Client) Members(ctx context.Context) ([]Member, error) {
	ctx, cancel := c.requestCtx(ctx)
	defer cancel()
	resp, err := c.cli.MemberList(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取 etcd 成员列表失败（endpoints %v）：%w；先执行 `etcdctl endpoint health` 判断集群是否可用",
			c.cfg.Endpoints, err)
	}
	out := make([]Member, 0, len(resp.Members))
	for _, m := range resp.Members {
		out = append(out, Member{
			ID:         uint64(m.ID),
			Name:       m.Name,
			PeerURLs:   append([]string(nil), m.PeerURLs...),
			ClientURLs: append([]string(nil), m.ClientURLs...),
			IsLearner:  m.IsLearner,
		})
	}
	sortMembers(out)
	return out, nil
}

// Status 实现 API。
func (c *Client) Status(ctx context.Context, endpoint string) (Status, error) {
	ctx, cancel := c.requestCtx(ctx)
	defer cancel()
	resp, err := c.cli.Status(ctx, endpoint)
	if err != nil {
		return Status{Endpoint: endpoint}, fmt.Errorf("读取 %s 的状态失败：%w", endpoint, err)
	}
	return statusFromResponse(endpoint, resp), nil
}

// statusFromResponse 转换 clientv3 的状态响应。
func statusFromResponse(endpoint string, resp *clientv3.StatusResponse) Status {
	return Status{
		Endpoint:         endpoint,
		MemberID:         uint64(resp.Header.GetMemberId()),
		Version:          resp.Version,
		DBSize:           resp.DbSize,
		Leader:           resp.Leader,
		RaftIndex:        resp.RaftIndex,
		RaftTerm:         resp.RaftTerm,
		RaftAppliedIndex: resp.RaftAppliedIndex,
		IsLearner:        resp.IsLearner,
		Errors:           append([]string(nil), resp.Errors...),
		Healthy:          len(resp.Errors) == 0,
	}
}

// Health 实现 API。
// 每个 endpoint 做两件事：
// 1. Status RPC：确认成员存活、拿到成员 ID、raft index 与自报错误；
// 2. 一次线性一致读（默认 Get）：只有能参与法定人数的成员才会成功，
// 这是“endpoint health”与“进程还在”的区别（与 etcdctl 一致）。
// 客户端证书没有读权限时 etcd 返回 permission denied：那依然证明成员在服务，
// 因此按健康处理。这里不 import rpctypes，用错误文本判断，代价是极端情况下
// 可能误判——健康结论只用于观察与准入校验，误判方向是“更严格”，可以接受。
func (c *Client) Health(ctx context.Context) ([]MemberHealth, error) {
	members, err := c.Members(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[uint64]Member{}
	for _, m := range members {
		byID[m.ID] = m
	}
	out := make([]MemberHealth, 0, len(c.cfg.Endpoints))
	for _, endpoint := range c.cfg.Endpoints {
		out = append(out, c.probeEndpoint(ctx, endpoint, byID))
	}
	return out, nil
}

// probeEndpoint 探测一个 endpoint 并聚合出健康结论（Health 的循环体）。
func (c *Client) probeEndpoint(ctx context.Context, endpoint string, byID map[uint64]Member) MemberHealth {
	start := c.cfg.Now()
	entry := MemberHealth{Endpoint: endpoint, Role: RoleVoting}
	st, err := c.Status(ctx, endpoint)
	entry.LatencyMS = c.cfg.Now().Sub(start).Milliseconds()
	if err != nil {
		entry.Healthy = false
		entry.Error = err.Error()
		return entry
	}
	entry.MemberID = st.MemberID
	entry.Status = &st
	if m, ok := byID[st.MemberID]; ok {
		entry.Member = m.Name
		entry.Role = m.Role()
	}
	entry.Healthy = st.Healthy
	if !st.Healthy {
		entry.Error = strings.Join(st.Errors, "; ")
		return entry
	}
	readCtx, readCancel := c.requestCtx(ctx)
	_, readErr := c.cli.Get(readCtx, "health")
	readCancel()
	if readErr != nil && !isPermissionDenied(readErr) {
		entry.Healthy = false
		entry.Error = fmt.Sprintf("线性一致读失败：%v", readErr)
	}
	return entry
}

// isPermissionDenied 报告鉴权拒绝（见 Health 的说明）。
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "permission denied")
}

// retryIdempotent 处理成员变更 RPC 的“响应丢失”情形：请求报错后立即重查
// 成员列表，幂等成立（变更其实已经生效）就按成功处理并发警告，否则返回
// fallback。AddLearner/Promote/RemoveMember 三处共用这套逻辑，
// 判定条件与文案通过回调表达。
func (c *Client) retryIdempotent(ctx context.Context, settled func(members []Member) bool, warn func(), fallback error) error {
	if again, lerr := c.Members(context.WithoutCancel(ctx)); lerr == nil && settled(again) {
		warn()
		return nil
	}
	return fallback
}

// AddLearner 实现 API。
func (c *Client) AddLearner(ctx context.Context, name string, peerURLs []string) (uint64, error) {
	if name == "" {
		return 0, errors.New("etcd: 成员名为空：请用 --node 指定本机在集群配置中的名字")
	}
	if len(peerURLs) == 0 {
		return 0, fmt.Errorf("etcd: 成员 %s 没有 peer URL：检查本机地址与端口 %d", name, constants.EtcdPeerPort)
	}
	cur, err := c.Members(ctx)
	if err != nil {
		return 0, err
	}
	if m, ok := FindMember(cur, name, peerURLs); ok {
		c.cfg.Report.Infof("etcd 成员 %s（id %x）已存在，跳过 learner 加入", memberLabel(m), m.ID)
		return m.ID, nil
	}
	c.warnEvenMembers(cur)
	ctx, cancel := c.requestCtx(ctx)
	defer cancel()
	resp, err := c.cli.MemberAddAsLearner(ctx, peerURLs)
	if err != nil {
		// 响应可能在网络层丢失：重新查一次，成员已存在就是成功。
		var added Member
		rerr := c.retryIdempotent(ctx,
			func(again []Member) bool {
				var ok bool
				added, ok = FindMember(again, name, peerURLs)
				return ok
			},
			func() {
				c.cfg.Report.Warnf("learner 加入请求返回错误但成员已存在（id %x）：按成功处理（%v）", added.ID, err)
			},
			fmt.Errorf("加入 learner %s（%v）失败：%w；确认集群健康（`etcdctl endpoint health`）且 spec.hosts 中的地址就是本机地址",
				name, peerURLs, err))
		if rerr == nil {
			return added.ID, nil
		}
		return 0, rerr
	}
	id := uint64(resp.Member.ID)
	c.cfg.Report.Infof("已把 %s（id %x）以 learner 身份加入集群；追平后再 promote（etcd phase 会自动完成）", name, id)
	return id, nil
}

// warnEvenMembers 在成员数为偶数时给出警告。
// 要求“每次只增减一个成员”：偶数成员的容错是 n/2-1，
// 加入一个成员会让它在“先降后升”的区间里抖动，因此只警告、不拒绝
// （拒绝会把运维逼到手工改集群状态，更危险）。
func (c *Client) warnEvenMembers(members []Member) {
	voters := 0
	for _, m := range members {
		if !m.IsLearner {
			voters++
		}
	}
	if voters == 0 {
		return
	}
	if voters%2 == 0 {
		c.cfg.Report.Warnf("当前投票成员数为偶数（%d），容错为 %d；加入一个成员后会变为奇数（%d），容错提升，请确认这是一次单成员变更",
			voters, voters/2-1, voters+1)
		return
	}
	c.cfg.Report.Warnf("加入后投票成员数将变为偶数（%d），容错从 %d 降为 %d；如需扩容请一次只加一个并在追平后 promote",
		voters+1, voters/2, (voters+1)/2-1)
}

// Promote 实现 API。
func (c *Client) Promote(ctx context.Context, id uint64) error {
	members, err := c.Members(ctx)
	if err != nil {
		return err
	}
	if m, ok := MemberByID(members, id); ok && !m.IsLearner {
		c.cfg.Report.Infof("etcd 成员 %s（id %x）已经是投票成员，无需 promote", memberLabel(m), id)
		return nil
	}
	ctx, cancel := c.requestCtx(ctx)
	defer cancel()
	if _, err := c.cli.MemberPromote(ctx, id); err != nil {
		return c.retryIdempotent(ctx,
			func(again []Member) bool {
				m, ok := MemberByID(again, id)
				return ok && !m.IsLearner
			},
			func() {
				c.cfg.Report.Warnf("promote 请求返回错误但成员已是投票成员（id %x）：按成功处理（%v）", id, err)
			},
			fmt.Errorf("promote 成员 %x 失败：%w；learner 尚未追平时 etcd 会拒绝，请稍后重试（`etcdctl member list` 可查看 RaftIndex）", id, err))
	}
	c.cfg.Report.Infof("成员 %x 已 promote 为投票成员", id)
	return nil
}

// RemoveMember 实现 API。
func (c *Client) RemoveMember(ctx context.Context, id uint64) error {
	members, err := c.Members(ctx)
	if err != nil {
		return err
	}
	m, ok := MemberByID(members, id)
	if !ok {
		c.cfg.Report.Infof("etcd 成员 %x 已不在集群中，跳过移除", id)
		return nil
	}
	ctx, cancel := c.requestCtx(ctx)
	defer cancel()
	if _, err := c.cli.MemberRemove(ctx, id); err != nil {
		return c.retryIdempotent(ctx,
			func(again []Member) bool {
				_, still := MemberByID(again, id)
				return !still
			},
			func() {
				c.cfg.Report.Warnf("移除请求返回错误但成员已不在集群中（id %x）：按成功处理（%v）", id, err)
			},
			fmt.Errorf("移除成员 %s（id %x）失败：%w；先在集群上执行 `etcdctl endpoint health` 确认法定人数", memberLabel(m), id, err))
	}
	c.cfg.Report.Infof("已移除 etcd 成员 %s（id %x）；请在被移除节点上执行 kubemono reset 清理数据目录", memberLabel(m), id)
	return nil
}

// CanRemoveMember 实现 API：移除指定成员前校验。
func (c *Client) CanRemoveMember(ctx context.Context, id uint64) error {
	members, err := c.Members(ctx)
	if err != nil {
		return err
	}
	m, ok := MemberByID(members, id)
	if !ok {
		// 已经不在集群里：移除是幂等的，安全。
		return nil
	}
	return c.checkRemoval(ctx, members, m)
}

// checkRemoval 实现法定人数与安全校验。
// 规则（target 是确定要移除的成员）：
// 1. 不能移除最后一个投票成员（剩余 0 个成员时数据不可访问）；
// 2. 剩余 ≥2 时要求剩余健康成员 ≥2（单点健康时移除会直接失去容错）；
// 3. 剩余健康成员必须仍构成新集群的法定人数（majority）；
// 4. 剩余成员数为偶数、或只剩 1 个成员时给出警告（不拒绝）——
// 的“每次只增减一个成员”由调用方保证，这里只提示风险。
func (c *Client) checkRemoval(ctx context.Context, members []Member, target Member) error {
	if target.IsLearner {
		// learner 不参与投票，移除它不影响法定人数。
		return nil
	}
	health, err := c.healthByMember(ctx)
	if err != nil {
		return err
	}
	voters := 0
	healthyVoters := 0
	for _, m := range members {
		if m.IsLearner {
			continue
		}
		voters++
		if health[m.ID] {
			healthyVoters++
		}
	}
	remaining := voters - 1
	healthyRemaining := healthyVoters
	if health[target.ID] {
		healthyRemaining--
	}
	if healthyRemaining < 0 {
		healthyRemaining = 0
	}
	switch {
	case remaining < 1:
		return errors.New("不能移除最后一个投票成员：剩余 0 个成员时集群数据将无法访问；如需下线整个集群请逐台执行 kubemono reset")
	case remaining >= 2 && healthyRemaining < 2:
		return fmt.Errorf("移除后只剩 %d 个健康投票成员（要求 ≥2）：先修复不健康成员（`etcdctl endpoint health`），再执行移除", healthyRemaining)
	case healthyRemaining < quorum(remaining):
		return fmt.Errorf("移除后健康成员 %d 个少于新集群的法定人数 %d：先修复不健康成员（`etcdctl endpoint health`），或在 master 上手动 `etcdctl member remove` + `kubectl delete node` 处理已损坏节点",
			healthyRemaining, quorum(remaining))
	}
	if remaining == 1 {
		c.cfg.Report.Warnf("移除后只剩 1 个投票成员，集群没有任何容错；只建议用于单节点环境")
	}
	if remaining%2 == 0 {
		c.cfg.Report.Warnf("移除后投票成员数为偶数（%d），容错降低；建议保持奇数（3 或 5），并一次只移除一个成员", remaining)
	}
	return nil
}

// healthByMember 返回成员 ID → 是否健康（依据配置里的 endpoints）。
// 只统计配置范围内的 endpoint：不在配置里的成员无法探测，按“不健康”处理，
// 这样校验会偏保守（宁可拒绝，也不要移除后失去法定人数）。
func (c *Client) healthByMember(ctx context.Context) (map[uint64]bool, error) {
	out := map[uint64]bool{}
	reachable := 0
	var lastErr error
	for _, endpoint := range c.cfg.Endpoints {
		st, err := c.Status(ctx, endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		reachable++
		out[st.MemberID] = st.Healthy
	}
	if reachable == 0 {
		return nil, fmt.Errorf("无法访问任何 etcd endpoint（%v）：%w；集群不可达时不能安全地变更成员", c.cfg.Endpoints, lastErr)
	}
	return out, nil
}

// 纯成员工具（quorum/FindMember/MemberByID/SamePeerURL/InitialCluster/
// PeerURLs/ClientURLs/sortMembers/memberLabel）在 members.go。
