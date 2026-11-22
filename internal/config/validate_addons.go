package config

import (
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func validateAddons(res *ValidationResult, c *Cluster, spec *field.Path) {
	if r := c.Addons.CoreDNS.Replicas; r != nil && *r < 0 {
		res.Errors = append(res.Errors, field.Invalid(spec.Child("coredns", "replicas"), *r, "must not be negative"))
	}
	if c.Addons.CoreDNS.Enabled != nil && *c.Addons.CoreDNS.Enabled && c.Network.ClusterDNS == "" {
		res.Errors = append(res.Errors, field.Required(field.NewPath("network", "clusterDNS"), "required when coredns is enabled"))
	}
	switch f := c.Addons.CoreDNS.LogFormat; f {
	case "", "text", "json":
	default:
		res.Errors = append(res.Errors, field.NotSupported(spec.Child("coredns", "logFormat"), f, []string{"text", "json"}))
	}
}
