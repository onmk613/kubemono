package kubelet

import (
	"context"
	"strings"
	"testing"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"kubemono/internal/bootstrap"
	"kubemono/internal/kubelet/kubeletfake"
)

func nodeObject(name, version string, ready bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: version},
		},
	}
}

func servingCSR(name, node string, decided bool) *certificatesv1.CertificateSigningRequest {
	csr := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			SignerName: bootstrap.SignerKubeletServing,
			Username:   "system:node:" + node,
		},
	}
	if decided {
		csr.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
			Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue,
		}}
	}
	return csr
}

func TestClientNodeClientNodeReady(t *testing.T) {
	cases := []struct {
		name    string
		objects []runtime.Object
		version string
		node    string
		want    bool
	}{
		{name: "Ready 且版本一致", objects: []runtime.Object{nodeObject("w1", "v1.31.4", true)}, version: "v1.31.4", node: "w1", want: true},
		{name: "Ready 但版本漂移", objects: []runtime.Object{nodeObject("w1", "v1.30.9", true)}, version: "v1.31.4", node: "w1", want: false},
		{name: "未 Ready", objects: []runtime.Object{nodeObject("w1", "v1.31.4", false)}, version: "v1.31.4", node: "w1", want: false},
		{name: "节点还没注册", objects: nil, version: "v1.31.4", node: "w1", want: false},
		{name: "不校验版本", objects: []runtime.Object{nodeObject("w1", "v1.30.9", true)}, version: "", node: "w1", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(tc.objects...)
			nc := NewNodeClient(client, tc.version)
			got, err := nc.NodeReady(context.Background(), tc.node)
			if err != nil {
				t.Fatalf("NodeReady: %v", err)
			}
			if got != tc.want {
				t.Fatalf("NodeReady = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClientNodeClientRequiresClientAndName(t *testing.T) {
	if _, err := (&ClientNodeClient{}).NodeReady(context.Background(), "w1"); err == nil {
		t.Fatal("没有客户端时必须报错")
	}
	nc := NewNodeClient(fake.NewSimpleClientset(), "v1.31.4")
	if _, err := nc.NodeReady(context.Background(), ""); err == nil {
		t.Fatal("节点名为空时必须报错")
	}
}

func TestPendingServingCSRs(t *testing.T) {
	client := fake.NewSimpleClientset(
		servingCSR("pending-w1", "w1", false),
		servingCSR("pending-w2", "w2", false),
		servingCSR("approved-w1", "w1", true),
		&certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "client-csr"},
			Spec: certificatesv1.CertificateSigningRequestSpec{
				SignerName: bootstrap.SignerKubeletClient,
				Username:   "system:node:w1",
			},
		},
	)
	var selector string
	client.PrependReactor("list", "certificatesigningrequests", func(action ktesting.Action) (bool, runtime.Object, error) {
		selector = action.(ktesting.ListActionImpl).GetListRestrictions().Fields.String()
		return false, nil, nil
	})

	nc := NewNodeClient(client, "v1.31.4")
	nc.Node = "w1"
	n, err := nc.PendingServingCSRs(context.Background())
	if err != nil {
		t.Fatalf("PendingServingCSRs: %v", err)
	}
	if n != 1 {
		t.Fatalf("w1 的待审批 serving CSR = %d, want 1", n)
	}
	if !strings.Contains(selector, bootstrap.SignerKubeletServing) {
		t.Fatalf("必须按 signerName 过滤，实际 selector = %q", selector)
	}

	// 不限定节点时统计所有未决 serving CSR。
	all := NewNodeClient(client, "v1.31.4")
	n, err = all.PendingServingCSRs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("全部待审批 serving CSR = %d, want 2", n)
	}
}

func TestFakeNodeClientCountsCalls(t *testing.T) {
	f := &kubeletfake.FakeNodeClient{Ready: map[string]bool{"w1": true}, Pending: 3}
	ctx := context.Background()
	if ok, err := f.NodeReady(ctx, "w1"); err != nil || !ok {
		t.Fatalf("NodeReady = %v, %v", ok, err)
	}
	if n, err := f.PendingServingCSRs(ctx); err != nil || n != 3 {
		t.Fatalf("PendingServingCSRs = %d, %v", n, err)
	}
	if f.NodeReadyCalls != 1 || f.PendingCalls != 1 {
		t.Fatalf("调用计数不对: %d/%d", f.NodeReadyCalls, f.PendingCalls)
	}
}

func TestNodeReadyAllowsNetworkNotReady(t *testing.T) {
	node := nodeObject("w1", "v1.31.4", false)
	node.Status.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady",
		Message: "container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:Network plugin returns error: cni plugin not initialized",
	}}
	strict := NewNodeClient(fake.NewSimpleClientset(node), "v1.31.4")
	if ready, err := strict.NodeReady(context.Background(), "w1"); err != nil || ready {
		t.Fatalf("默认语义下 CNI 未就绪必须是 NotReady: ready=%v err=%v", ready, err)
	}
	lenient := NewNodeClient(fake.NewSimpleClientset(node.DeepCopy()), "v1.31.4")
	lenient.AllowNetworkNotReady = true
	if ready, err := lenient.NodeReady(context.Background(), "w1"); err != nil || !ready {
		t.Fatalf("AllowNetworkNotReady 时只缺 CNI 应视为完成: ready=%v err=%v", ready, err)
	}

	// 其它原因（例如 PLEG）不能被放过。
	other := nodeObject("w2", "v1.31.4", false)
	other.Status.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady", Message: "PLEG is not healthy",
	}}
	lenient2 := NewNodeClient(fake.NewSimpleClientset(other), "v1.31.4")
	lenient2.AllowNetworkNotReady = true
	if ready, _ := lenient2.NodeReady(context.Background(), "w2"); ready {
		t.Fatal("非网络原因的 NotReady 不能视为完成")
	}
}
