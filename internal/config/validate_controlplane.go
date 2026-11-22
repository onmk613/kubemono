package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func validatePKI(res *ValidationResult, c *Cluster, spec *field.Path) {
	p := spec.Child("pki")
	validateEnum(res, p.Child("keyAlgorithm"), c.ControlPlane.PKI.KeyAlgorithm, []string{KeyAlgorithmRSA2048, KeyAlgorithmECDSAP256})
	if c.ControlPlane.PKI.CAValidity.D() <= 0 {
		res.Errors = append(res.Errors, field.Invalid(p.Child("caValidity"), c.ControlPlane.PKI.CAValidity.String(), "must be positive"))
	}
	if c.ControlPlane.PKI.CertValidity.D() <= 0 {
		res.Errors = append(res.Errors, field.Invalid(p.Child("certValidity"), c.ControlPlane.PKI.CertValidity.String(), "must be positive"))
	}
	if c.ControlPlane.PKI.CertValidity.D() > c.ControlPlane.PKI.CAValidity.D() {
		res.Errors = append(res.Errors, field.Invalid(p.Child("certValidity"), c.ControlPlane.PKI.CertValidity.String(), "must not exceed caValidity"))
	}
	for _, ext := range []struct {
		path string
		spec ExternalCASpec
	}{
		{"externalCA", c.ControlPlane.PKI.ExternalCA},
		{"externalFrontProxyCA", c.ControlPlane.PKI.ExternalFrontProxyCA},
		{"externalEtcdCA", c.ControlPlane.PKI.ExternalEtcdCA},
	} {
		if (ext.spec.CertFile == "") != (ext.spec.KeyFile == "") {
			res.Errors = append(res.Errors, field.Invalid(p.Child(ext.path), ext.spec, "certFile and keyFile must be provided together"))
		}
	}
}

func validateEtcd(res *ValidationResult, c *Cluster, spec *field.Path) {
	p := spec.Child("etcd")
	if !c.ControlPlane.Etcd.IsExternal() {
		validateDataDir(res, p.Child("local", "dataDir"), c.ControlPlane.Etcd.Local.DataDir)
		return
	}
	ext := c.ControlPlane.Etcd.External
	ep := p.Child("external")
	if c.ControlPlane.Etcd.Local.DataDir != "" || len(c.ControlPlane.Etcd.Local.ExtraArgs) > 0 {
		res.warnf("controlPlane.etcd.local is ignored when external etcd is configured")
	}

	if len(ext.Endpoints) == 0 {
		res.Errors = append(res.Errors, field.Required(ep.Child("endpoints"), "required for external etcd"))
	}
	for i, e := range ext.Endpoints {
		if err := validateEtcdEndpoint(e); err != nil {
			res.Errors = append(res.Errors, field.Invalid(ep.Child("endpoints").Index(i), e, err.Error()))
		}
	}
	for name, f := range map[string]string{"caFile": ext.CAFile, "certFile": ext.CertFile, "keyFile": ext.KeyFile} {
		if f == "" {
			res.Errors = append(res.Errors, field.Required(ep.Child(name), "required for external etcd"))
		}
	}
}

func validateControlPlane(res *ValidationResult, c *Cluster, spec *field.Path) {
	p := spec
	for i, ep := range c.ControlPlane.ExternalEndpoints {
		ap := p.Child("externalEndpoints").Index(i)
		if _, err := ParseAPIEndpoint(ep, 6443); err != nil {
			res.Errors = append(res.Errors, field.Invalid(ap, ep, err.Error()))
			continue
		}
		if !strings.Contains(ep, ":") {
			res.warnf("externalEndpoints[%d]=%q has no port; %d is assumed", i, ep, 6443)
		}
		host := EndpointHost(ep)
		if len(c.Cluster.Hosts.Masters()) > 1 {
			for _, m := range c.Cluster.Hosts.Masters() {
				if m.Address != "" && strings.EqualFold(host, m.Address) {
					res.warnf("externalEndpoints[%d]=%q is a master address, not a VIP/DNS; kubelet uses only the first endpoint and will not fail over", i, ep)
					break
				}
			}
		}
	}
	for i, san := range c.ControlPlane.CertSANs {
		if err := validateCertSAN(san); err != nil {
			res.Errors = append(res.Errors, field.Invalid(p.Child("certSANs").Index(i), san, err.Error()))
		}
	}
}

var dnsSANPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// validateCertSAN 校验一个证书 SAN：只接受 IPv4 或 DNS 名。
// 拒绝通配符、host:port、路径和空白——这些是手写 SAN 时最常见的错误，
// 而一张含错误 SAN 的 apiserver 证书会在 TLS 握手时才暴露。
func validateCertSAN(raw string) error {
	san := strings.TrimSpace(raw)
	if san == "" {
		return fmt.Errorf("SAN must not be empty")
	}
	if san != raw {
		return fmt.Errorf("SAN must not contain surrounding whitespace")
	}
	if strings.ContainsAny(san, " \t\n\r/:*") {
		return fmt.Errorf("SAN must be an IPv4 address or DNS name (no port, path, wildcard or whitespace)")
	}
	if ip, err := netip.ParseAddr(san); err == nil {
		if ip.Is6() {
			return fmt.Errorf("IPv6 SANs are not supported yet")
		}
		return nil
	}
	if !dnsSANPattern.MatchString(san) {
		return fmt.Errorf("SAN is not a valid DNS name or IPv4 address")
	}
	return nil
}

