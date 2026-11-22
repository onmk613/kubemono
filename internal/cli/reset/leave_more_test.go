package reset

import (
	"testing"

	"kubemono/internal/cli/options"
)

func TestNewOptionsRequiresClusterCredential(t *testing.T) {
	opts := options.Default()
	opts.TestRoot = t.TempDir()
	if _, err := newLeaveOptions(opts, false, false, false, false); err == nil {
		t.Fatal("没有 admin kubeconfig 时 NewOptions 必须失败")
	}
}
