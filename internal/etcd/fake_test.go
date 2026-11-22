package etcd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// captureReporter 记录 phase/客户端发出的日志，便于断言警告内容。
type captureReporter struct {
	mu     sync.Mutex
	infos  []string
	warns  []string
	errs   []string
	debugs []string
}

func (r *captureReporter) log(dst *[]string, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*dst = append(*dst, fmt.Sprintf(format, args...))
}

func (r *captureReporter) Debugf(format string, args ...any) { r.log(&r.debugs, format, args...) }
func (r *captureReporter) Infof(format string, args ...any)  { r.log(&r.infos, format, args...) }
func (r *captureReporter) Warnf(format string, args ...any)  { r.log(&r.warns, format, args...) }
func (r *captureReporter) Errorf(format string, args ...any) { r.log(&r.errs, format, args...) }
func (r *captureReporter) Step(string) func(error)           { return func(error) {} }
func (r *captureReporter) Result(bool, string, ...any)       {}

// containsWarn 报告某条警告是否包含 sub。
func (r *captureReporter) containsWarn(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.warns {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// all 返回全部捕获到的日志行，便于调试失败的测试。
func (r *captureReporter) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(append(append(append([]string{}, r.debugs...), r.infos...), append(r.warns, r.errs...)...), "\n")
}

// simMember 是模拟集群里的一个成员。
type simMember struct {
	id        uint64
	name      string
	peerURL   string
	clientURL string
	learner   bool
	healthy   bool
	raftIndex uint64
	dbSize    int64
	version   string
	// statusErr 非空时该成员的 Status RPC 报错。
	statusErr error
	// errors 是 Status 响应里的自报错误。
	errors []string
}

// simRPC 是一个最小的 etcd 集群模拟：成员表 + 每成员状态 + 调用记录。
// 覆盖 AddLearner → Promote 的调用序列与幂等、RemoveMember 的法定人数校验、
// 健康/成员解析；所有测试都是 hermetic 的（无网络、无真实 etcd）。
type simRPC struct {
	mu      sync.Mutex
	members []simMember
	calls   []string

	leaderID uint64

	getErr        error
	addErr        error
	addErrButAdds bool
	promoteErr    error
	promoteLate   bool
	removeErr     error
	removeLate    bool
	defragErr     error

	alarms []*pb.AlarmMember

	promoteCalls    int
	removeCalls     int
	defragCalls     []string
	snapshotData    []byte
	snapshotErr     error
	snapshotReadErr error
}

// errAfterReader 先返回数据，再返回错误：用来验证快照写盘的失败清理。
type errAfterReader struct {
	data []byte
	err  error
	done bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

func (r *errAfterReader) Close() error { return nil }

func newSim(members ...simMember) *simRPC {
	s := &simRPC{snapshotData: []byte("snapshot-bytes")}
	for _, m := range members {
		if m.clientURL == "" {
			m.clientURL = fmt.Sprintf("https://10.0.0.%d:%d", m.id, 2379)
		}
		if m.peerURL == "" {
			m.peerURL = fmt.Sprintf("https://10.0.0.%d:%d", m.id, 2380)
		}
		if m.name == "" && !m.learner {
			m.name = fmt.Sprintf("m%d", m.id)
		}
		if m.version == "" {
			m.version = "3.5.15"
		}
		if m.raftIndex == 0 {
			m.raftIndex = 100
		}
		s.members = append(s.members, m)
	}
	if len(s.members) > 0 {
		s.leaderID = s.members[0].id
	}
	return s
}

// endpoints 返回全部成员的 client URL。
func (s *simRPC) endpoints() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.members))
	for _, m := range s.members {
		out = append(out, m.clientURL)
	}
	return out
}

func (s *simRPC) record(call string) {
	s.calls = append(s.calls, call)
}

// Calls 返回记录到的 RPC 名称。
func (s *simRPC) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// MemberIDs 返回当前的成员 ID。
func (s *simRPC) MemberIDs() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, 0, len(s.members))
	for _, m := range s.members {
		out = append(out, m.id)
	}
	return out
}

// MemberList 实现 rpc。
func (s *simRPC) MemberList(context.Context, ...clientv3.OpOption) (*clientv3.MemberListResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("MemberList")
	resp := &clientv3.MemberListResponse{}
	for _, m := range s.members {
		resp.Members = append(resp.Members, simMemberPB(m))
	}
	return resp, nil
}

// MemberAddAsLearner 实现 rpc。
func (s *simRPC) MemberAddAsLearner(_ context.Context, peerURLs []string) (*clientv3.MemberAddResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("MemberAddAsLearner")
	if s.addErr != nil {
		if s.addErrButAdds {
			// 模拟“RPC 成功但响应丢失”：成员已经进集群。
			s.members = append(s.members, simMember{
				id:        s.nextIDLocked(),
				peerURL:   peerURLs[0],
				clientURL: fmt.Sprintf("https://%s:%d", hostOf(peerURLs[0]), 2379),
				learner:   true,
			})
		}
		return nil, s.addErr
	}
	m := simMember{
		id:        s.nextIDLocked(),
		peerURL:   peerURLs[0],
		clientURL: fmt.Sprintf("https://%s:%d", hostOf(peerURLs[0]), 2379),
		learner:   true,
	}
	s.members = append(s.members, m)
	pbm := simMemberPB(m)
	return &clientv3.MemberAddResponse{Member: pbm, Members: s.membersPB()}, nil
}

