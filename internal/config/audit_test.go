package config

import (
	"strings"
	"testing"
)

func TestAuditDefaults(t *testing.T) {
	c := DefaultCluster()
	a := c.ControlPlane.Audit
	if !a.IsEnabled() || a.MaxAge() != 7 || a.MaxBackup() != 10 || a.MaxSize() != 100 {
		t.Fatalf("audit 默认值不正确: %+v", a)
	}
	if a.Policy != "" {
		t.Fatalf("默认 policy 应为空（使用内置策略），got %q", a.Policy)
	}
}

func TestAuditPolicyValidation(t *testing.T) {
	c := DefaultCluster()
	c.ControlPlane.Audit.Policy = `apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Metadata
`
	if res := Validate(c); !res.OK() {
		t.Fatalf("合法审计策略被拒绝: %v", res.Errors)
	}

	bad := []struct {
		name   string
		policy string
	}{
		{"yaml", "not: [valid"},
		{"apiVersion", "apiVersion: v1\nkind: Policy\nrules:\n  - level: Metadata\n"},
		{"kind", "apiVersion: audit.k8s.io/v1\nkind: Config\nrules:\n  - level: Metadata\n"},
		{"rules", "apiVersion: audit.k8s.io/v1\nkind: Policy\n"},
		{"level", "apiVersion: audit.k8s.io/v1\nkind: Policy\nrules:\n  - level: Everything\n"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCluster()
			c.ControlPlane.Audit.Policy = tc.policy
			if res := Validate(c); res.OK() {
				t.Fatal("非法审计策略应被拒绝")
			}
		})
	}
}

func TestAuditDisabledRejectsPolicy(t *testing.T) {
	c := DefaultCluster()
	c.ControlPlane.Audit.Enabled = BoolPtr(false)
	c.ControlPlane.Audit.Policy = "apiVersion: audit.k8s.io/v1\nkind: Policy\nrules:\n  - level: Metadata\n"
	res := Validate(c)
	if res.OK() {
		t.Fatal("关闭 audit 时不应接受 policy")
	}
	if !strings.Contains(res.Errors.ToAggregate().Error(), "policy") {
		t.Fatalf("错误应指向 policy: %v", res.Errors)
	}
}

func TestAuditReservedExtraArgs(t *testing.T) {
	c := DefaultCluster()
	c.ControlPlane.APIServer.ExtraArgs = map[string]string{"audit-log-maxage": "14"}
	res := Validate(c)
	if res.OK() {
		t.Fatal("audit-log-maxage 不应允许写在 apiServer.extraArgs")
	}
	if !strings.Contains(res.Errors.ToAggregate().Error(), "controlPlane.audit") {
		t.Fatalf("错误应指向 controlPlane.audit: %v", res.Errors)
	}
}
