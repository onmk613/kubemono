package reset

import (
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/utils"
)

// TestNewOptionsForwardsKeepFlags 守住 `kubemono reset --leave --keep-data`：
// 两个 keep 开关必须原样落到 leave.Options；否则退群后仍会删除
// etcd/kubelet 数据目录（不可逆）。
func TestNewLeaveOptionsForwardsKeepFlags(t *testing.T) {
	orig := utils.NewClusterClient
	t.Cleanup(func() { utils.NewClusterClient = orig })
	utils.NewClusterClient = func(*options.GlobalOptions) (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(), nil
	}

	for _, tc := range []struct {
		keepData     bool
		keepHostInit bool
	}{
		{false, false},
		{true, false},
		{false, true},
		{true, true},
	} {
		opts := options.Default()
		opts.TestRoot = t.TempDir()
		lo, err := newLeaveOptions(opts, tc.keepData, tc.keepHostInit, false, false)
		if err != nil {
			t.Fatalf("newLeaveOptions: %v", err)
		}
		if lo.KeepData != tc.keepData {
			t.Errorf("KeepData = %v，期望 %v", lo.KeepData, tc.keepData)
		}
		if lo.KeepHostInit != tc.keepHostInit {
			t.Errorf("KeepHostInit = %v，期望 %v", lo.KeepHostInit, tc.keepHostInit)
		}
	}
}
