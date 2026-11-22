package leave

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"kubemono/internal/constants"
	"kubemono/internal/etcd"
	"kubemono/internal/util/output"
)

// fakeEtcd 记录 etcd 成员操作。
type fakeEtcd struct {
	members      []etcd.Member
	removed      []uint64
	canRemoveIDs []uint64
	canRemoveErr error
	order        *[]string
	err          error
}

func (f *fakeEtcd) List(context.Context) ([]etcd.Member, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.members, nil
}

func (f *fakeEtcd) CanRemove(_ context.Context, id uint64) error {
	f.canRemoveIDs = append(f.canRemoveIDs, id)
	return f.canRemoveErr
}

func (f *fakeEtcd) Remove(_ context.Context, id uint64) error {
	f.removed = append(f.removed, id)
	if f.order != nil {
		*f.order = append(*f.order, "etcd")
	}
	return nil
}

func healthyMembers(names ...string) []etcd.Member {
	out := make([]etcd.Member, 0, len(names))
	for i, name := range names {
		out = append(out, etcd.Member{ID: uint64(i + 1), Name: name,
			ClientURLs: []string{fmt.Sprintf("https://10.0.0.%d:2379", i+1)}})
	}
	return out
}

// harness 组装 leave 的测试环境。
type harness struct {
	paths  constants.Paths
	client *fake.Clientset
	etcd   *fakeEtcd
	reset  *fakeReset
	order  []string
	opts   *Options
}

type fakeReset struct {
	calls int
	opts  []*Options
	order *[]string
	err   error
}

func (f *fakeReset) run(_ context.Context, opts *Options) error {
	f.calls++
	f.opts = append(f.opts, opts)
	if f.order != nil {
		*f.order = append(*f.order, "reset")
	}
	return f.err
}

func newHarness(t *testing.T, pods ...*corev1.Pod) *harness {
	t.Helper()
	paths := constants.NewPaths(t.TempDir())
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}}
	objects := []runtime.Object{node}
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	client := fake.NewSimpleClientset(objects...)

	h := &harness{paths: paths, client: client}
	h.etcd = &fakeEtcd{}
	h.order = []string{}
	h.etcd.order = &h.order
	h.reset = &fakeReset{order: &h.order}

	client.PrependReactor("delete", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		h.order = append(h.order, "node-delete")
		return false, nil, nil
	})

	h.opts = &Options{
		Paths:            paths,
		Client:           client,
		NodeName:         "w1",
		RemoveEtcdMember: true,
		Report:           output.Discard(),
		Etcd:             h.etcd,
		Reset:            h.reset.run,
	}
	return h
}

func pod(name string, mutators ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default",
		// 默认是控制器管理的 Pod；需要裸 Pod 的用例显式清空 OwnerReferences。
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web"}},
	}}
	for _, mutate := range mutators {
		mutate(p)
	}
	return p
}

func TestLeaveOrderAndEffects(t *testing.T) {
	h := newHarness(t, pod("web-1"))
	h.etcd.members = healthyMembers("m1", "w1")

	res, err := Leave(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	want := []string{"node-delete", "etcd", "reset"}
	if strings.Join(h.order, ",") != strings.Join(want, ",") {
		t.Fatalf("执行顺序不符：\n got: %v\nwant: %v", h.order, want)
	}
	if !res.NodeDeleted || !res.EtcdMemberRemoved || !res.Reset {
		t.Fatalf("Result 不完整：%+v", res)
	}
	if _, err := h.client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Node 对象应被删除：%v", err)
	}
	if h.reset.opts[0].KeepData {
		t.Fatal("未指定 --keep-data 时不应保留数据")
	}
}

func TestEtcdMemberRemovalChecksTargetBeforeRemoving(t *testing.T) {
	h := newHarness(t)
	h.etcd.members = healthyMembers("w1", "m2")
	h.etcd.canRemoveErr = errors.New("移除后健康成员不足法定人数")

	err := removeEtcdMember(context.Background(), h.opts, &Result{NodeName: "w1"})
	if err == nil || !strings.Contains(err.Error(), "法定人数") {
		t.Fatalf("法定人数校验失败时必须拒绝：%v", err)
	}
	if len(h.etcd.canRemoveIDs) != 1 || h.etcd.canRemoveIDs[0] != 1 {
		t.Fatalf("CanRemove 必须收到目标成员 ID，实际 %v", h.etcd.canRemoveIDs)
	}
	if len(h.etcd.removed) != 0 {
		t.Fatal("拒绝时不应移除任何成员")
	}
}

func TestEtcdMemberRemovalRefusedWithoutForce(t *testing.T) {
	h := newHarness(t)
	h.etcd.members = healthyMembers("w1") // 单成员集群
	h.etcd.canRemoveErr = errors.New("不能移除最后一个投票成员")

	err := removeEtcdMember(context.Background(), h.opts, &Result{NodeName: "w1"})
	if err == nil {
		t.Fatal("法定人数不足时必须拒绝")
	}
	if len(h.etcd.removed) != 0 {
		t.Fatal("拒绝时不应移除任何成员")
	}

	h2 := newHarness(t)
	h2.etcd.members = healthyMembers("w1")
	h2.opts.Force = true
	if err := removeEtcdMember(context.Background(), h2.opts, &Result{NodeName: "w1"}); err != nil {
		t.Fatalf("--force 应跳过法定人数校验：%v", err)
	}
	if len(h2.etcd.canRemoveIDs) != 0 {
		t.Fatalf("--force 不应调用 CanRemove：%v", h2.etcd.canRemoveIDs)
	}
	if len(h2.etcd.removed) != 0 {
		t.Fatalf("单成员 etcd 在 --force 下应跳过成员移除：%v", h2.etcd.removed)
	}
}

