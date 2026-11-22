// Package controlplane 把控制面组件作为 systemd unit 落地
// 上层 phase 只与 Runner 打交道：
//
//	Apply → 写 unit、daemon-reload、重启并等待健康（失败自动回滚）
//	Restart → 强制重启 unit
//	Stop → 停掉组件但保留文件
//	Remove → 彻底移除
//	Health → 探活
//
// internal/render 负责“应该是什么”，本包负责“怎么让它变成那样”。
package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/execx"
)

// Component 标识一个控制面组件。
type Component string

// 控制面组件。
const (
	ComponentEtcd              Component = constants.ComponentEtcd
	ComponentAPIServer         Component = constants.ComponentAPIServer
	ComponentControllerManager Component = constants.ComponentControllerManager
	ComponentScheduler         Component = constants.ComponentScheduler
	ComponentKubelet           Component = constants.ComponentKubelet
)

// RenderedSpec 是一个组件期望的落盘状态。
type RenderedSpec struct {
	// Component 是该 spec 所属的组件。
	Component Component
	// Unit 是 systemd unit 文件内容。
	Unit []byte
	// Health 描述如何验证组件已经起来。
	Health HealthCheck
}

// HealthCheck 描述一个组件的健康检查端点。
type HealthCheck struct {
	// Scheme 是 http 或 https。
	Scheme string
	// Address 是 host:port。
	Address string
	// Path 是端点路径，例如 /readyz。
	Path string
	// ExpectStatus 列出可接受的 HTTP 状态码；为空表示只接受 200。
	// kcm/scheduler 的 /healthz 在未授权访问时返回 401/403，但进程已经起来了，
	// 因此这两个组件把 401/403 也视为“已就绪”。
	ExpectStatus []int
	// Timeout 是单次请求超时。
	Timeout time.Duration
}

// URL 渲染健康检查 URL。
func (h HealthCheck) URL() string {
	scheme := h.Scheme
	if scheme == "" {
		scheme = "https"
	}
	path := h.Path
	if path == "" {
		path = "/healthz"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("%s://%s%s", scheme, h.Address, path)
}

// Accepts 表示某个 HTTP 状态码是否算就绪。
func (h HealthCheck) Accepts(status int) bool {
	if len(h.ExpectStatus) == 0 {
		return status == 200
	}
	return slices.Contains(h.ExpectStatus, status)
}

// HealthChecker 执行 HTTP 探活。
type HealthChecker interface {
	// Check 在组件就绪时返回 nil。
	Check(ctx context.Context, h HealthCheck) error
}

// Runner 应用渲染好的控制面 spec。
type Runner interface {
	// Apply 写入组件文件并等待其健康。
	Apply(ctx context.Context, spec *RenderedSpec) error
	// Restart 强制重启并等待其重新健康。
	Restart(ctx context.Context, component Component) error
	// Stop 停止组件但不删除其文件。
	Stop(ctx context.Context, component Component) error
	// Remove 删除组件文件。
	Remove(ctx context.Context, component Component) error
	// Health 探活一个组件。
	Health(ctx context.Context, component Component) error
}

// Options 是 Runner 的依赖。
type Options struct {
	// Paths 解析所有目标路径。
	Paths constants.Paths
	// Health 执行探活。
	Health HealthChecker
	// Timeout 限制“等待到健康”的时间。
	Timeout time.Duration
	// PollInterval 是健康轮询间隔。
	PollInterval time.Duration
	// Sleep 可注入以便测试。
	Sleep func(time.Duration)
	// Report 接收进度消息。
	Report func(format string, args ...any)
}

// Defaults 补齐未设置的选项。
func (o *Options) Defaults() {
	if o.Timeout <= 0 {
		o.Timeout = 90 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Report == nil {
		o.Report = func(string, ...any) {}
	}
}

// CommandRunner 执行主机命令（systemctl / ctr）。
type CommandRunner = execx.Runner

// waitHealthy 轮询健康端点，直到就绪或超时。
func waitHealthy(ctx context.Context, opts Options, component Component, h HealthCheck) error {
	if opts.Health == nil {
		return nil
	}
	deadline := time.Now().Add(opts.Timeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = opts.Health.Check(ctx, h)
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s 在 %s 内未就绪: %w", component, opts.Timeout, lastErr)
		}
		opts.Sleep(opts.PollInterval)
	}
}

// specHealthOr 返回组件的健康检查，没有则返回默认值。
func specHealthOr(spec *RenderedSpec) HealthCheck {
	if spec.Health.Address != "" {
		return spec.Health
	}
	return defaultHealth(spec.Component)
}

// defaultHealth 返回组件文档中记录的健康端点
func defaultHealth(c Component) HealthCheck {
	switch c {
	case ComponentAPIServer:
		return HealthCheck{Scheme: "https", Address: fmt.Sprintf("127.0.0.1:%d", constants.APIServerPort), Path: "/readyz"}
	case ComponentControllerManager:
		return HealthCheck{
			Scheme: "https", Address: fmt.Sprintf("127.0.0.1:%d", constants.ControllerManagerPort), Path: "/healthz",
			// 未授权访问返回 401/403 时进程已经可用。
			ExpectStatus: []int{200, 401, 403},
		}
	case ComponentScheduler:
		return HealthCheck{
			Scheme: "https", Address: fmt.Sprintf("127.0.0.1:%d", constants.SchedulerPort), Path: "/healthz",
			ExpectStatus: []int{200, 401, 403},
		}
	case ComponentEtcd:
		return HealthCheck{Scheme: "http", Address: fmt.Sprintf("127.0.0.1:%d", constants.EtcdMetricsPort), Path: "/health"}
	case ComponentKubelet:
		return HealthCheck{Scheme: "http", Address: fmt.Sprintf("127.0.0.1:%d", constants.KubeletHealthzPort), Path: "/healthz"}
	default:
		return HealthCheck{}
	}
}

// ComponentHealth 返回组件的健康探针。
func ComponentHealth(c Component) HealthCheck { return defaultHealth(c) }
