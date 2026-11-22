package config

import (
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"kubemono/internal/constants"
)

// kubernetesVersionPattern 限定 kubernetesVersion 的形状：vX.Y.Z，不接受
// 预发布后缀（kubemono 只发稳定版）。
var kubernetesVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func validateComponentVersions(res *ValidationResult, c *Cluster, spec *field.Path) {
	p := spec.Child("componentVersions")
	for name, version := range c.Images.ComponentVersions {
		key := p.Key(name)
		if strings.TrimSpace(name) == "" {
			res.Errors = append(res.Errors, field.Invalid(p, name, "component name must not be empty"))
			continue
		}
		if strings.TrimSpace(version) == "" {
			res.Errors = append(res.Errors, field.Required(key, "component version must not be empty"))
			continue
		}
		// k8s 组件只能通过顶层的 kubernetesVersion 整组固定：两个入口会造成
		// 同一条版本线有两个事实来源，因此这里直接拒绝。
		if constants.IsKubernetesComponent(name) {
			res.Errors = append(res.Errors, field.Forbidden(key,
				"Kubernetes components are controlled by spec.kubernetesVersion; remove them from componentVersions"))
		}
		if name == "kubemono" {
			res.Errors = append(res.Errors, field.Forbidden(key, "the kubemono binary version is part of the release and cannot be overridden"))
		}
	}
}

func validateImageOverrides(res *ValidationResult, c *Cluster, spec *field.Path) {
	p := spec.Child("components")
	for name, ref := range c.Images.Components {
		key := p.Key(name)
		if strings.TrimSpace(name) == "" {
			res.Errors = append(res.Errors, field.Invalid(p, name, "component name must not be empty"))
			continue
		}
		if name == "kubemono" {
			res.Errors = append(res.Errors, field.Forbidden(key, "the kubemono binary image is not part of the BOM and cannot be overridden"))
			continue
		}
		if strings.TrimSpace(ref) == "" {
			res.Errors = append(res.Errors, field.Required(key, "image must not be empty"))
			continue
		}
		if ref != strings.TrimSpace(ref) {
			res.Errors = append(res.Errors, field.Invalid(key, ref, "image must not contain surrounding whitespace"))
			continue
		}
		if strings.ContainsAny(ref, " \t\n\r") {
			res.Errors = append(res.Errors, field.Invalid(key, ref, "image must not contain whitespace"))
			continue
		}
		if !strings.Contains(ref, "/") {
			res.Errors = append(res.Errors, field.Invalid(key, ref, "image must be a full reference including registry host"))
			continue
		}
		last := ref[strings.LastIndex(ref, "/")+1:]
		if !strings.ContainsAny(last, ":@") {
			res.Errors = append(res.Errors, field.Invalid(key, ref, "image must include a tag or digest"))
		}
	}
}

// validateKubernetesVersion 校验 k8s 版本入口的形状。
// 版本范围/整组规则由加载阶段的 bom.WithVersionOverrides 负责；
// 这里只做不依赖 BOM 的格式校验。
func validateKubernetesVersion(res *ValidationResult, c *Cluster, p *field.Path) {
	version := strings.TrimSpace(c.KubernetesVersion)
	if version == "" {
		return
	}
	if !kubernetesVersionPattern.MatchString(version) {
		res.Errors = append(res.Errors, field.Invalid(p, c.KubernetesVersion,
			"must be a Kubernetes version like v1.37.4"))
	}
}
