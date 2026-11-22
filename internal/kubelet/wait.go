package kubelet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"kubemono/internal/constants"
	controlplane "kubemono/internal/controlplane"
	"kubemono/internal/util/fsutil"
)

// 引导等待的默认值。
const (
	// DefaultBootstrapTimeout 约束“等到节点上报 Ready”的时间。
	DefaultBootstrapTimeout = 5 * time.Minute
	// DefaultPollInterval 是重新检查节点的间隔。
	DefaultPollInterval = 2 * time.Second
)

// NodeClientFactory 构造 NodeClient；每次轮询都会重试，因为 worker 的
// kubelet.conf 只有在 TLS Bootstrap 完成后才会出现。
type NodeClientFactory func(ctx context.Context) (NodeClient, error)

// WaitOptions 控制 WaitForBootstrap。
type WaitOptions struct {
	// Node 是要等待的节点名。
	Node string
	// Master 跳过等待 kubelet.conf：master 的 kubelet.conf 由 kubeconfig
	// 阶段直签，TLS Bootstrap 只发生在 worker 上。
	Master bool
	// Paths 解析 kubeconfig 路径。
	Paths constants.Paths
	// NodeClient 是（可重试的）客户端工厂。
	NodeClient NodeClientFactory
	// Health 探测 kubelet 健康端点；它只用于丰富错误信息与诊断，
	// 是否就绪由 Node Ready 决定。
	Health controlplane.HealthChecker
	// Timeout 约束整个等待过程；为零时使用 DefaultBootstrapTimeout。
	Timeout time.Duration
	// PollInterval 是轮询周期；为零时使用 DefaultPollInterval。
	PollInterval time.Duration
	// Sleep 可注入以便测试；为 nil 时使用 time.Sleep。
	Sleep func(time.Duration)
	// Now 是计算截止时间所用的时钟；为 nil 时使用 time.Now。
	Now func() time.Time
	// Report 接收进度消息。
	Report func(string, ...any)
	// LocalOnly 只等待本机 kubelet healthz 端点。
	// master 上 kubelet 先于 etcd/控制面启动（静态 Pod 要靠它拉起，、），
	// 此时还没有 apiserver，无法查询 Node 对象；注册与标签由之后的
	// mark-control-plane phase 等待并确认。
	LocalOnly bool
}

// KubeletHealthCheck 返回 kubelet healthz 探针（附录 A：127.0.0.1:10248）。
func KubeletHealthCheck() controlplane.HealthCheck {
	return controlplane.ComponentHealth(controlplane.ComponentKubelet)
}

func (o *WaitOptions) defaults() {
	if o.Timeout <= 0 {
		o.Timeout = DefaultBootstrapTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Report == nil {
		o.Report = func(string, ...any) {}
	}
	if o.Health == nil {
		o.Health = controlplane.NewHTTPChecker()
	}
}

// WaitForBootstrap 等待节点加入并进入 Ready。
// worker：先等 kubelet.conf（kubelet 用 bootstrap-kubeconfig 换来的正式凭据）
// 出现，再等 Node Ready；master：kubelet.conf 由 kubeconfig 阶段直签，只等
func WaitForBootstrap(ctx context.Context, opts WaitOptions) error {
	opts.defaults()
	deadline := opts.Now().Add(opts.Timeout)
	waitingLogged := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := readyOnce(ctx, opts)
		if err == nil && ready {
			if opts.LocalOnly {
				opts.Report("kubelet 已在本机运行（%s 通过）", KubeletHealthCheck().URL())
			} else {
				opts.Report("节点 %s 已 Ready", opts.Node)
			}
			return nil
		}
		if !waitingLogged {
			if opts.LocalOnly {
				opts.Report("等待本机 kubelet 健康（最长 %s）", opts.Timeout)
			} else {
				opts.Report("等待节点 %s Ready（最长 %s）", opts.Node, opts.Timeout)
			}
			waitingLogged = true
		}
		if !opts.Now().Before(deadline) {
			return bootstrapTimeoutError(ctx, opts, err)
		}
		opts.Sleep(opts.PollInterval)
	}
}

// readyOnce 执行一次就绪检查。
func readyOnce(ctx context.Context, opts WaitOptions) (bool, error) {
	if opts.LocalOnly {
		if err := healthDiagnostic(ctx, opts); err != nil {
			return false, err
		}
		return true, nil
	}
	if !opts.Master {
		conf := opts.Paths.KubeconfigPath(constants.KubeletKubeconfigName)
		if !fsutil.FileExists(conf) {
			return false, fmt.Errorf("等待 %s 出现（kubelet 正在用 bootstrap-kubeconfig 换取正式凭据）", conf)
		}
	}
	if opts.NodeClient == nil {
		return false, fmt.Errorf("没有配置 Kubernetes 客户端，无法判断节点 %s 是否 Ready", opts.Node)
	}
	client, err := opts.NodeClient(ctx)
	if err != nil {
		return false, err
	}
	if client == nil {
		return false, fmt.Errorf("无法获取 Kubernetes 客户端，无法判断节点 %s 是否 Ready", opts.Node)
	}
	ready, err := client.NodeReady(ctx, opts.Node)
	if err != nil {
		return false, err
	}
	if !ready {
		return false, healthDiagnostic(ctx, opts)
	}
	return true, nil
}

// healthDiagnostic 探测 kubelet 健康端点，并把失败转换为面向运维人员的消息
// （端点有应答或没有失败时返回 nil）。
func healthDiagnostic(ctx context.Context, opts WaitOptions) error {
	if opts.Health == nil {
		return nil
	}
	h := KubeletHealthCheck()
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := opts.Health.Check(reqCtx, h); err != nil {
		return fmt.Errorf("kubelet 健康端点 %s 不可用: %w", h.URL(), err)
	}
	return nil
}

// bootstrapTimeoutError 构造面向运维人员的超时错误。
func bootstrapTimeoutError(ctx context.Context, opts WaitOptions, last error) error {
	h := KubeletHealthCheck()
	var b strings.Builder
	fmt.Fprintf(&b, "kubelet 未就绪：节点 %s 在 %s 内未上报 Ready（健康端点 %s）", opts.Node, opts.Timeout, h.URL())
	if last != nil {
		fmt.Fprintf(&b, "；最后一次检查：%v", last)
	}
	// readyOnce 已经在“未 Ready”时附带过健康探测结果，避免重复探测。
	if last == nil || !strings.Contains(last.Error(), h.URL()) {
		if diag := healthDiagnostic(ctx, opts); diag != nil {
			fmt.Fprintf(&b, "；%v", diag)
		}
	}
	b.WriteString("。请确认该节点已 approved 的引导令牌（kubectl get csr）、Node 对象已注册且 kubelet 版本与 BOM 一致，并用 `journalctl -u kubelet` 查看日志")
	return errors.New(b.String())
}
