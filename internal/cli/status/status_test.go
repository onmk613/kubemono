package status

import (
	"context"
	"strings"
	"testing"

	"kubemono/internal/cli/options"
)

// TestCollectWithoutConfig 覆盖“配置缺失也能给出本机状态”的路径：
// status 不应因为读不到 cluster.yaml 就整体失败。
func TestCollectWithoutConfig(t *testing.T) {
	opts := options.Default()
	opts.TestRoot = t.TempDir()
	rep, err := collect(context.Background(), opts)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if rep.DataDir == "" {
		t.Fatal("DataDir 不能为空")
	}
	if !strings.Contains(rep.DataDir, opts.TestRoot) {
		t.Fatalf("DataDir %q 未重定位到测试根", rep.DataDir)
	}
}

func TestReportProblems(t *testing.T) {
	ready := false
	active := true
	rep := &Report{Installed: true, NodeReady: &ready}
	rep.Problems = rep.problems()
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "未 Ready") {
		t.Fatalf("NodeNotReady 应成为问题，got %v", rep.Problems)
	}

	rep = &Report{
		Installed:    true,
		DesiredDrift: []string{"x"},
		Pending:      []string{"kubelet"},
		Certs:        &CertView{Expired: 1},
		Units: []UnitView{
			{Unit: "kubelet.service", Required: true, Active: &active},
			{Unit: "kube-apiserver.service", Required: true},
			{Unit: "kubemono-cert-renew.service", Result: "failed"},
		},
	}
	problems := rep.problems()
	for _, want := range []string{"漂移", "未完成的重启", "已过期", "kube-apiserver", "timer 上次执行失败"} {
		found := false
		for _, p := range problems {
			if strings.Contains(p, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("problems 缺少 %q：%v", want, problems)
		}
	}
}
