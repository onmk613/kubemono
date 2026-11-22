package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ValidationResult 承载错误与非致命警告。
type ValidationResult struct {
	Errors   field.ErrorList
	Warnings []string
}

// OK 报告校验是否通过。
func (r ValidationResult) OK() bool { return len(r.Errors) == 0 }

// Err 返回聚合错误，校验通过时返回 nil。
func (r ValidationResult) Err() error {
	if r.OK() {
		return nil
	}
	return r.Errors.ToAggregate()
}

func (r *ValidationResult) warnf(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Validate 按文档规则校验集群配置。调用方应先执行 SetDefaults。
func Validate(c *Cluster) ValidationResult {
	var res ValidationResult
	if c == nil {
		res.Errors = append(res.Errors, field.Required(field.NewPath(""), "cluster configuration is empty"))
		return res
	}
	clusterPath := field.NewPath("cluster")
	controlPlanePath := field.NewPath("controlPlane")
	nodePath := field.NewPath("node")

	validateKubernetesVersion(&res, c, field.NewPath("kubernetesVersion"))
	validateHosts(&res, c, clusterPath)
	validatePKI(&res, c, controlPlanePath)
	validateRuntime(&res, c, nodePath)
	validateComponentVersions(&res, c, field.NewPath("images"))
	validateImageOverrides(&res, c, field.NewPath("images"))
	validateEtcd(&res, c, controlPlanePath)
	validateNetwork(&res, c, field.NewPath("network"))
	validateKubelet(&res, c, nodePath)
	validateAddons(&res, c, field.NewPath("addons"))
	validateHostInit(&res, c, nodePath)
	validateControlPlane(&res, c, controlPlanePath)
	validateAudit(&res, c)
	validateExtraArgs(&res, c)
	return res
}

func validateEnum(res *ValidationResult, path *field.Path, value string, allowed []string) {
	if slices.Contains(allowed, value) {
		return
	}
	res.Errors = append(res.Errors, field.NotSupported(path, value, allowed))
}

// validateDataDir 要求数据目录是绝对路径且不是根目录。
func validateDataDir(res *ValidationResult, p *field.Path, value string) {
	if value == "" {
		return
	}
	if strings.ContainsAny(value, " \t\n\r") {
		res.Errors = append(res.Errors, field.Invalid(p, value, "must not contain whitespace"))
		return
	}
	if !filepath.IsAbs(value) {
		res.Errors = append(res.Errors, field.Invalid(p, value, "must be an absolute path"))
		return
	}
	if filepath.Clean(value) == "/" {
		res.Errors = append(res.Errors, field.Invalid(p, value, "must not be the root directory"))
	}
}
