package install

import (
	"context"
	"slices"
	"strings"
	"testing"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/identity"
)

// TestSelectPhasesKeepsPlanOrder 固定 --only 的排序语义：输出按计划顺序，
// 而不是命令行顺序。local-proxy 必须先于 kubelet（worker 的 TLS Bootstrap
// 走 127.0.0.1:6443），用户写反了也不能把顺序带错。
func TestSelectPhasesKeepsPlanOrder(t *testing.T) {
	planned := []string{"host-init", "runtime", "kubeconfig", "local-proxy", "kubelet", "network"}
	got, err := selectPhases(planned, []string{"kubelet", "local-proxy"})
	if err != nil {
		t.Fatalf("selectPhases: %v", err)
	}
	want := []string{"local-proxy", "kubelet"}
	if !slices.Equal(got, want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
}

func TestSelectPhasesRejectsUnknownAndEmpty(t *testing.T) {
	planned := []string{"kubeconfig", "local-proxy"}
	if _, err := selectPhases(planned, []string{"etcd"}); err == nil ||
		!strings.Contains(err.Error(), "不在本机修复计划中") {
		t.Fatalf("计划外的 phase 必须被拒绝并列出可用项，got %v", err)
	}
	if _, err := selectPhases(planned, []string{" ", ""}); err == nil ||
		!strings.Contains(err.Error(), "至少需要一个 phase") {
		t.Fatalf("空的 --only 必须报错，got %v", err)
	}
}

// repairOptions 构造一个已安装 worker 的 sync 计划输入。
func repairOptions(t *testing.T) *Options {
	t.Helper()
	opts := testOptions(t, "w1")
	opts.ClusterID = "cid-1"
	opts.ForceAction = ActionSync
	// 本机 hostname 必须与 node.yaml 里的 NodeName 一致，否则会触发改名保护。
	opts.Resolver.Hostname = func() (string, error) { return "w1", nil }
	store := &identity.Store{Dir: opts.Paths.Kubemono()}
	if err := store.Save(&clusterconfig.NodeConfig{
		ClusterID: "cid-1",
		HostName:  "w1",
		NodeName:  "w1",
		Roles:     []clusterconfig.Role{clusterconfig.RoleWorker},
	}); err != nil {
		t.Fatal(err)
	}
	return opts
}

// TestDecideSyncOnlyFiltersPlan 验证 --only 真的作用到计划上，
// 而不是只影响命令行帮助：未选中的 phase 不能出现在执行列表里。
func TestDecideSyncOnlyFiltersPlan(t *testing.T) {
	opts := repairOptions(t)
	opts.Only = []string{"local-proxy"}
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !slices.Equal(plan.Phases, []string{"local-proxy"}) {
		t.Fatalf("phases = %v, want [local-proxy]", plan.Phases)
	}
}

// TestDecideSyncOnlyRejectsPhaseOutsidePlan 覆盖角色相关的拒绝：
// worker 的计划里没有 etcd，指定它必须报错而不是静默跳过。
func TestDecideSyncOnlyRejectsPhaseOutsidePlan(t *testing.T) {
	opts := repairOptions(t)
	opts.Only = []string{"etcd"}
	if _, err := Decide(context.Background(), opts); err == nil ||
		!strings.Contains(err.Error(), "不在本机修复计划中") {
		t.Fatalf("worker 上 --only etcd 必须被拒绝，got %v", err)
	}
}

// TestDecideOnlyRefusedForInit 固定“--only 只属于 repair”：
// init/join 少跑一步会留下半装节点，因此必须在计划阶段就拒绝。
func TestDecideOnlyRefusedForInit(t *testing.T) {
	opts := initOptions(t, "m1")
	opts.Only = []string{"certs"}
	if _, err := Decide(context.Background(), opts); err == nil ||
		!strings.Contains(err.Error(), "--only") {
		t.Fatalf("init 带 --only 必须被拒绝，got %v", err)
	}
}

// TestDecideSyncRebindIdentity：repair 时 --node 与本机身份不一致会改写
// node.yaml，因此必须显式加 --rebind-identity。
func TestDecideSyncRebindIdentity(t *testing.T) {
	opts := repairOptions(t)
	opts.Node = "m2"
	if _, err := Decide(context.Background(), opts); err == nil ||
		!strings.Contains(err.Error(), "--rebind-identity") {
		t.Fatalf("身份不一致必须要求 --rebind-identity，got %v", err)
	}
	opts.RebindIdentity = true
	plan, err := Decide(context.Background(), opts)
	if err != nil {
		t.Fatalf("带 --rebind-identity 应允许改名迁移: %v", err)
	}
	if plan.ConfigName != "m2" {
		t.Fatalf("ConfigName = %q, want m2", plan.ConfigName)
	}
}