// MemberPromote 实现 rpc。
func (s *simRPC) MemberPromote(_ context.Context, id uint64) (*clientv3.MemberPromoteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("MemberPromote")
	s.promoteCalls++
	if s.promoteErr != nil {
		if s.promoteLate {
			s.setLearnerLocked(id, false)
		}
		return nil, s.promoteErr
	}
	s.setLearnerLocked(id, false)
	return &clientv3.MemberPromoteResponse{Members: s.membersPB()}, nil
}

// MemberRemove 实现 rpc。
func (s *simRPC) MemberRemove(_ context.Context, id uint64) (*clientv3.MemberRemoveResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("MemberRemove")
	s.removeCalls++
	if s.removeErr != nil {
		if s.removeLate {
			s.dropLocked(id)
		}
		return nil, s.removeErr
	}
	s.dropLocked(id)
	return &clientv3.MemberRemoveResponse{Members: s.membersPB()}, nil
}

// MoveLeader 实现 rpc。
func (s *simRPC) MoveLeader(_ context.Context, id uint64) (*clientv3.MoveLeaderResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("MoveLeader")
	s.leaderID = id
	return &clientv3.MoveLeaderResponse{}, nil
}

// Status 实现 rpc。
func (s *simRPC) Status(_ context.Context, endpoint string) (*clientv3.StatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("Status")
	for _, m := range s.members {
		if m.clientURL != endpoint {
			continue
		}
		if m.statusErr != nil {
			return nil, m.statusErr
		}
		return &clientv3.StatusResponse{
			Header:           &pb.ResponseHeader{MemberId: m.id},
			Version:          m.version,
			DbSize:           m.dbSize,
			DbSizeInUse:      m.dbSize,
			Leader:           s.leaderID,
			RaftIndex:        m.raftIndex,
			RaftAppliedIndex: m.raftIndex,
			IsLearner:        m.learner,
			Errors:           m.errors,
		}, nil
	}
	return nil, fmt.Errorf("sim: 未知 endpoint %s", endpoint)
}

// Defragment 实现 rpc。
func (s *simRPC) Defragment(_ context.Context, endpoint string) (*clientv3.DefragmentResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("Defragment")
	s.defragCalls = append(s.defragCalls, endpoint)
	if s.defragErr != nil {
		return nil, s.defragErr
	}
	return &clientv3.DefragmentResponse{}, nil
}

// AlarmList 实现 rpc。
func (s *simRPC) AlarmList(context.Context) (*clientv3.AlarmResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("AlarmList")
	return &clientv3.AlarmResponse{Alarms: append([]*pb.AlarmMember(nil), s.alarms...)}, nil
}

// AlarmDisarm 实现 rpc。
func (s *simRPC) AlarmDisarm(_ context.Context, _ *clientv3.AlarmMember) (*clientv3.AlarmResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("AlarmDisarm")
	resp := &clientv3.AlarmResponse{Alarms: append([]*pb.AlarmMember(nil), s.alarms...)}
	s.alarms = nil
	return resp, nil
}

// Get 实现 rpc。
func (s *simRPC) Get(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("Get")
	if s.getErr != nil {
		return nil, s.getErr
	}
	return &clientv3.GetResponse{}, nil
}

// Snapshot 实现 rpc。
func (s *simRPC) Snapshot(context.Context) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("Snapshot")
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	if s.snapshotReadErr != nil {
		return &errAfterReader{data: s.snapshotData, err: s.snapshotReadErr}, nil
	}
	return io.NopCloser(bytes.NewReader(s.snapshotData)), nil
}

// Close 实现 rpc。
func (s *simRPC) Close() error { return nil }

func (s *simRPC) nextIDLocked() uint64 {
	var max uint64
	for _, m := range s.members {
		if m.id > max {
			max = m.id
		}
	}
	return max + 1
}

func (s *simRPC) setLearnerLocked(id uint64, learner bool) {
	for i := range s.members {
		if s.members[i].id == id {
			s.members[i].learner = learner
			if !learner && s.members[i].name == "" {
				s.members[i].name = fmt.Sprintf("m%d", id)
			}
		}
	}
}

func (s *simRPC) dropLocked(id uint64) {
	out := s.members[:0]
	for _, m := range s.members {
		if m.id != id {
			out = append(out, m)
		}
	}
	s.members = out
}

func (s *simRPC) membersPB() []*pb.Member {
	out := make([]*pb.Member, 0, len(s.members))
	for _, m := range s.members {
		out = append(out, simMemberPB(m))
	}
	return out
}

func simMemberPB(m simMember) *pb.Member {
	return &pb.Member{
		ID:         m.id,
		Name:       m.name,
		PeerURLs:   []string{m.peerURL},
		ClientURLs: []string{m.clientURL},
		IsLearner:  m.learner,
	}
}

// hostOf 从 URL 里取出 host:port（模拟用，不追求完整解析）。
func hostOf(rawURL string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	if before, _, ok := strings.Cut(u, ":"); ok {
		return before
	}
	return u
}

// fixedNow 是测试用的固定时间。
func fixedNow() time.Time { return time.Date(2024, 11, 1, 2, 3, 4, 0, time.UTC) }
