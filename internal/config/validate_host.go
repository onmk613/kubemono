package config

import (
	"fmt"
	"net/netip"
	"strings"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func validateHosts(res *ValidationResult, c *Cluster, spec *field.Path) {
	hostsPath := spec.Child("hosts")
	list := c.Cluster.Hosts
	if len(list) == 0 {
		res.Errors = append(res.Errors, field.Required(hostsPath, "at least one host is required"))
		return
	}

	names := map[string]int{}
	nodeNames := map[string]int{}
	addresses := map[string]string{}
	masters := 0
	for i, h := range list {
		hp := hostsPath.Index(i)
		validateHostName(res, h, hp, names, i)
		validateHostNodeName(res, h, hp, nodeNames, i)
		validateHostRoles(res, h, hp)
		validateHostAddresses(res, h, hp, addresses)
		validateHostLabelsAndTaints(res, h, hp)
		if h.IsMaster() {
			masters++
		}
	}

	if masters == 0 {
		res.Errors = append(res.Errors, field.Required(hostsPath, "at least one host must have the master role"))
	}
	if masters%2 == 0 && !c.ControlPlane.Etcd.IsExternal() && masters > 0 {
		res.warnf("本地 etcd with %d masters: an even number of etcd members reduces fault tolerance", masters)
	}
}

func validateHostName(res *ValidationResult, h Host, hp *field.Path, names map[string]int, i int) {
	if h.Name == "" {
		res.Errors = append(res.Errors, field.Required(hp.Child("name"), "host name is required"))
		return
	}
	if errs := utilvalidation.IsDNS1123Subdomain(h.Name); len(errs) > 0 {
		res.Errors = append(res.Errors, field.Invalid(hp.Child("name"), h.Name, strings.Join(errs, "; ")))
		return
	}
	if prev, dup := names[h.Name]; dup {
		res.Errors = append(res.Errors, field.Duplicate(hp.Child("name"), fmt.Sprintf("%s (also hosts[%d])", h.Name, prev)))
		return
	}
	names[h.Name] = i
}

func validateHostNodeName(res *ValidationResult, h Host, hp *field.Path, nodeNames map[string]int, i int) {
	// nodeName 是 kubelet/etcd 的实际节点名和证书 CN；hostname 策略依赖远端主机，
	// 配置校验时只校验显式值并检查唯一性。
	source := h.NodeNameSource()
	if source == NodeNameFromHostname {
		return
	}
	if source == NodeNameFromAddress {
		if strings.TrimSpace(h.Address) == "" {
			res.Errors = append(res.Errors, field.Required(hp.Child("address"), "address is required when nodeName is address"))
		} else if addr, err := netip.ParseAddr(strings.TrimSpace(h.Address)); err == nil && addr.Is6() {
			res.Errors = append(res.Errors, field.Invalid(hp.Child("nodeName"), h.Address, "IPv6 address cannot be used as a Kubernetes NodeName"))
		}
	} else if addr, err := netip.ParseAddr(source); err == nil {
		if addr.Is6() {
			res.Errors = append(res.Errors, field.Invalid(hp.Child("nodeName"), source, "IPv6 address cannot be used as a Kubernetes NodeName"))
		}
	} else if errs := utilvalidation.IsDNS1123Subdomain(source); len(errs) > 0 {
		res.Errors = append(res.Errors, field.Invalid(hp.Child("nodeName"), source, strings.Join(errs, "; ")))
	}
	if effective := h.NodeNameOrName(); effective != "" {
		if prev, dup := nodeNames[effective]; dup {
			res.Errors = append(res.Errors, field.Duplicate(hp.Child("nodeName"), fmt.Sprintf("%s (also hosts[%d])", effective, prev)))
		} else {
			nodeNames[effective] = i
		}
	}
}

func validateHostRoles(res *ValidationResult, h Host, hp *field.Path) {
	if len(h.Roles) == 0 {
		res.Errors = append(res.Errors, field.Required(hp.Child("roles"), "at least one role is required"))
		return
	}
	seen := map[Role]bool{}
	for j, r := range h.Roles {
		if !r.Valid() {
			res.Errors = append(res.Errors, field.NotSupported(hp.Child("roles").Index(j), r, roleStrings()))
		}
		if seen[r] {
			res.Errors = append(res.Errors, field.Duplicate(hp.Child("roles").Index(j), r))
		}
		seen[r] = true
	}
}

func validateHostAddresses(res *ValidationResult, h Host, hp *field.Path, addresses map[string]string) {
	if h.Address == "" {
		res.Errors = append(res.Errors, field.Required(hp.Child("address"), "host address is required"))
	}
	for j, addr := range h.AllAddresses() {
		ap := hp.Child("address")
		if j > 0 {
			ap = hp.Child("matchAddresses").Index(j - 1)
		}
		parsed, err := netip.ParseAddr(addr)
		if err != nil {
			res.Errors = append(res.Errors, field.Invalid(ap, addr, "not a valid IP address"))
			continue
		}
		if parsed.Is6() {
			res.Errors = append(res.Errors, field.Invalid(ap, addr,
				"IPv6 is not supported yet: node addresses, CNI dual-stack and etcd URLs are IPv4-only"))
			continue
		}
		key := parsed.String()
		if other, dup := addresses[key]; dup {
			res.Errors = append(res.Errors, field.Duplicate(ap, fmt.Sprintf("%s (already used by host %s)", addr, other)))
		} else {
			addresses[key] = h.Name
		}
	}
}

func validateHostLabelsAndTaints(res *ValidationResult, h Host, hp *field.Path) {
	for k := range h.Labels {
		if strings.TrimSpace(k) == "" {
			res.Errors = append(res.Errors, field.Invalid(hp.Child("labels"), k, "label key must not be empty"))
		}
	}
	validateHostTaints(res, h, hp)
}

// validateHostTaints 校验 hosts[].taints 的 "key[=value]:Effect" 形式。
// 必须同时校验 key 与 effect：非法 effect 会一路渲染进 kubelet 配置
// （RegisterWithTaints），要到 kubelet 启动失败才暴露；写入 Node 的
// mark-control-plane 路径在 kubelet 起不来之后已经跑不到。解析逻辑与
// profile/markcontrolplane 共用 ParseTaint，避免三处语义漂移。
func validateHostTaints(res *ValidationResult, h Host, hp *field.Path) {
	for i, raw := range h.Taints {
		if _, _, _, err := ParseTaint(raw); err != nil {
			res.Errors = append(res.Errors, field.Invalid(hp.Child("taints").Index(i), raw, err.Error()))
		}
	}
}

func roleStrings() []string {
	out := make([]string, 0, len(AllRoles))
	for _, r := range AllRoles {
		out = append(out, string(r))
	}
	return out
}
