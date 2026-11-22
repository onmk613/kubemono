package cert_test

import (
	"strings"
	"testing"

	"kubemono/internal/cli/clitest"
)

func TestCertRenewNonInteractiveAborts(t *testing.T) {
	out, err := clitest.ExecuteRoot(t, "cert", "renew", "--data-dir", t.TempDir())
	if err == nil {
		t.Fatalf("non-interactive renew must abort:\n%s", out)
	}
	if !strings.Contains(err.Error(), "已取消") {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}
}

// TestCertRenewRejectsUnknownCert 固定 --cert 的可用值校验。
func TestCertRenewRejectsUnknownCert(t *testing.T) {
	out, err := clitest.ExecuteRoot(t, "cert", "renew", "--cert", "nope", "--assume-yes", "--data-dir", t.TempDir())
	if err == nil {
		t.Fatalf("unknown --cert must fail:\n%s", out)
	}
	if !strings.Contains(err.Error(), "未知证书") {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}
}
