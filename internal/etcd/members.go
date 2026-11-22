package etcd

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"kubemono/internal/constants"
)

// 本文件只放“纯成员工具”：输入是成员列表或地址，不触碰 clientv3。
// 从 client.go 原样移入，不导出新符号、不改变实现。

// quorum 返回 n 个投票成员所需的多数派。
func quorum(n int) int {
	if n <= 0 {
		return 0
	}
	return n/2 + 1
}

// FindMember 按名字或 peer URL 查找成员。
// learner 刚加入、还没上报名字时 Name 为空，因此 peer URL 是不可省略的匹配条件。
func FindMember(members []Member, name string, peerURLs []string) (Member, bool) {
	if name != "" {
		for _, m := range members {
			if m.Name == name {
				return m, true
			}
		}
	}
	for _, m := range members {
		if slices.ContainsFunc(peerURLs, m.HasPeerURL) {
			return m, true
		}
	}
	return Member{}, false
}

// MemberByID 按成员 ID 查找。
func MemberByID(members []Member, id uint64) (Member, bool) {
	for _, m := range members {
		if m.ID == id {
			return m, true
		}
	}
	return Member{}, false
}

// SamePeerURL 报告两个 peer URL 是否指向同一个 endpoint。
// 容忍大小写与结尾斜杠差异（配置里手写的 URL 与 etcd 返回的 URL 常常不一致）。
func SamePeerURL(a, b string) bool {
	if a == b {
		return true
	}
	ua, errA := url.Parse(strings.TrimSuffix(a, "/"))
	ub, errB := url.Parse(strings.TrimSuffix(b, "/"))
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// InitialCluster 生成 etcd 的 --initial-cluster 值（name=peerURL 列表）。
// 语义（k0s 同款，因为 etcd 在 --initial-cluster-state=existing 时会
// 比对 --initial-cluster 的条目数与集群实际成员数，见 membership.ValidateClusterAndAssignIDs）：
// - peer URL 与 localPeerURL 匹配的成员用 localName 命名（加入者自己还不知道名字）；
// - 从未上报名字的成员用 unnamed-<id> 占位，保证列表项数与成员数一致；
// - 重名时追加 ID 后缀，避免生成非法的重复 key。
func InitialCluster(members []Member, localName, localPeerURL string) ([]string, error) {
	out := make([]string, 0, len(members))
	used := map[string]bool{}
	found := localPeerURL == ""
	for _, m := range members {
		peer := m.PeerURL()
		if peer == "" {
			return nil, fmt.Errorf("成员 %x 没有 peer URL，无法生成 --initial-cluster；先执行 `etcdctl member list` 检查集群状态", m.ID)
		}
		name := m.Name
		if localPeerURL != "" && m.HasPeerURL(localPeerURL) {
			name = localName
			found = true
		}
		if name == "" {
			name = fmt.Sprintf("unnamed-%x", m.ID)
		}
		if used[name] {
			name = fmt.Sprintf("%s-%x", name, m.ID)
		}
		used[name] = true
		out = append(out, name+"="+peer)
	}
	if !found {
		return nil, fmt.Errorf("集群成员里没有 peer URL 为 %s 的成员：先执行 `etcdctl member add`，或确认本机地址与集群配置一致", localPeerURL)
	}
	sort.Strings(out)
	return out, nil
}

// PeerURLs 返回一组 host 地址对应的 peer URL（https://<addr>:2380）。
func PeerURLs(addresses []string) []string {
	out := make([]string, 0, len(addresses))
	for _, a := range addresses {
		if a == "" {
			continue
		}
		out = append(out, fmt.Sprintf("https://%s:%d", a, constants.EtcdPeerPort))
	}
	return out
}

// ClientURLs 返回一组 host 地址对应的 client URL（https://<addr>:2379）。
func ClientURLs(addresses []string) []string {
	out := make([]string, 0, len(addresses))
	for _, a := range addresses {
		if a == "" {
			continue
		}
		out = append(out, fmt.Sprintf("https://%s:%d", a, constants.EtcdClientPort))
	}
	return out
}

// sortMembers 让输出稳定（按名字、再按 ID）。
func sortMembers(members []Member) {
	sort.Slice(members, func(i, j int) bool {
		if members[i].Name != members[j].Name {
			return members[i].Name < members[j].Name
		}
		return members[i].ID < members[j].ID
	})
}

// memberLabel 返回展示用的成员名（learner 还没上报名字时用 unnamed-<id>）。
func memberLabel(m Member) string {
	if m.Name != "" {
		return m.Name
	}
	return fmt.Sprintf("unnamed-%x", m.ID)
}
