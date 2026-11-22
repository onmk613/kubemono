package phase

import (
	"errors"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// TestKubeClientPrefersInjectedClient: 显式注入的 Client 必须优先，
// 且不得再调用 ClientFunc（否则 install/join 的 phase 会多读一次磁盘）。
// ClientFunc must not be called (otherwise install/join 的 phase reads the disk twice).
func TestKubeClientPrefersInjectedClient(t *testing.T) {
	injected := fake.NewSimpleClientset()
	called := false
	c := &Context{
		Client: injected,
		ClientFunc: func() (kubernetes.Interface, error) {
			called = true
			return fake.NewSimpleClientset(), nil
		},
	}
	got, err := c.KubeClient()
	if err != nil {
		t.Fatalf("KubeClient: %v", err)
	}
	if got != kubernetes.Interface(injected) {
		t.Fatalf("KubeClient 应返回注入的 Client")
	}
	if called {
		t.Fatal("注入 Client 时不应调用 ClientFunc")
	}
}

// TestKubeClientFallsBackToClientFunc: install 依赖这条路径——决定计划时还没有
// admin.conf，只有真正执行 phase 时才用 ClientFunc 读凭据。
func TestKubeClientFallsBackToClientFunc(t *testing.T) {
	lazy := fake.NewSimpleClientset()
	c := &Context{ClientFunc: func() (kubernetes.Interface, error) { return lazy, nil }}
	got, err := c.KubeClient()
	if err != nil {
		t.Fatalf("KubeClient: %v", err)
	}
	if got != kubernetes.Interface(lazy) {
		t.Fatalf("KubeClient 应返回 ClientFunc 构造的 Client")
	}
}

// TestKubeClientPropagatesClientFuncError: 凭据缺失的原因必须透出，
// 而不是被替换成"缺少客户端"。
func TestKubeClientPropagatesClientFuncError(t *testing.T) {
	boom := errors.New("找不到集群凭据 /etc/kubernetes/admin.conf")
	c := &Context{ClientFunc: func() (kubernetes.Interface, error) { return nil, boom }}
	if _, err := c.KubeClient(); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// TestKubeClientWithoutProvider: 两条路径都不可用时返回统一的哨兵错误。
func TestKubeClientWithoutProvider(t *testing.T) {
	if _, err := (&Context{}).KubeClient(); !errors.Is(err, ErrNoClusterClient) {
		t.Fatalf("err = %v, want ErrNoClusterClient", err)
	}
	var nilCtx *Context
	if _, err := nilCtx.KubeClient(); !errors.Is(err, ErrNoClusterClient) {
		t.Fatalf("nil context err = %v, want ErrNoClusterClient", err)
	}
}

// TestKubeClientNilResultFromClientFunc: ClientFunc 返回 (nil, nil) 时
// 不能把 nil 当成可用客户端交给 phase。
func TestKubeClientNilResultFromClientFunc(t *testing.T) {
	c := &Context{ClientFunc: func() (kubernetes.Interface, error) { return nil, nil }}
	if _, err := c.KubeClient(); !errors.Is(err, ErrNoClusterClient) {
		t.Fatalf("err = %v, want ErrNoClusterClient", err)
	}
}

// TestClientPresenceIgnoresClientFunc: Client 只反映"现在就有一个客户端"，
// 惰性提供者不算，避免 phase 用错判据。
func TestClientPresenceIgnoresClientFunc(t *testing.T) {
	c := &Context{ClientFunc: func() (kubernetes.Interface, error) { return fake.NewSimpleClientset(), nil }}
	if c.Client != nil {
		t.Fatal("只有 ClientFunc 时 Client 应为 nil")
	}
	c.Client = fake.NewSimpleClientset()
	if c.Client == nil {
		t.Fatal("注入 Client 后 Client 应非 nil")
	}
}
