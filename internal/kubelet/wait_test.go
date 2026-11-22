package kubelet

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"kubemono/internal/constants"
	controlplane "kubemono/internal/controlplane"
	"kubemono/internal/kubelet/kubeletfake"
)

// fakeClock 是一个由 Sleep 推进的确定性时钟。
type fakeClock struct {
	now    time.Time
	sleeps int
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1700000000, 0).UTC()}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) {
	c.sleeps++
	c.now = c.now.Add(d)
}

type okHealth struct{}

func (okHealth) Check(context.Context, controlplane.HealthCheck) error { return nil }

type failHealth struct{ err error }

func (f failHealth) Check(context.Context, controlplane.HealthCheck) error { return f.err }

// writeKubeletConf 创建 TLS Bootstrap 输出文件。
func writeKubeletConf(t *testing.T, paths constants.Paths) {
	t.Helper()
	p := paths.KubeconfigPath(constants.KubeletKubeconfigName)
	if err := os.MkdirAll(paths.Kubernetes(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForBootstrapScenarios(t *testing.T) {
	cases := []struct {
		name string
		// master 选择 master 路径。
		master bool
		// conf 决定 kubelet.conf 是否存在。
		conf bool
		// ready 是 NodeClient 上报的状态。
		ready bool
		// clientErr 让工厂失败（模拟“没有可用 kubeconfig”）。
		clientErr bool
		health    controlplane.HealthChecker
		wantErr   []string
		// wantClientCalled 断言工厂是否被调用。
		wantClientCalled bool
	}{
		{
			name: "master 不需要 bootstrap 文件", master: true, conf: false, ready: true,
			wantClientCalled: true,
		},
		{
			name: "worker 有 kubelet.conf 且 Ready", master: false, conf: true, ready: true,
			wantClientCalled: true,
		},
		{
			name:   "worker 没有 kubelet.conf 时不查询 Node",
			master: false, conf: false, ready: true,
			wantErr:          []string{"kubelet.conf", "bootstrap"},
			wantClientCalled: false,
		},
		{
			name:   "节点始终未 Ready",
			master: true, conf: false, ready: false,
			wantErr:          []string{"w1", "http://127.0.0.1:10248/healthz"},
			wantClientCalled: true,
		},
		{
			name:   "健康端点不可用时错误里带出探测失败",
			master: true, conf: false, ready: false,
			health:           failHealth{err: errors.New("connection refused")},
			wantErr:          []string{"w1", "http://127.0.0.1:10248/healthz", "connection refused"},
			wantClientCalled: true,
		},
		{
			name:   "客户端不可用会重试到超时",
			master: true, conf: false, ready: true, clientErr: true,
			wantErr:          []string{"没有可用凭据"},
			wantClientCalled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := constants.NewPaths(t.TempDir())
			if tc.conf {
				writeKubeletConf(t, paths)
			}
			clock := newFakeClock()
			fake := &kubeletfake.FakeNodeClient{Ready: map[string]bool{"w1": tc.ready}}
			calls := 0
			health := tc.health
			if health == nil {
				health = okHealth{}
			}
			opts := WaitOptions{
				Node:         "w1",
				Master:       tc.master,
				Paths:        paths,
				Timeout:      6 * time.Second,
				PollInterval: 2 * time.Second,
				Sleep:        clock.Sleep,
				Now:          clock.Now,
				Health:       health,
				NodeClient: func(context.Context) (NodeClient, error) {
					calls++
					if tc.clientErr {
						return nil, errors.New("没有可用凭据：kubelet.conf 尚未生成")
					}
					return fake, nil
				},
			}
			err := WaitForBootstrap(context.Background(), opts)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatal("应当超时报错")
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("错误 %q 缺少 %q", err.Error(), want)
					}
				}
				if clock.sleeps == 0 {
					t.Error("超时前必须轮询等待")
				}
			} else if err != nil {
				t.Fatalf("WaitForBootstrap: %v", err)
			}
			if tc.wantClientCalled && calls == 0 {
				t.Error("应当调用 NodeClient 工厂")
			}
			if !tc.wantClientCalled && calls != 0 {
				t.Errorf("不应调用 NodeClient 工厂（调用了 %d 次）", calls)
			}
		})
	}
}

func TestWaitForBootstrapStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := newFakeClock()
	opts := WaitOptions{
		Node:         "w1",
		Master:       true,
		Paths:        constants.NewPaths(t.TempDir()),
		Timeout:      time.Hour,
		PollInterval: time.Second,
		Sleep: func(d time.Duration) {
			cancel()
			clock.sleeps++
		},
		Now:    clock.Now,
		Health: okHealth{},
		NodeClient: func(context.Context) (NodeClient, error) {
			return &kubeletfake.FakeNodeClient{Ready: map[string]bool{}}, nil
		},
	}
	err := WaitForBootstrap(ctx, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应当返回 context.Canceled，实际 %v", err)
	}
}

func TestWaitOptionsDefaults(t *testing.T) {
	opts := WaitOptions{}
	opts.defaults()
	if opts.Timeout != DefaultBootstrapTimeout || opts.PollInterval != DefaultPollInterval {
		t.Fatalf("默认超时/间隔不对: %s/%s", opts.Timeout, opts.PollInterval)
	}
	if got := KubeletHealthCheck().Address; got != "127.0.0.1:10248" {
		t.Fatalf("默认健康端点 = %q", got)
	}
}
