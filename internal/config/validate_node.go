package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// registryHostPattern 限定 registries.mirrors/configs 的键：域名或 IPv4，
// 可选端口。这些键会直接拼进 containerd certs.d 的目录名，含 "/"、".." 或
// 空白的键会写出目录树之外。
var registryHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

func validateRuntime(res *ValidationResult, c *Cluster, spec *field.Path) {
	p := spec.Child("runtime")
	if patch := strings.TrimSpace(c.Node.Runtime.Containerd.ConfigPatch); patch != "" {
		var parsed map[string]any
		if err := toml.Unmarshal([]byte(patch), &parsed); err != nil {
			res.Errors = append(res.Errors, field.Invalid(p.Child("containerd", "configPatch"), patch,
				"invalid TOML: "+err.Error()))
		}
	}
	if c.Node.Runtime.IsCustom() {
		cp := p.Child("custom")
		if c.Node.Runtime.Custom.CRISocket == "" {
			res.Errors = append(res.Errors, field.Required(cp.Child("criSocket"), "required when node.runtime.custom is configured"))
		}
		if c.Node.Runtime.Custom.ServiceName == "" {
			res.Errors = append(res.Errors, field.Required(cp.Child("serviceName"), "required when node.runtime.custom is configured"))
		}
		if d := c.Node.Runtime.Custom.CgroupDriver; d != "" && d != "systemd" && d != "cgroupfs" {
			res.Errors = append(res.Errors, field.NotSupported(cp.Child("cgroupDriver"), d, []string{"systemd", "cgroupfs"}))
		}
	}
	if !c.Node.Runtime.IsCustom() {
		validateDataDir(res, p.Child("containerd", "root"), c.Node.Runtime.Containerd.Root)
		for host, mirrors := range c.Node.Runtime.Containerd.Registries.Mirrors {
			if err := validateRegistryHost(host); err != nil {
				res.Errors = append(res.Errors, field.Invalid(p.Child("containerd", "registries", "mirrors").Key(host), host, err.Error()))
			}
			for i, m := range mirrors {
				if m == "" {
					res.Errors = append(res.Errors, field.Invalid(p.Child("containerd", "registries", "mirrors").Key(host).Index(i), m, "mirror must not be empty"))
				}
			}
		}
		for host, auth := range c.Node.Runtime.Containerd.Registries.Configs {
			key := p.Child("containerd", "registries", "configs").Key(host)
			if err := validateRegistryHost(host); err != nil {
				res.Errors = append(res.Errors, field.Invalid(key, host, err.Error()))
			}
			// 认证按 Basic 头渲染，只写一半会在运行期 401：
			// 要么都给，要么都不给（只用 CA / skipVerify 的仓库不受影响）。
			if (auth.Username == "") != (auth.Password == "") {
				res.Errors = append(res.Errors, field.Invalid(key, host,
					"username 与 password 必须成对配置（只写其中一个会让 containerd 认证失败）"))
			}
		}
	}
}

func validateRegistryHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("registry host must not be empty")
	}
	if strings.TrimSpace(host) != host {
		return fmt.Errorf("registry host must not contain surrounding whitespace")
	}
	if strings.Contains(host, "..") {
		return fmt.Errorf("registry host must not contain \"..\"")
	}
	if !registryHostPattern.MatchString(host) {
		return fmt.Errorf("registry host must be a domain or IPv4 with an optional port, e.g. registry.example.com:5000")
	}
	return nil
}

// reservedKubeletConfigPatch 是 kubelet.configPatch 不允许覆盖的顶层键：
// 它们决定 apiserver 如何认证到 kubelet、kubelet 读哪份配置/凭据/静态 Pod。
// 这些值不是“调优”，覆盖会直接拆掉安全基线或让 kubemono 的接线失效。
var reservedKubeletConfigPatch = map[string]string{
	"readOnlyPort":             "固定为 0（关闭只读端口）",
	"authentication":           "固定为 x509 + webhook（安全基线）",
	"authorization":            "固定为 Webhook（安全基线）",
	"staticPodPath":            "固定为 /etc/kubernetes/manifests",
	"containerRuntimeEndpoint": "由 node.runtime 决定",
	"cgroupDriver":             "由 node.runtime 决定",
	"clusterDomain":            "由 network.dnsDomain 决定",
}

func validateKubelet(res *ValidationResult, c *Cluster, spec *field.Path) {
	validateDataDir(res, spec.Child("kubelet", "rootDir"), c.Node.Kubelet.RootDir)
	for key := range c.Node.Kubelet.ConfigPatch {
		if reason := reservedKubeletConfigPatch[key]; reason != "" {
			res.Errors = append(res.Errors, field.Forbidden(
				spec.Child("kubelet", "configPatch").Key(key),
				"该字段由 kubemono 管理，不能通过 configPatch 覆盖："+reason))
		}
	}
}

func validateHostInit(res *ValidationResult, c *Cluster, spec *field.Path) {
	p := spec.Child("host")
	for i, m := range c.Node.Host.KernelModules {
		key := p.Child("kernelModules").Index(i)
		name := strings.TrimSpace(m)
		if name == "" {
			res.Errors = append(res.Errors, field.Invalid(key, m, "module name must not be empty"))
			continue
		}
		if name != m {
			res.Errors = append(res.Errors, field.Invalid(key, m, "module name must not contain surrounding whitespace"))
		}
		if strings.ContainsAny(name, " \t\n/") {
			res.Errors = append(res.Errors, field.Invalid(key, m, "module name must not contain whitespace or path separators"))
		}
	}
	for k, v := range c.Node.Host.Sysctls {
		if err := ValidateSysctlKey(k); err != nil {
			res.Errors = append(res.Errors, field.Invalid(p.Child("sysctls"), k, err.Error()))
		}
		// 值会同时写进 /proc/sys 与 sysctl.d；换行会注入额外的配置行。
		if strings.ContainsAny(v, "\n\r\x00") {
			res.Errors = append(res.Errors, field.Invalid(p.Child("sysctls").Key(k), v, "must not contain newlines or NUL"))
		}
	}
}

// ValidateSysctlKey 校验 sysctls 的键：只允许点或斜杠分隔的字母数字、
// 下划线与连字符，禁止空段、".." 与绝对路径。applier 会把点替换成斜杠后
// 拼到 /proc/sys 下，键里一旦出现 ".." 就能写出该目录。
func ValidateSysctlKey(key string) error {
	if key == "" {
		return fmt.Errorf("sysctl key must not be empty")
	}
	if strings.TrimSpace(key) != key {
		return fmt.Errorf("sysctl key must not contain surrounding whitespace")
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") ||
		strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") || strings.Contains(key, "//") {
		return fmt.Errorf("sysctl key must not be absolute or contain empty segments")
	}
	if strings.Contains(key, "..") {
		return fmt.Errorf("sysctl key must not contain \"..\"")
	}
	if !strings.ContainsAny(key, "./") {
		return fmt.Errorf("sysctl key must be namespaced, e.g. net.ipv4.ip_forward")
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '/', r == '_', r == '-':
		default:
			return fmt.Errorf("sysctl key %q contains invalid character %q", key, r)
		}
	}
	for _, seg := range strings.FieldsFunc(key, func(r rune) bool { return r == '.' || r == '/' }) {
		if seg == "" {
			return fmt.Errorf("sysctl key must not contain empty segments")
		}
	}
	return nil
}
