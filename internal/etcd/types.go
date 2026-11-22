package etcd

// MemberRole 是成员在 raft 中的角色。
type MemberRole string

// 成员角色。
const (
	// RoleVoting 是参与投票、计入法定人数的成员。
	RoleVoting MemberRole = "voting"
	// RoleLearner 是不参与投票、只异步追数据的成员。
	RoleLearner MemberRole = "learner"
)

// Member 描述一个 etcd 成员。
// 字段直接对应 CLI 表格与 --output json：ID 是 etcd 的成员 ID（十六进制展示），
// IsLearner 让“learner 尚未 promote”在输出里一眼可见。
type Member struct {
	ID         uint64   `json:"id"`
	Name       string   `json:"name"`
	PeerURLs   []string `json:"peerURLs,omitempty"`
	ClientURLs []string `json:"clientURLs,omitempty"`
	IsLearner  bool     `json:"isLearner"`
}

// Role 返回成员的 raft 角色。
func (m Member) Role() MemberRole {
	if m.IsLearner {
		return RoleLearner
	}
	return RoleVoting
}

// PeerURL 返回第一个 peer URL；成员没有 peer URL 时返回 ""。
func (m Member) PeerURL() string {
	if len(m.PeerURLs) == 0 {
		return ""
	}
	return m.PeerURLs[0]
}

// HasPeerURL 判断成员的某个 peer URL 是否与 url 匹配。
func (m Member) HasPeerURL(url string) bool {
	for _, u := range m.PeerURLs {
		if SamePeerURL(u, url) {
			return true
		}
	}
	return false
}

// Status 是一个 endpoint 的成员与 raft 状态快照（clientv3 的 Status RPC）。
type Status struct {
	// Endpoint 是查询所用的 client URL。
	Endpoint string `json:"endpoint"`
	// MemberID 是该 endpoint 对应成员的 ID。
	MemberID uint64 `json:"memberID"`
	// Version 是 etcd 版本。
	Version string `json:"version,omitempty"`
	// DBSize 是后端数据库大小（字节）。
	DBSize int64 `json:"dbSize"`
	// Leader 是当前 leader 的成员 ID。
	Leader uint64 `json:"leader"`
	// RaftIndex / RaftTerm / RaftAppliedIndex 用于判断 learner 是否追平。
	RaftIndex        uint64 `json:"raftIndex"`
	RaftTerm         uint64 `json:"raftTerm"`
	RaftAppliedIndex uint64 `json:"raftAppliedIndex"`
	// IsLearner 表示该成员当前是 learner。
	IsLearner bool `json:"isLearner"`
	// Errors 是成员自报的异常（例如失去 leader）。
	Errors []string `json:"errors,omitempty"`
	// Healthy 表示 Status RPC 成功且没有自报错误。
	Healthy bool `json:"healthy"`
}

// MemberHealth 是一个 endpoint 的健康结论，可直接喂给 CLI 表格与 JSON 输出。
type MemberHealth struct {
	// Endpoint 是查询所用的 client URL。
	Endpoint string `json:"endpoint"`
	// MemberID 是该 endpoint 对应成员的 ID（0 表示无法确定）。
	MemberID uint64 `json:"memberID"`
	// Member 是成员名；learner 刚加入、还没上报名字时为空。
	Member string `json:"member,omitempty"`
	// Role 是成员角色（voting|learner）。
	Role MemberRole `json:"role"`
	// Healthy 表示该 endpoint 可服务。
	Healthy bool `json:"healthy"`
	// Error 是探测失败的原因（成功时为空）。
	Error string `json:"error,omitempty"`
	// LatencyMS 是探测耗时（毫秒）。
	LatencyMS int64 `json:"latencyMS,omitempty"`
	// Status 是成功时的详细状态。
	Status *Status `json:"status,omitempty"`
}

// Local 描述本机 etcd 成员。
// Member 为 false 表示本机不运行 etcd：stacked 的 worker、external+unmanaged
// 的所有节点都是这样，etcd phase 因此不适用。
type Local struct {
	// Name 是成员名（等于节点名）。
	Name string `json:"name"`
	// Member 表示本机是否运行 etcd 成员。
	Member bool `json:"member"`
	// PeerURLs 是本机对外的 peer URL。
	PeerURLs []string `json:"peerURLs,omitempty"`
	// ClientURL 是本机对外的 client URL（健康探测使用）。
	ClientURL string `json:"clientURL,omitempty"`
	// DataDir 是本机 etcd 数据目录。
	DataDir string `json:"dataDir,omitempty"`
	// ServiceName 是 systemd unit 名。
	ServiceName string `json:"serviceName,omitempty"`
}
