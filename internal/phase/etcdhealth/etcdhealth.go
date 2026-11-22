// Package etcdhealth 实现 `etcd-health` phase：master 启动前检查外部 etcd 集群。
// 对 external，etcd 由用户预先部署；master 必须在启动
// apiserver 前确认 endpoints 健康、成员表可读，否则直接退出，避免 apiserver
// 进入 crash-loop。
package etcdhealth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"kubemono/internal/etcd"
	"kubemono/internal/phase"
)

// PhaseName 是 `etcd-health` phase 的名称。
const PhaseName = "etcd-health"

func init() { phase.MustRegister(&Phase{}) }

// Phase 检查外部 etcd 集群健康。
type Phase struct{}

// DefaultTimeout 是健康检查的默认超时。
const DefaultTimeout = 30 * time.Second

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 实现 phase.Phase：只对 external etcd 的 master 生效。
func (p *Phase) Applies(c *phase.Context) bool {
	return c.Cluster != nil && c.IsMaster() && c.Cluster.ControlPlane.Etcd.IsExternal()
}

// Check 报告 etcd 是否已经健康。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	if err := p.check(ctx, c); err != nil {
		c.Reporter().Debugf("etcd-health 尚未通过：%v", err)
		return false, nil
	}
	return true, nil
}

// Run 检查 etcd 健康，不健康即返回错误。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	return p.check(ctx, c)
}

func (p *Phase) check(ctx context.Context, c *phase.Context) error {
	if c.Cluster == nil {
		return fmt.Errorf("etcd-health: 缺少集群配置")
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	client, err := p.client(c)
	if err != nil {
		return fmt.Errorf("外部 etcd 健康检查无法建立客户端（确认 external.caFile/certFile/keyFile 与 endpoints）：%w", err)
	}
	defer func() { _ = client.Close() }()
	healths, err := client.Health(ctx)
	if err != nil {
		return fmt.Errorf("外部 etcd 不健康：读取成员/状态失败: %w", err)
	}
	if len(healths) == 0 {
		return fmt.Errorf("外部 etcd 不健康：没有可检查的 endpoint")
	}
	var bad []string
	for _, h := range healths {
		if !h.Healthy {
			msg := h.Endpoint
			if h.Error != "" {
				msg += ": " + h.Error
			}
			bad = append(bad, msg)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("外部 etcd 不健康，拒绝启动控制面：%s", strings.Join(bad, "; "))
	}
	c.Reporter().Infof("外部 etcd 健康：%d 个 endpoint 全部通过", len(healths))
	return nil
}

func (p *Phase) client(c *phase.Context) (etcd.API, error) {
	host, _ := c.Self()
	prov, err := etcd.NewProvider(c.Cluster, host, c.Paths, c.NodeName())
	if err != nil {
		return nil, err
	}
	cfg, err := prov.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Report = c.Reporter()
	return etcd.New(cfg)
}