func TestClusterOperationsRequireClient(t *testing.T) {
	opts := &Options{Paths: constants.NewPaths(t.TempDir()), NodeName: "w1"}
	if _, err := Leave(context.Background(), opts); !errors.Is(err, ErrNoClient) {
		t.Fatalf("缺少 client 时应返回 ErrNoClient：%v", err)
	}
	if !strings.Contains(ErrNoClient.Error(), "admin kubeconfig") {
		t.Fatalf("错误文本应说明需要 admin kubeconfig：%v", ErrNoClient)
	}
}

func TestLeaveDryRunTouchesNothing(t *testing.T) {
	h := newHarness(t, pod("web-1"))
	h.etcd.members = healthyMembers("m1", "w1")
	h.opts.DryRun = true
	res, err := Leave(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("Leave --dry-run: %v", err)
	}
	if len(h.etcd.removed) != 0 {
		t.Fatalf("dry-run 不应改集群：etcd=%v", h.etcd.removed)
	}
	if _, err := h.client.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{}); err != nil {
		t.Fatalf("dry-run 不应删除 Node 对象：%v", err)
	}
	if !res.Reset {
		t.Fatal("dry-run 仍应报告会执行 reset")
	}
	if !h.reset.opts[0].DryRun {
		t.Fatal("dry-run 必须把 DryRun 传给 reset")
	}
}

// TestFindMemberByName 固定 leave 的成员匹配语义：按节点名查找（kubemono 用
// 节点名作 etcd --name）。不按子串地址匹配：本机 10.0.0.1 不能误匹配另一台
// 10.0.0.11 的成员；成员 URL 集合由 internal/etcd 统一匹配。
func TestFindMemberByName(t *testing.T) {
	members := []etcd.Member{
		{ID: 1, Name: "m1", ClientURLs: []string{"https://10.0.0.1:2379"}},
		{ID: 2, Name: "w1", PeerURLs: []string{"https://10.0.0.2:2380"}},
	}
	if member, ok := etcd.FindMember(members, "w1", nil); !ok || member.ID != 2 {
		t.Fatalf("按名字查找失败：%+v", member)
	}
	// 裸地址不参与匹配：10.0.0.1 只是 10.0.0.11 的前缀，子串匹配会误中。
	if _, ok := etcd.FindMember(members, "nope", nil); ok {
		t.Fatal("不存在的成员不应被找到")
	}
}

func TestLeaveNodeDeleteFailureKeepsEtcdMember(t *testing.T) {
	h := newHarness(t, pod("web-1"))
	h.etcd.members = healthyMembers("m1", "w1")
	h.client.PrependReactor("delete", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTimeoutError("node delete timeout", 1)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := Leave(ctx, h.opts)
	if err == nil {
		t.Fatal("Node 删除失败时 Leave 必须失败")
	}
	if len(h.etcd.removed) != 0 {
		t.Fatalf("Node 删除失败后不应移除 etcd 成员，实际 %v", h.etcd.removed)
	}
	if h.reset.calls != 0 {
		t.Fatal("Node 删除失败后不应 reset 本机")
	}
	if res.NodeDeleted {
		t.Fatal("NodeDeleted 不应为 true")
	}
}

// TestLeaveRetryAfterPartialState 覆盖“Node 已删、etcd 成员已不在”的
// 上一次失败现场：重跑必须把剩余步骤做完，而不是报 member not found。
func TestLeaveRetryAfterPartialState(t *testing.T) {
	h := newHarness(t)
	// 模拟上一次已经删掉 Node 和 etcd 成员，但配置里仍有 w1。
	if err := h.client.CoreV1().Nodes().Delete(context.Background(), "w1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.etcd.members = healthyMembers("m1") // w1 已移除
	h.opts.Force = true                   // 上一次 leave 已删 Node，允许从该状态继续
	h.opts.Reset = h.reset.run

	res, err := Leave(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("重跑 Leave: %v", err)
	}
	if !res.NodeDeleted || res.EtcdMemberRemoved {
		t.Fatalf("重跑结果不符：%+v", res)
	}
	if h.reset.calls != 1 {
		t.Fatalf("重跑应继续 reset，实际 %d 次", h.reset.calls)
	}
}

// 单成员 etcd 无法通过 MemberRemove 移除自己；Leave --force 应跳过成员删除，
// 继续写配置并 reset 本机（单节点集群 cleanup 路径）。
func TestLeaveSingleMemberForceSkipsEtcdRemoval(t *testing.T) {
	h := newHarness(t, pod("web-1"))
	h.etcd.members = healthyMembers("w1")
	h.opts.Force = true

	if _, err := Leave(context.Background(), h.opts); err != nil {
		t.Fatalf("单成员 --force Leave: %v", err)
	}
	if len(h.etcd.removed) != 0 {
		t.Fatalf("单成员不应调用 Remove，实际 %v", h.etcd.removed)
	}
	if h.reset.calls != 1 {
		t.Fatalf("应继续 reset 本机，实际 %d 次", h.reset.calls)
	}
}
