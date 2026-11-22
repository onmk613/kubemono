package config

import (
	"slices"
	"sort"
	"strings"
)

// Role 是节点角色。
type Role string

// 节点角色。
const (
	// RoleMaster 运行控制面。
	RoleMaster Role = "master"
	// RoleWorker 运行工作负载。
	RoleWorker Role = "worker"
)

// AllRoles 是规范的角色顺序。
var AllRoles = []Role{RoleMaster, RoleWorker}

// Valid 报告角色是否已知。
func (r Role) Valid() bool {
	switch r {
	case RoleMaster, RoleWorker:
		return true
	default:
		return false
	}
}

// Hosts 是集群成员列表。
type Hosts []Host

// Host 是一个集群成员。
type Host struct {
	// Name 是 cluster.yaml 中的身份键，用于配置引用、bootstrap.node、--node
	// 和 join 校验；它不是 kubelet/etcd 的实际节点名。
	Name string `json:"name" yaml:"name"`
	// NodeName 控制实际节点名（kubelet NodeName、etcd 成员名、证书 CN）：
	// - 空或 "hostname"：使用本机 hostname（默认）；
	// - "address"：使用 Address；
	// - 其他值：原样使用。
	// 多 master 场景下，若希望 master 的 etcd/证书身份在配置里可推导，应显式
	// 写 nodeName（address 或固定名字），不要依赖其他机器的主机名。
	NodeName       string            `json:"nodeName,omitempty" yaml:"nodeName,omitempty"`
	Address        string            `json:"address" yaml:"address"`
	MatchAddresses []string          `json:"matchAddresses,omitempty" yaml:"matchAddresses,omitempty"`
	Roles          []Role            `json:"roles" yaml:"roles"`
	Labels         map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Taints         []string          `json:"taints,omitempty" yaml:"taints,omitempty"`
}

// 节点名来源的特殊取值。
const (
	// NodeNameFromHostname 表示使用本机 hostname（默认）。
	NodeNameFromHostname = "hostname"
	// NodeNameFromAddress 表示使用 Host.Address 作为节点名。
	NodeNameFromAddress = "address"
)

// NodeNameSource 返回规范化后的 nodeName 策略。
func (h Host) NodeNameSource() string {
	switch strings.TrimSpace(h.NodeName) {
	case "", NodeNameFromHostname:
		return NodeNameFromHostname
	case NodeNameFromAddress:
		return NodeNameFromAddress
	default:
		return strings.TrimSpace(h.NodeName)
	}
}

// EffectiveNodeName 返回本机实际使用的节点名。
// hostname 是本机 hostname；为空时回退到 Host.Name，保证离线渲染/单元测试
// 没有 hostname 注入时仍能工作。
func (h Host) EffectiveNodeName(hostname string) string {
	switch h.NodeNameSource() {
	case NodeNameFromHostname:
		if strings.TrimSpace(hostname) != "" {
			return strings.TrimSpace(hostname)
		}
		return h.Name
	case NodeNameFromAddress:
		return h.Address
	default:
		return h.NodeNameSource()
	}
}

// NodeNameOrName 返回不依赖本机 hostname 的节点名：显式 nodeName/address 原样
// 使用，hostname 策略回退到配置身份 Name。
func (h Host) NodeNameOrName() string {
	switch h.NodeNameSource() {
	case NodeNameFromHostname:
		return h.Name
	case NodeNameFromAddress:
		return h.Address
	default:
		return h.NodeNameSource()
	}
}

// NodeNameOf 计算本机实际的 Kubernetes NodeName：显式覆盖优先，其次 node.yaml，
// 最后是 cluster.yaml 中的条目。
func NodeNameOf(override string, node *NodeConfig, host *Host) string {
	if override != "" {
		return override
	}
	if node != nil && node.NodeName != "" {
		return node.NodeName
	}
	if host != nil {
		return host.NodeNameOrName()
	}
	return ""
}

// HasRole 报告主机是否具有给定角色。
func (h Host) HasRole(r Role) bool {
	return slices.Contains(h.Roles, r)
}

// IsMaster 报告主机是否运行控制面。
func (h Host) IsMaster() bool { return h.HasRole(RoleMaster) }

// IsWorker 报告主机是否运行工作负载。
func (h Host) IsWorker() bool { return h.HasRole(RoleWorker) }

// IsMixedMaster 报告主机是否同时承担控制面与工作负载（roles: [master, worker]）。
func (h Host) IsMixedMaster() bool { return h.IsMaster() && h.IsWorker() }

// AllAddresses 返回声明地址加上所有额外的 match 地址。
func (h Host) AllAddresses() []string {
	out := make([]string, 0, 1+len(h.MatchAddresses))
	if h.Address != "" {
		out = append(out, h.Address)
	}
	out = append(out, h.MatchAddresses...)
	return out
}

// RolesString 按规范顺序渲染角色，例如 "master,worker"。
func (h Host) RolesString() string {
	roles := make([]string, 0, len(h.Roles))
	for _, r := range AllRoles {
		if h.HasRole(r) {
			roles = append(roles, string(r))
		}
	}
	var out strings.Builder
	for i, r := range roles {
		if i > 0 {
			out.WriteString(",")
		}
		out.WriteString(r)
	}
	return out.String()
}

// FindHostByNodeName 按实际 NodeName（显式 nodeName 或 address 策略）查找主机。
// hostname 策略在配置侧无法展开，因此不会匹配。
func (s Hosts) FindHostByNodeName(nodeName string) (*Host, bool) {
	for i := range s {
		if s[i].NodeNameSource() != NodeNameFromHostname && s[i].NodeNameOrName() == nodeName {
			return &s[i], true
		}
	}
	return nil, false
}

// FindHost 返回给定名字的主机。
func (s Hosts) FindHost(name string) (*Host, bool) {
	for i := range s {
		if s[i].Name == name {
			return &s[i], true
		}
	}
	return nil, false
}

// HostNames 按声明顺序返回全部主机名。
func (s Hosts) HostNames() []string {
	out := make([]string, 0, len(s))
	for _, h := range s {
		out = append(out, h.Name)
	}
	return out
}

// Masters 按声明顺序返回 master 主机。
func (s Hosts) Masters() []Host {
	return s.byRole(RoleMaster)
}

// Workers 按声明顺序返回 worker 主机。
func (s Hosts) Workers() []Host {
	return s.byRole(RoleWorker)
}

func (s Hosts) byRole(r Role) []Host {
	out := make([]Host, 0, len(s))
	for _, h := range s {
		if h.HasRole(r) {
			out = append(out, h)
		}
	}
	return out
}

// SortedNames 返回按字典序排序的全部主机名。
func (s Hosts) SortedNames() []string {
	out := s.HostNames()
	sort.Strings(out)
	return out
}