// validateEtcdEndpoint 校验 external etcd 的 endpoint URL。
func validateEtcdEndpoint(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must be an http(s) URL")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("must contain a host")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("must not contain a path")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("invalid port %q", port)
		}
	}
	return nil
}

// validateExtraArgs 检查 extraArgs 的 flag 名格式与保留项。
// extraArgs 是"上游默认值之外的唯一覆盖入口"；但决定集群身份/信任根/数据目录/
// 端口的 flag 由 kubemono 管理（见 reserved.go），覆盖它们不会得到合法集群。
// 名字规则与 profile 渲染共用 ValidateExtraArgName，避免“校验放过、渲染失败”。
// validateAudit 校验审计开关、日志参数和内联策略。
func validateAudit(res *ValidationResult, c *Cluster) {
	p := field.NewPath("controlPlane", "audit")
	a := c.ControlPlane.Audit
	if a.LogMaxAge != nil && *a.LogMaxAge < 0 {
		res.Errors = append(res.Errors, field.Invalid(p.Child("logMaxAge"), *a.LogMaxAge, "must be >= 0"))
	}
	if a.LogMaxBackup != nil && *a.LogMaxBackup < 0 {
		res.Errors = append(res.Errors, field.Invalid(p.Child("logMaxBackup"), *a.LogMaxBackup, "must be >= 0"))
	}
	if a.LogMaxSize != nil && *a.LogMaxSize <= 0 {
		res.Errors = append(res.Errors, field.Invalid(p.Child("logMaxSize"), *a.LogMaxSize, "must be > 0"))
	}
	if !a.IsEnabled() {
		if strings.TrimSpace(a.Policy) != "" {
			res.Errors = append(res.Errors, field.Forbidden(p.Child("policy"), "audit policy requires controlPlane.audit.enabled=true"))
		}
		return
	}
	if strings.TrimSpace(a.Policy) == "" {
		return
	}
	var doc struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Rules      []struct {
			Level string `yaml:"level"`
		} `yaml:"rules"`
	}
	if err := yaml.Unmarshal([]byte(a.Policy), &doc); err != nil {
		res.Errors = append(res.Errors, field.Invalid(p.Child("policy"), "invalid YAML", err.Error()))
		return
	}
	if doc.APIVersion != "audit.k8s.io/v1" {
		res.Errors = append(res.Errors, field.Invalid(p.Child("policy", "apiVersion"), doc.APIVersion,
			"must be audit.k8s.io/v1"))
	}
	if doc.Kind != "Policy" {
		res.Errors = append(res.Errors, field.Invalid(p.Child("policy", "kind"), doc.Kind, "must be Policy"))
	}
	if len(doc.Rules) == 0 {
		res.Errors = append(res.Errors, field.Required(p.Child("policy", "rules"), "at least one audit rule is required"))
	}
	for i, rule := range doc.Rules {
		switch rule.Level {
		case "None", "Metadata", "Request", "RequestResponse":
		default:
			res.Errors = append(res.Errors, field.NotSupported(p.Child("policy", "rules").Index(i).Child("level"), rule.Level,
				[]string{"None", "Metadata", "Request", "RequestResponse"}))
		}
	}
}

func validateExtraArgs(res *ValidationResult, c *Cluster) {
	// 组件清单与 render 的取值共用 ExtraArgsSites，新增组件不需要改两处。
	for _, site := range c.ExtraArgsSites() {
		for k, v := range site.Args(c) {
			if err := ValidateExtraArgName(k); err != nil {
				res.Errors = append(res.Errors, field.Invalid(site.Path, k, err.Error()))
				continue
			}
			if reason := ReservedExtraArgReason(site.Component, k); reason != "" {
				res.Errors = append(res.Errors, field.Forbidden(site.Path.Key(k),
					"该 flag 由 kubemono 管理，不能通过 extraArgs 覆盖："+reason))
			}
			if strings.ContainsAny(v, " \t\n\r") {
				res.Errors = append(res.Errors, field.Invalid(site.Path.Key(k), v, "flag value must not contain whitespace"))
			}
		}
	}
	// kube-router 不承担 Service 代理：禁止通过 extraArgs 把它打开，
	// 否则会出现 kube-router 与 kube-proxy 同时写 Service 规则。
	if _, ok := c.Network.KubeRouter.ExtraArgs["run-service-proxy"]; ok {
		res.Errors = append(res.Errors, field.Forbidden(
			field.NewPath("network", "kubeRouter", "extraArgs").Key("run-service-proxy"),
			"kube-router only provides CNI now; Service proxying is handled by kube-proxy"))
	}
	for _, flag := range []string{"audit-log-maxage", "audit-log-maxbackup", "audit-log-maxsize", "audit-policy-file", "audit-log-path"} {
		if _, ok := c.ControlPlane.APIServer.ExtraArgs[flag]; ok {
			res.Errors = append(res.Errors, field.Forbidden(
				field.NewPath("controlPlane", "apiServer", "extraArgs").Key(flag),
				"audit flags are configured under controlPlane.audit"))
		}
	}
}
