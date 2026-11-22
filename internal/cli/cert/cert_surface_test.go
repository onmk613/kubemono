package cert_test

import (
	"testing"

	"kubemono/internal/cli/cert"
	"kubemono/internal/cli/options"
)

func TestNewRegistersSubcommandsAndFlags(t *testing.T) {
	cmd := cert.New(options.Default())
	names := map[string]bool{}
	for _, c := range cmd.Commands() {
		names[c.Name()] = true
	}
	for _, want := range []string{"check-expiration", "renew", "show"} {
		if !names[want] {
			t.Errorf("cert 缺少子命令 %s（实际 %v）", want, names)
		}
	}
	var renew = cmd.Commands()[0]
	for _, c := range cmd.Commands() {
		if c.Name() == "renew" {
			renew = c
		}
	}
	for _, flag := range []string{"cert", "all", "assume-yes", "dry-run"} {
		if renew.Flags().Lookup(flag) == nil {
			t.Errorf("cert renew 缺少 flag --%s", flag)
		}
	}
}
